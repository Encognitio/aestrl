package engine

import (
	"bufio"
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/Encognitio/aestrl/pkg/engine/model"
	"github.com/trengrj/go-potion"
)

var (
	potionOnce    sync.Once
	potionInst    *potion.Potion
	potionInitErr error
)

func ensureEmbeddedModel(modelKind potion.Model) error {
	cacheDir := os.Getenv("GO_POTION_HOME")
	if cacheDir == "" {
		userCache, err := os.UserCacheDir()
		if err != nil {
			home, hErr := os.UserHomeDir()
			if hErr != nil {
				cacheDir = filepath.Join(os.TempDir(), "go-potion")
			} else {
				cacheDir = filepath.Join(home, ".cache", "go-potion")
			}
		} else {
			cacheDir = filepath.Join(userCache, "go-potion")
		}
	}

	modelDir := filepath.Join(cacheDir, string(modelKind))
	if err := os.MkdirAll(modelDir, 0755); err != nil {
		return err
	}

	stPath := filepath.Join(modelDir, "model.safetensors")
	if fi, err := os.Stat(stPath); err != nil || fi.Size() == 0 {
		if len(model.SafeTensors) > 0 {
			if err := os.WriteFile(stPath, model.SafeTensors, 0644); err != nil {
				return err
			}
		}
	}

	tokPath := filepath.Join(modelDir, "tokenizer.json")
	if fi, err := os.Stat(tokPath); err != nil || fi.Size() == 0 {
		if len(model.TokenizerJSON) > 0 {
			if err := os.WriteFile(tokPath, model.TokenizerJSON, 0644); err != nil {
				return err
			}
		}
	}

	return nil
}

func getPotionEncoder() *potion.Potion {
	potionOnce.Do(func() {
		_ = ensureEmbeddedModel(potion.BASE8M)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		potionInst, potionInitErr = potion.New(ctx, potion.BASE8M)
	})
	return potionInst
}

// dotProduct computes cosine similarity for L2-normalized embeddings.
func dotProduct(a, b []float32) float64 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 0
	}
	var sum float64
	for i := range a {
		sum += float64(a[i] * b[i])
	}
	return sum
}

// SearchMatch represents a content search hit with line context.
type SearchMatch struct {
	Path          string   `json:"path"`
	LineNumber    int      `json:"line_number"`
	LineText      string   `json:"line_text"`
	ContextBefore []string `json:"context_before,omitempty"`
	ContextAfter  []string `json:"context_after,omitempty"`
}

// SearchResult represents a hybrid ranked search excerpt.
type SearchResult struct {
	Path       string  `json:"path"`
	LineNumber int     `json:"line_number"`
	Score      float64 `json:"score"`
	Kind       string  `json:"kind"` // "symbol", "path", "content", "semantic"
	Excerpt    string  `json:"excerpt"`
}

// ContextResult packages high-relevance evidence for a question.
type ContextResult struct {
	Query     string         `json:"query"`
	Budget    int            `json:"budget"`
	UsedChars int            `json:"used_chars"`
	Excerpts  []SearchResult `json:"excerpts"`
	Outline   []string       `json:"outline,omitempty"`
	Summary   string         `json:"summary"`
}

// Document represents an indexed file for BM25 and lexical search.
type Document struct {
	Path      string
	Terms     map[string]int // term -> frequency
	TermCount int
	Lines     []string
	Symbols   []string
	Vector    []float32 // CPU static embedding vector
}

// HybridIndex maintains an in-memory index for fast BM25 + symbol search.
type HybridIndex struct {
	eng       *Engine
	docs      map[string]*Document // path -> Document
	docFreq   map[string]int       // term -> doc count
	avgDocLen float64
	totalDocs int
	mu        sync.RWMutex
}

// NewHybridIndex creates a new hybrid index attached to an engine.
func NewHybridIndex(eng *Engine) *HybridIndex {
	return &HybridIndex{
		eng:     eng,
		docs:    make(map[string]*Document),
		docFreq: make(map[string]int),
	}
}

// BuildIndex scans files in the root and builds the index.
func (idx *HybridIndex) BuildIndex() error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	idx.docs = make(map[string]*Document)
	idx.docFreq = make(map[string]int)

	totalLength := 0

	_ = filepath.Walk(idx.eng.realRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(idx.eng.realRoot, path)
		if err != nil {
			return nil
		}
		normRel := filepath.ToSlash(rel)
		if idx.eng.isExcluded(info.Name(), normRel) {
			return nil
		}
		if info.Size() > idx.eng.maxReadBytes || isFileBinary(path, info.Size()) {
			return nil
		}

		doc := idx.indexFile(path, normRel)
		if doc != nil {
			if doc.Vector == nil {
				doc.Vector = idx.encodeVector(doc)
			}
			idx.docs[normRel] = doc
			totalLength += doc.TermCount
			for term := range doc.Terms {
				idx.docFreq[term]++
			}
		}
		return nil
	})

	idx.totalDocs = len(idx.docs)
	if idx.totalDocs > 0 {
		idx.avgDocLen = float64(totalLength) / float64(idx.totalDocs)
	}
	return nil
}

// encodeVector encodes document text using CPU static embeddings.
func (idx *HybridIndex) encodeVector(doc *Document) []float32 {
	enc := getPotionEncoder()
	if enc == nil {
		return nil
	}
	var sb strings.Builder
	for _, sym := range doc.Symbols {
		sb.WriteString(sym)
		sb.WriteString(" ")
	}
	for i, l := range doc.Lines {
		if i > 20 || sb.Len() > 400 {
			break
		}
		sb.WriteString(l)
		sb.WriteString(" ")
	}
	text := strings.TrimSpace(sb.String())
	if text == "" {
		text = filepath.Base(doc.Path)
	}
	vec, err := enc.Encode(text)
	if err != nil {
		return nil
	}
	return vec
}

func (idx *HybridIndex) indexFile(realPath, normRel string) *Document {
	f, err := os.Open(realPath)
	if err != nil {
		return nil
	}
	defer f.Close()

	var lines []string
	terms := make(map[string]int)
	termCount := 0

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		lines = append(lines, line)
		lineTerms := tokenize(line)
		for _, t := range lineTerms {
			terms[t]++
			termCount++
		}
	}

	// Also index filename parts
	pathTerms := tokenize(normRel)
	for _, t := range pathTerms {
		terms[t] += 2 // weight boost for path
		termCount += 2
	}

	// Extract basic symbols
	symbols := extractSymbols(lines)

	return &Document{
		Path:      normRel,
		Terms:     terms,
		TermCount: termCount,
		Lines:     lines,
		Symbols:   symbols,
	}
}

// tokenize splits text into search tokens, handling identifiers (camelCase, snake_case).
func tokenize(text string) []string {
	var tokens []string
	var current strings.Builder

	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if unicode.IsUpper(r) && current.Len() > 0 {
				tokens = append(tokens, strings.ToLower(current.String()))
				current.Reset()
			}
			current.WriteRune(r)
		} else {
			if current.Len() > 0 {
				tokens = append(tokens, strings.ToLower(current.String()))
				current.Reset()
			}
		}
	}
	if current.Len() > 0 {
		tokens = append(tokens, strings.ToLower(current.String()))
	}
	return tokens
}

// Search performs hybrid BM25 + semantic embeddings + symbol ranked search.
func (e *Engine) Search(query, relPath string, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 50
	}

	// If index is empty, build it on the fly
	if e.index.totalDocs == 0 {
		_ = e.index.BuildIndex()
	}

	queryTokens := tokenize(query)

	enc := getPotionEncoder()
	var qVec []float32
	if enc != nil {
		qVec, _ = enc.Encode(query)
	}

	if len(queryTokens) == 0 && len(qVec) == 0 {
		return []SearchResult{}, nil
	}

	e.index.mu.RLock()
	defer e.index.mu.RUnlock()

	k1 := 1.2
	b := 0.75

	var results []SearchResult

	for docPath, doc := range e.index.docs {
		if relPath != "" && !strings.HasPrefix(docPath, relPath) {
			continue
		}

		bm25Score := 0.0
		hasMatch := false

		for _, q := range queryTokens {
			tf := float64(doc.Terms[q])
			if tf > 0 {
				hasMatch = true
				df := float64(e.index.docFreq[q])
				idf := math.Log(1.0 + (float64(e.index.totalDocs)-df+0.5)/(df+0.5))
				docLenRatio := float64(doc.TermCount) / e.index.avgDocLen
				termScore := idf * ((tf * (k1 + 1.0)) / (tf + k1*(1.0-b+b*docLenRatio)))
				bm25Score += termScore
			}
		}

		cosSim := 0.0
		if len(qVec) > 0 && len(doc.Vector) > 0 {
			cosSim = dotProduct(qVec, doc.Vector)
		}

		// Require either lexical match or decent semantic similarity
		if !hasMatch && cosSim < 0.45 {
			continue
		}

		score := bm25Score
		if cosSim > 0.40 {
			score += (cosSim * 3.5)
		}

		// Check symbol matches for bonus score
		symbolMatch := false
		lowerQ := strings.ToLower(query)
		for _, sym := range doc.Symbols {
			if strings.Contains(strings.ToLower(sym), lowerQ) {
				score *= 2.5
				symbolMatch = true
				break
			}
		}

		// Find best matching line for excerpt
		bestLineIdx := 0
		bestLineScore := 0
		for idx, line := range doc.Lines {
			lowerLine := strings.ToLower(line)
			lineScore := 0
			for _, q := range queryTokens {
				if strings.Contains(lowerLine, q) {
					lineScore++
				}
			}
			if lineScore > bestLineScore {
				bestLineScore = lineScore
				bestLineIdx = idx
			}
		}

		kind := "content"
		if symbolMatch {
			kind = "symbol"
		} else if strings.Contains(strings.ToLower(docPath), lowerQ) {
			kind = "path"
		} else if cosSim > 0.60 && !hasMatch {
			kind = "semantic"
		}

		excerpt := ""
		if bestLineIdx < len(doc.Lines) {
			excerpt = strings.TrimSpace(doc.Lines[bestLineIdx])
		}

		results = append(results, SearchResult{
			Path:       docPath,
			LineNumber: bestLineIdx + 1,
			Score:      score,
			Kind:       kind,
			Excerpt:    excerpt,
		})
	}

	// Sort descending by score
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

// Grep scans files line-by-line using literal or regex matching.
func (e *Engine) Grep(query, relPath string, isRegex bool, contextLines, limit int) ([]SearchMatch, error) {
	if limit <= 0 {
		limit = DefaultMaxSearchHits
	}
	realPath, _, err := e.resolvePath(relPath)
	if err != nil {
		return nil, err
	}

	var reg *regexp.Regexp
	if isRegex {
		var err error
		reg, err = regexp.Compile(query)
		if err != nil {
			return nil, fmt.Errorf("invalid regex: %w", err)
		}
	} else {
		query = strings.ToLower(query)
	}

	var matches []SearchMatch

	_ = filepath.Walk(realPath, func(path string, info os.FileInfo, err error) error {
		if err != nil || len(matches) >= limit || info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(e.realRoot, path)
		if err != nil {
			return nil
		}
		normRel := filepath.ToSlash(rel)
		if e.isExcluded(info.Name(), normRel) {
			return nil
		}
		if info.Size() > e.maxReadBytes || isFileBinary(path, info.Size()) {
			return nil
		}

		fileMatches := e.searchInFile(path, normRel, query, reg, isRegex, contextLines, limit-len(matches))
		matches = append(matches, fileMatches...)
		return nil
	})

	return matches, nil
}

func (e *Engine) searchInFile(filePath, relPath, query string, reg *regexp.Regexp, isRegex bool, contextLines, maxRemaining int) []SearchMatch {
	f, err := os.Open(filePath)
	if err != nil {
		return nil
	}
	defer f.Close()

	var matches []SearchMatch
	var window []string
	scanner := bufio.NewScanner(f)
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := scanner.Text()

		matched := false
		if isRegex {
			matched = reg.MatchString(line)
		} else {
			matched = strings.Contains(strings.ToLower(line), query)
		}

		if matched {
			var before []string
			if contextLines > 0 && len(window) > 0 {
				start := len(window) - contextLines
				if start < 0 {
					start = 0
				}
				before = append([]string{}, window[start:]...)
			}

			matches = append(matches, SearchMatch{
				Path:          relPath,
				LineNumber:    lineNum,
				LineText:      line,
				ContextBefore: before,
			})
			if len(matches) >= maxRemaining {
				break
			}
		}

		if contextLines > 0 {
			window = append(window, line)
			if len(window) > contextLines {
				window = window[1:]
			}
		}
	}
	return matches
}

// Context returns ranked evidence and structural context for a natural language question.
func (e *Engine) Context(query, relPath string, budget int) (*ContextResult, error) {
	if budget <= 0 {
		budget = 4000
	}

	searchHits, err := e.Search(query, relPath, 20)
	if err != nil {
		return nil, err
	}

	var usedChars int
	var selected []SearchResult

	for _, hit := range searchHits {
		lineCost := len(hit.Excerpt) + len(hit.Path) + 20
		if usedChars+lineCost > budget {
			break
		}
		selected = append(selected, hit)
		usedChars += lineCost
	}

	// Also fetch high level outline if budget allows
	var outlineLines []string
	if usedChars < budget-200 {
		if out, err := e.Outline(relPath); err == nil {
			for _, item := range out.Items {
				line := fmt.Sprintf("%s:%d [%s] %s", item.Path, item.LineNumber, item.Kind, item.Name)
				if usedChars+len(line)+1 > budget {
					break
				}
				outlineLines = append(outlineLines, line)
				usedChars += len(line) + 1
			}
		}
	}

	summary := fmt.Sprintf("Found %d relevant excerpts across shared files for '%s'.", len(selected), query)

	return &ContextResult{
		Query:     query,
		Budget:    budget,
		UsedChars: usedChars,
		Excerpts:  selected,
		Outline:   outlineLines,
		Summary:   summary,
	}, nil
}
