package engine

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

var (
	ErrAccessDenied     = errors.New("access denied: path outside sharing root or excluded")
	ErrPathNotFound     = errors.New("path not found")
	ErrNotADirectory    = errors.New("path is not a directory")
	ErrIsDirectory      = errors.New("path is a directory")
	ErrFileTooLarge     = errors.New("file exceeds maximum read size")
	ErrBinaryFile       = errors.New("file is binary")
	ErrSearchTimeout    = errors.New("search timed out")
	ErrInvalidLineRange = errors.New("invalid line range")
)

const (
	DefaultMaxReadBytes   = 10 * 1024 * 1024 // 10 MB
	DefaultMaxSearchHits  = 200
	DefaultMaxTreeEntries = 2000
	DefaultSearchTimeout  = 10 * time.Second
)

// FileItem represents a filesystem entry with metadata.
type FileItem struct {
	Name      string    `json:"name"`
	Path      string    `json:"path"` // relative path using '/'
	IsDir     bool      `json:"is_dir"`
	Size      int64     `json:"size"`
	ModTime   time.Time `json:"mod_time"`
	MimeType  string    `json:"mime_type,omitempty"`
	IsBinary  bool      `json:"is_binary"`
	LineCount int       `json:"line_count,omitempty"`
}

// ReadFileResult represents text content returned by read/cat.
type ReadFileResult struct {
	Path       string `json:"path"`
	TotalLines int    `json:"total_lines"`
	StartLine  int    `json:"start_line"`
	EndLine    int    `json:"end_line"`
	Content    string `json:"content"`
	IsBinary   bool   `json:"is_binary"`
	Size       int64  `json:"size"`
	MimeType   string `json:"mime_type"`
}

// RawFileResult represents raw binary/text data with byte offsets.
type RawFileResult struct {
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Offset   int64  `json:"offset"`
	Length   int64  `json:"length"`
	MimeType string `json:"mime_type"`
	Data     []byte `json:"data,omitempty"`
}

// TreeNode represents a recursive directory tree node.
type TreeNode struct {
	Name     string      `json:"name"`
	Path     string      `json:"path"`
	IsDir    bool        `json:"is_dir"`
	Size     int64       `json:"size,omitempty"`
	Children []*TreeNode `json:"children,omitempty"`
}

// CompareDiffLine represents a line in a file comparison.
type CompareDiffLine struct {
	Type    string `json:"type"` // "+", "-", " "
	LineNum int    `json:"line_num,omitempty"`
	Text    string `json:"text"`
}

// CompareResult represents a line-by-line diff between two files.
type CompareResult struct {
	Path1 string            `json:"path1"`
	Path2 string            `json:"path2"`
	Diff  []CompareDiffLine `json:"diff"`
}

// ManifestItem represents an entry in the snapshot inventory.
type ManifestItem struct {
	Path     string    `json:"path"`
	Size     int64     `json:"size"`
	ModTime  time.Time `json:"mod_time"`
	SHA256   string    `json:"sha256,omitempty"`
	MimeType string    `json:"mime_type"`
	IsBinary bool      `json:"is_binary"`
}

// ManifestResult represents complete snapshot inventory.
type ManifestResult struct {
	RootPath   string         `json:"root_path"`
	TotalFiles int            `json:"total_files"`
	TotalBytes int64          `json:"total_bytes"`
	CreatedAt  time.Time      `json:"created_at"`
	Files      []ManifestItem `json:"files"`
}

// Engine performs safe, read-only file operations rooted at a directory.
type Engine struct {
	rootPath     string
	realRoot     string
	exclusions   []string
	maxReadBytes int64
	index        *HybridIndex
	mu           sync.RWMutex
}

// New creates a new safe read-only inspection engine for the target root directory.
// Users control what they share; no automatic exclusions are imposed.
func New(rootDirectory string, customExclusions []string, maxReadBytes int64) (*Engine, error) {
	absPath, err := filepath.Abs(rootDirectory)
	if err != nil {
		return nil, fmt.Errorf("invalid root directory path: %w", err)
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return nil, fmt.Errorf("root directory error: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("root path '%s' is not a directory", absPath)
	}

	realRoot, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return nil, fmt.Errorf("failed to evaluate root symlinks: %w", err)
	}

	if maxReadBytes <= 0 {
		maxReadBytes = DefaultMaxReadBytes
	}

	eng := &Engine{
		rootPath:     absPath,
		realRoot:     realRoot,
		exclusions:   customExclusions,
		maxReadBytes: maxReadBytes,
	}
	eng.index = NewHybridIndex(eng)
	return eng, nil
}

// RootPath returns the absolute sharing root directory.
func (e *Engine) RootPath() string {
	return e.rootPath
}

// Index returns the engine's hybrid index.
func (e *Engine) Index() *HybridIndex {
	return e.index
}

// isExcluded checks if a name or relative path matches custom exclusions.
func (e *Engine) isExcluded(name, relPath string) bool {
	if len(e.exclusions) == 0 {
		return false
	}
	for _, pattern := range e.exclusions {
		if matched, _ := filepath.Match(pattern, name); matched {
			return true
		}
		parts := strings.Split(relPath, string(filepath.Separator))
		for _, part := range parts {
			if matched, _ := filepath.Match(pattern, part); matched {
				return true
			}
		}
	}
	return false
}

// resolvePath checks traversal and symlink escapes, returning the validated physical path.
func (e *Engine) resolvePath(rel string) (string, string, error) {
	cleanRel := filepath.Clean(strings.ReplaceAll(rel, "/", string(filepath.Separator)))
	if cleanRel == "." || cleanRel == "" {
		cleanRel = ""
	}

	if strings.HasPrefix(cleanRel, "..") || filepath.IsAbs(cleanRel) {
		return "", "", ErrAccessDenied
	}

	target := filepath.Join(e.realRoot, cleanRel)
	if cleanRel != "" {
		baseName := filepath.Base(cleanRel)
		if e.isExcluded(baseName, cleanRel) {
			return "", "", ErrAccessDenied
		}
	}

	info, err := os.Stat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", ErrPathNotFound
		}
		return "", "", err
	}

	realTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", "", fmt.Errorf("failed to resolve symlink: %w", err)
	}

	relToRoot, err := filepath.Rel(e.realRoot, realTarget)
	if err != nil || strings.HasPrefix(relToRoot, "..") {
		return "", "", ErrAccessDenied
	}

	normalizedRel := filepath.ToSlash(cleanRel)
	_ = info
	return realTarget, normalizedRel, nil
}

// Execute is the universal transport-independent operation dispatcher.
func (e *Engine) Execute(op string, args map[string]interface{}) (interface{}, error) {
	if args == nil {
		args = make(map[string]interface{})
	}

	switch strings.ToLower(op) {
	case "ls", "list_directory":
		path, _ := args["path"].(string)
		return e.ListDirectory(path)

	case "tree":
		path, _ := args["path"].(string)
		depth := 3
		if d, ok := args["depth"].(float64); ok {
			depth = int(d)
		} else if d, ok := args["depth"].(int); ok {
			depth = d
		}
		return e.Tree(path, depth)

	case "stat", "file_info":
		path, _ := args["path"].(string)
		return e.FileInfo(path)

	case "find", "search_files":
		pattern, _ := args["query"].(string)
		if pattern == "" {
			pattern, _ = args["pattern"].(string)
		}
		path, _ := args["path"].(string)
		return e.Find(pattern, path)

	case "search":
		query, _ := args["query"].(string)
		path, _ := args["path"].(string)
		limit := 50
		if l, ok := args["limit"].(float64); ok {
			limit = int(l)
		} else if l, ok := args["limit"].(int); ok {
			limit = l
		}
		return e.Search(query, path, limit)

	case "grep", "rg", "search_content":
		query, _ := args["query"].(string)
		path, _ := args["path"].(string)
		isRegex, _ := args["is_regex"].(bool)
		ctxLines := 2
		if c, ok := args["context_lines"].(float64); ok {
			ctxLines = int(c)
		} else if c, ok := args["context_lines"].(int); ok {
			ctxLines = c
		}
		limit := 100
		if l, ok := args["limit"].(float64); ok {
			limit = int(l)
		} else if l, ok := args["limit"].(int); ok {
			limit = l
		}
		return e.Grep(query, path, isRegex, ctxLines, limit)

	case "context":
		query, _ := args["query"].(string)
		path, _ := args["path"].(string)
		budget := 4000
		if b, ok := args["budget"].(float64); ok {
			budget = int(b)
		} else if b, ok := args["budget"].(int); ok {
			budget = b
		}
		return e.Context(query, path, budget)

	case "read", "cat", "read_file":
		path, _ := args["path"].(string)
		start := 1
		end := 0
		if s, ok := args["start_line"].(float64); ok {
			start = int(s)
		} else if s, ok := args["start_line"].(int); ok {
			start = s
		}
		if ed, ok := args["end_line"].(float64); ok {
			end = int(ed)
		} else if ed, ok := args["end_line"].(int); ok {
			end = ed
		}
		return e.ReadFile(path, start, end)

	case "outline":
		path, _ := args["path"].(string)
		return e.Outline(path)

	case "raw", "get", "get_file":
		path, _ := args["path"].(string)
		var offset int64 = 0
		var length int64 = -1
		if off, ok := args["offset"].(float64); ok {
			offset = int64(off)
		} else if off, ok := args["offset"].(int64); ok {
			offset = off
		} else if off, ok := args["offset"].(int); ok {
			offset = int64(off)
		}

		if l, ok := args["length"].(float64); ok {
			length = int64(l)
		} else if l, ok := args["length"].(int64); ok {
			length = l
		} else if l, ok := args["length"].(int); ok {
			length = int64(l)
		}
		return e.GetRawFile(path, offset, length)

	case "symbol":
		name, _ := args["name"].(string)
		if name == "" {
			name, _ = args["query"].(string)
		}
		path, _ := args["path"].(string)
		return e.Symbol(name, path)

	case "refs":
		symbol, _ := args["symbol"].(string)
		if symbol == "" {
			symbol, _ = args["query"].(string)
		}
		path, _ := args["path"].(string)
		return e.Refs(symbol, path)

	case "deps":
		path, _ := args["path"].(string)
		return e.Deps(path)

	case "compare":
		p1, _ := args["path1"].(string)
		p2, _ := args["path2"].(string)
		return e.Compare(p1, p2)

	case "manifest":
		path, _ := args["path"].(string)
		return e.Manifest(path)

	default:
		return nil, fmt.Errorf("unknown operation: %s", op)
	}
}

// ListDirectory lists files and folders within a relative path.
func (e *Engine) ListDirectory(relPath string) ([]FileItem, error) {
	realPath, normalizedRel, err := e.resolvePath(relPath)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(realPath)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, ErrNotADirectory
	}

	entries, err := os.ReadDir(realPath)
	if err != nil {
		return nil, err
	}

	var items []FileItem
	for _, entry := range entries {
		name := entry.Name()
		childRel := filepath.ToSlash(filepath.Join(normalizedRel, name))
		if e.isExcluded(name, childRel) {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		isBinary := false
		mimeType := ""
		if !entry.IsDir() {
			isBinary = isFileBinary(filepath.Join(realPath, name), info.Size())
			mimeType = detectMime(name, filepath.Join(realPath, name))
		}

		items = append(items, FileItem{
			Name:     name,
			Path:     childRel,
			IsDir:    entry.IsDir(),
			Size:     info.Size(),
			ModTime:  info.ModTime(),
			MimeType: mimeType,
			IsBinary: isBinary,
		})
	}
	return items, nil
}

// Tree generates a visual/structured directory tree up to maxDepth.
func (e *Engine) Tree(relPath string, maxDepth int) (*TreeNode, error) {
	if maxDepth <= 0 {
		maxDepth = 3
	}
	realPath, normalizedRel, err := e.resolvePath(relPath)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(realPath)
	if err != nil {
		return nil, err
	}

	count := 0
	return e.buildTree(realPath, normalizedRel, info.Name(), info.IsDir(), 0, maxDepth, &count, DefaultMaxTreeEntries)
}

func (e *Engine) buildTree(realPath, relPath, name string, isDir bool, currentDepth, maxDepth int, count *int, maxEntries int) (*TreeNode, error) {
	if *count >= maxEntries {
		return nil, nil
	}
	*count++

	node := &TreeNode{
		Name:  name,
		Path:  relPath,
		IsDir: isDir,
	}

	if !isDir {
		if fi, err := os.Stat(realPath); err == nil {
			node.Size = fi.Size()
		}
		return node, nil
	}

	if currentDepth >= maxDepth {
		return node, nil
	}

	entries, err := os.ReadDir(realPath)
	if err != nil {
		return node, nil
	}

	for _, entry := range entries {
		if *count >= maxEntries {
			break
		}
		cName := entry.Name()
		childRel := filepath.ToSlash(filepath.Join(relPath, cName))
		if e.isExcluded(cName, childRel) {
			continue
		}

		childReal := filepath.Join(realPath, cName)
		childNode, err := e.buildTree(childReal, childRel, cName, entry.IsDir(), currentDepth+1, maxDepth, count, maxEntries)
		if err == nil && childNode != nil {
			node.Children = append(node.Children, childNode)
		}
	}
	return node, nil
}

// FileInfo returns detailed metadata for a file or directory.
func (e *Engine) FileInfo(relPath string) (*FileItem, error) {
	realPath, normalizedRel, err := e.resolvePath(relPath)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(realPath)
	if err != nil {
		return nil, err
	}

	isBinary := false
	mimeType := ""
	lineCount := 0
	if !info.IsDir() {
		isBinary = isFileBinary(realPath, info.Size())
		mimeType = detectMime(info.Name(), realPath)
		if !isBinary && info.Size() <= e.maxReadBytes {
			lineCount = countLines(realPath)
		}
	}

	return &FileItem{
		Name:      info.Name(),
		Path:      normalizedRel,
		IsDir:     info.IsDir(),
		Size:      info.Size(),
		ModTime:   info.ModTime(),
		MimeType:  mimeType,
		IsBinary:  isBinary,
		LineCount: lineCount,
	}, nil
}

// Find matches filenames, paths, and glob patterns recursively.
func (e *Engine) Find(pattern, relPath string) ([]FileItem, error) {
	realPath, normalizedRel, err := e.resolvePath(relPath)
	if err != nil {
		return nil, err
	}

	lowerPat := strings.ToLower(pattern)
	var matches []FileItem

	_ = filepath.Walk(realPath, func(path string, info os.FileInfo, err error) error {
		if err != nil || len(matches) >= DefaultMaxSearchHits {
			return nil
		}
		rel, err := filepath.Rel(e.realRoot, path)
		if err != nil {
			return nil
		}
		normRel := filepath.ToSlash(rel)
		name := info.Name()

		if e.isExcluded(name, normRel) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		matched := false
		if pattern == "" || pattern == "*" {
			matched = true
		} else {
			if m, _ := filepath.Match(lowerPat, strings.ToLower(name)); m {
				matched = true
			} else if strings.Contains(strings.ToLower(name), lowerPat) {
				matched = true
			} else if strings.Contains(strings.ToLower(normRel), lowerPat) {
				matched = true
			}
		}

		if matched && normRel != normalizedRel {
			isBin := false
			mime := ""
			if !info.IsDir() {
				isBin = isFileBinary(path, info.Size())
				mime = detectMime(name, path)
			}
			matches = append(matches, FileItem{
				Name:     name,
				Path:     normRel,
				IsDir:    info.IsDir(),
				Size:     info.Size(),
				ModTime:  info.ModTime(),
				MimeType: mime,
				IsBinary: isBin,
			})
		}
		return nil
	})

	return matches, nil
}

// ReadFile safely reads line ranges from a text file.
func (e *Engine) ReadFile(relPath string, startLine, endLine int) (*ReadFileResult, error) {
	realPath, normalizedRel, err := e.resolvePath(relPath)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(realPath)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, ErrIsDirectory
	}

	if info.Size() > e.maxReadBytes {
		return nil, ErrFileTooLarge
	}

	if isFileBinary(realPath, info.Size()) {
		return nil, ErrBinaryFile
	}

	f, err := os.Open(realPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	currentLine := 0
	totalLines := 0

	for scanner.Scan() {
		currentLine++
		totalLines++
		if (startLine <= 0 || currentLine >= startLine) && (endLine <= 0 || currentLine <= endLine) {
			lines = append(lines, scanner.Text())
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	if startLine <= 0 {
		startLine = 1
	}
	if endLine <= 0 || endLine > totalLines {
		endLine = totalLines
	}

	mimeType := detectMime(info.Name(), realPath)
	return &ReadFileResult{
		Path:       normalizedRel,
		TotalLines: totalLines,
		StartLine:  startLine,
		EndLine:    endLine,
		Content:    strings.Join(lines, "\n"),
		IsBinary:   false,
		Size:       info.Size(),
		MimeType:   mimeType,
	}, nil
}

// GetRawFile reads raw bytes with offset and length for streaming or binary inspection.
func (e *Engine) GetRawFile(relPath string, offset, length int64) (*RawFileResult, error) {
	realPath, normalizedRel, err := e.resolvePath(relPath)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(realPath)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, ErrIsDirectory
	}

	f, err := os.Open(realPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	size := info.Size()
	if offset < 0 {
		offset = 0
	}
	if offset > size {
		offset = size
	}

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}

	readLen := size - offset
	if length >= 0 && length < readLen {
		readLen = length
	}
	if readLen > e.maxReadBytes {
		readLen = e.maxReadBytes
	}

	buf := make([]byte, readLen)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	}

	mimeType := detectMime(info.Name(), realPath)
	return &RawFileResult{
		Path:     normalizedRel,
		Size:     size,
		Offset:   offset,
		Length:   int64(n),
		MimeType: mimeType,
		Data:     buf[:n],
	}, nil
}

// StreamRawFile opens a file for chunked streaming, seeking to offset and returning an io.ReadCloser,
// total file size, mime type, and error. Bounded memory: caller reads in small buffers.
func (e *Engine) StreamRawFile(relPath string, offset, length int64) (io.ReadCloser, int64, string, error) {
	realPath, _, err := e.resolvePath(relPath)
	if err != nil {
		return nil, 0, "", err
	}

	info, err := os.Stat(realPath)
	if err != nil {
		return nil, 0, "", err
	}
	if info.IsDir() {
		return nil, 0, "", ErrIsDirectory
	}

	f, err := os.Open(realPath)
	if err != nil {
		return nil, 0, "", err
	}

	size := info.Size()
	if offset < 0 {
		offset = 0
	}
	if offset > size {
		offset = size
	}

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, 0, "", err
	}

	mimeType := detectMime(info.Name(), realPath)

	var rc io.ReadCloser = f
	if length >= 0 && offset+length < size {
		rc = struct {
			io.Reader
			io.Closer
		}{
			Reader: io.LimitReader(f, length),
			Closer: f,
		}
	}

	return rc, size, mimeType, nil
}

// Compare produces a line-by-line diff between two text files.
func (e *Engine) Compare(path1, path2 string) (*CompareResult, error) {
	res1, err := e.ReadFile(path1, 1, 0)
	if err != nil {
		return nil, fmt.Errorf("failed reading path1: %w", err)
	}
	res2, err := e.ReadFile(path2, 1, 0)
	if err != nil {
		return nil, fmt.Errorf("failed reading path2: %w", err)
	}

	lines1 := strings.Split(res1.Content, "\n")
	lines2 := strings.Split(res2.Content, "\n")

	var diff []CompareDiffLine
	maxLines := len(lines1)
	if len(lines2) > maxLines {
		maxLines = len(lines2)
	}

	for i := 0; i < maxLines; i++ {
		l1 := ""
		l2 := ""
		if i < len(lines1) {
			l1 = lines1[i]
		}
		if i < len(lines2) {
			l2 = lines2[i]
		}

		if i < len(lines1) && i < len(lines2) {
			if l1 == l2 {
				diff = append(diff, CompareDiffLine{Type: " ", LineNum: i + 1, Text: l1})
			} else {
				diff = append(diff, CompareDiffLine{Type: "-", LineNum: i + 1, Text: l1})
				diff = append(diff, CompareDiffLine{Type: "+", LineNum: i + 1, Text: l2})
			}
		} else if i < len(lines1) {
			diff = append(diff, CompareDiffLine{Type: "-", LineNum: i + 1, Text: l1})
		} else {
			diff = append(diff, CompareDiffLine{Type: "+", LineNum: i + 1, Text: l2})
		}
	}

	return &CompareResult{
		Path1: path1,
		Path2: path2,
		Diff:  diff,
	}, nil
}

// Manifest builds a snapshot inventory with SHA256 checksums of all files.
func (e *Engine) Manifest(relPath string) (*ManifestResult, error) {
	realPath, _, err := e.resolvePath(relPath)
	if err != nil {
		return nil, err
	}

	var items []ManifestItem
	var totalBytes int64

	_ = filepath.Walk(realPath, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
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

		isBin := isFileBinary(path, info.Size())
		mime := detectMime(info.Name(), path)

		// Calculate SHA256
		hashStr := ""
		if f, err := os.Open(path); err == nil {
			h := sha256.New()
			_, _ = io.Copy(h, f)
			f.Close()
			hashStr = hex.EncodeToString(h.Sum(nil))
		}

		items = append(items, ManifestItem{
			Path:     normRel,
			Size:     info.Size(),
			ModTime:  info.ModTime(),
			SHA256:   hashStr,
			MimeType: mime,
			IsBinary: isBin,
		})
		totalBytes += info.Size()
		return nil
	})

	return &ManifestResult{
		RootPath:   e.rootPath,
		TotalFiles: len(items),
		TotalBytes: totalBytes,
		CreatedAt:  time.Now(),
		Files:      items,
	}, nil
}

// Helper functions for binary detection & MIME typing
func isFileBinary(filePath string, size int64) bool {
	if size == 0 {
		return false
	}
	f, err := os.Open(filePath)
	if err != nil {
		return false
	}
	defer f.Close()

	buf := make([]byte, 512)
	n, err := f.Read(buf)
	if err != nil && err != io.EOF {
		return false
	}
	return DetectBinary(buf[:n])
}

func DetectBinary(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	if bytes.IndexByte(data, 0) != -1 {
		return true
	}
	nonPrintable := 0
	for _, b := range data {
		if (b < 0x09 || (b > 0x0A && b < 0x0D) || (b > 0x0D && b < 0x20)) && b != 0x1B {
			nonPrintable++
		}
	}
	if float64(nonPrintable)/float64(len(data)) > 0.15 {
		return true
	}
	return !utf8.Valid(data)
}

func detectMime(name, filePath string) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".go":
		return "text/x-go; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".ts":
		return "text/typescript; charset=utf-8"
	case ".py":
		return "text/x-python; charset=utf-8"
	case ".rs":
		return "text/rust; charset=utf-8"
	case ".c", ".h":
		return "text/x-c; charset=utf-8"
	case ".cpp", ".hpp", ".cc":
		return "text/x-c++; charset=utf-8"
	case ".java":
		return "text/x-java; charset=utf-8"
	case ".json":
		return "application/json"
	case ".md":
		return "text/markdown; charset=utf-8"
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".yaml", ".yml":
		return "text/yaml; charset=utf-8"
	case ".sh", ".bash":
		return "text/x-shellscript; charset=utf-8"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".svg":
		return "image/svg+xml"
	case ".pdf":
		return "application/pdf"
	case ".zip":
		return "application/zip"
	case ".tar":
		return "application/x-tar"
	case ".gz":
		return "application/gzip"
	}

	if f, err := os.Open(filePath); err == nil {
		buf := make([]byte, 512)
		n, _ := f.Read(buf)
		f.Close()
		if n > 0 {
			return http.DetectContentType(buf[:n])
		}
	}
	return "application/octet-stream"
}

func countLines(filePath string) int {
	f, err := os.Open(filePath)
	if err != nil {
		return 0
	}
	defer f.Close()

	count := 0
	buf := make([]byte, 32*1024)
	for {
		n, err := f.Read(buf)
		count += bytes.Count(buf[:n], []byte{'\n'})
		if err != nil {
			break
		}
	}
	return count
}
