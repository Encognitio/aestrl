package engine

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

var (
	ErrAccessDenied    = errors.New("access denied: path outside sharing root or excluded")
	ErrPathNotFound    = errors.New("path not found")
	ErrNotADirectory   = errors.New("path is not a directory")
	ErrIsDirectory     = errors.New("path is a directory")
	ErrFileTooLarge    = errors.New("file exceeds maximum read size")
	ErrBinaryFile      = errors.New("file is binary")
	ErrSearchTimeout   = errors.New("search timed out")
	ErrInvalidLineRange = errors.New("invalid line range")
)

// Default limits
const (
	DefaultMaxReadBytes   = 5 * 1024 * 1024 // 5 MB
	DefaultMaxSearchHits  = 200
	DefaultMaxTreeEntries = 2000
	DefaultSearchTimeout  = 10 * time.Second
)

// FileItem represents a filesystem entry.
type FileItem struct {
	Name      string    `json:"name"`
	Path      string    `json:"path"` // relative path from root using '/'
	IsDir     bool      `json:"is_dir"`
	Size      int64     `json:"size"`
	ModTime   time.Time `json:"mod_time"`
	MimeType  string    `json:"mime_type,omitempty"`
	IsBinary  bool      `json:"is_binary"`
}

// SearchMatch represents a content search hit.
type SearchMatch struct {
	Path          string   `json:"path"`
	LineNumber    int      `json:"line_number"`
	LineText      string   `json:"line_text"`
	ContextBefore []string `json:"context_before,omitempty"`
	ContextAfter  []string `json:"context_after,omitempty"`
}

// TreeNode represents a recursive directory tree node.
type TreeNode struct {
	Name     string      `json:"name"`
	Path     string      `json:"path"`
	IsDir    bool        `json:"is_dir"`
	Size     int64       `json:"size,omitempty"`
	Children []*TreeNode `json:"children,omitempty"`
}

// Engine performs safe, read-only file operations rooted at a directory.
type Engine struct {
	rootPath     string
	realRoot     string
	exclusions   []string
	maxReadBytes int64
	mu           sync.RWMutex
}

// DefaultExclusions contains safety exclusions for sensitive files and heavy build artifacts.
var DefaultExclusions = []string{
	// Sensitive / Secrets
	".git",
	".env*",
	"*credential*",
	"*secret*",
	"*.pem",
	"*.key",
	"id_rsa*",
	"id_ed25519*",
	"*.pfx",
	"*.p12",
	"*.kdbx",
	// Build caches & dependencies
	"node_modules",
	"vendor",
	"__pycache__",
	".idea",
	".vscode",
	"target",
	"dist",
	"build",
	".next",
	".nuxt",
	"bin",
	"obj",
	".DS_Store",
	"Thumbs.db",
}

// New creates a new safe read-only inspection engine for the target root directory.
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

	exclusions := append([]string{}, DefaultExclusions...)
	exclusions = append(exclusions, customExclusions...)

	if maxReadBytes <= 0 {
		maxReadBytes = DefaultMaxReadBytes
	}

	return &Engine{
		rootPath:     absPath,
		realRoot:     realRoot,
		exclusions:   exclusions,
		maxReadBytes: maxReadBytes,
	}, nil
}

// RootPath returns the absolute sharing root directory.
func (e *Engine) RootPath() string {
	return e.rootPath
}

// isExcluded checks if a name or relative path matches exclusion patterns.
func (e *Engine) isExcluded(name, relPath string) bool {
	for _, pattern := range e.exclusions {
		// Match against name
		if matched, _ := filepath.Match(pattern, name); matched {
			return true
		}
		// Match against relative path parts
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
	// Normalize slashes
	cleanRel := filepath.Clean(strings.ReplaceAll(rel, "/", string(filepath.Separator)))
	if cleanRel == "." || cleanRel == "" {
		cleanRel = ""
	}

	// Disallow any traversal escape
	if strings.HasPrefix(cleanRel, "..") || filepath.IsAbs(cleanRel) {
		return "", "", ErrAccessDenied
	}

	target := filepath.Join(e.realRoot, cleanRel)

	// Check exclusions on relative path
	if cleanRel != "" {
		baseName := filepath.Base(cleanRel)
		if e.isExcluded(baseName, cleanRel) {
			return "", "", ErrAccessDenied
		}
	}

	// Check target exists
	info, err := os.Stat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", ErrPathNotFound
		}
		return "", "", err
	}

	// Verify physical symlink resolution does not escape realRoot
	realTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", "", fmt.Errorf("failed to resolve symlink: %w", err)
	}

	relToRoot, err := filepath.Rel(e.realRoot, realTarget)
	if err != nil || strings.HasPrefix(relToRoot, "..") {
		return "", "", ErrAccessDenied
	}

	_ = info
	// Use forward slash for normalized relative path across all OSes
	normRel := filepath.ToSlash(cleanRel)
	return realTarget, normRel, nil
}

// DetectBinary checks if data or a file is binary.
func DetectBinary(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	// Check for null bytes in the first sample
	sample := data
	if len(sample) > 1024 {
		sample = sample[:1024]
	}
	if bytes.IndexByte(sample, 0) != -1 {
		return true
	}
	// Check if valid utf-8
	return !utf8.Valid(sample)
}

// ListDirectory lists contents of a directory.
func (e *Engine) ListDirectory(relPath string) ([]FileItem, error) {
	realPath, _, err := e.resolvePath(relPath)
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(realPath)
	if err != nil {
		return nil, err
	}

	var results []FileItem
	for _, entry := range entries {
		name := entry.Name()
		childRel := filepath.ToSlash(filepath.Join(relPath, name))
		if e.isExcluded(name, childRel) {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		item := FileItem{
			Name:    name,
			Path:    childRel,
			IsDir:   entry.IsDir(),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		}

		if !entry.IsDir() {
			item.MimeType = detectMime(name, realPath)
			// Quick binary check for files
			item.IsBinary = isFileBinary(filepath.Join(realPath, name), info.Size())
		}

		results = append(results, item)
	}

	return results, nil
}

// Tree generates a recursive directory tree up to maxDepth and maxEntries.
func (e *Engine) Tree(relPath string, maxDepth, maxEntries int) (*TreeNode, error) {
	realPath, normRel, err := e.resolvePath(relPath)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(realPath)
	if err != nil {
		return nil, err
	}

	if maxDepth <= 0 {
		maxDepth = 10
	}
	if maxEntries <= 0 {
		maxEntries = DefaultMaxTreeEntries
	}

	count := 0
	return e.buildTree(realPath, normRel, info.Name(), info.IsDir(), 0, maxDepth, &count, maxEntries)
}

func (e *Engine) buildTree(realPath, relPath, name string, isDir bool, currentDepth, maxDepth int, count *int, maxEntries int) (*TreeNode, error) {
	*count++
	if *count > maxEntries {
		return nil, nil
	}

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
		entryName := entry.Name()
		childRel := filepath.ToSlash(filepath.Join(relPath, entryName))
		if e.isExcluded(entryName, childRel) {
			continue
		}

		childRealPath := filepath.Join(realPath, entryName)
		childNode, err := e.buildTree(childRealPath, childRel, entryName, entry.IsDir(), currentDepth+1, maxDepth, count, maxEntries)
		if err == nil && childNode != nil {
			node.Children = append(node.Children, childNode)
		}
		if *count >= maxEntries {
			break
		}
	}

	return node, nil
}

// SearchFiles searches for files whose names match a glob pattern or substring.
func (e *Engine) SearchFiles(query, relPath string, maxResults int) ([]FileItem, error) {
	realPath, normRel, err := e.resolvePath(relPath)
	if err != nil {
		return nil, err
	}

	if maxResults <= 0 {
		maxResults = DefaultMaxSearchHits
	}

	queryLower := strings.ToLower(query)
	var results []FileItem

	err = filepath.WalkDir(realPath, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}

		name := d.Name()
		relChild, rErr := filepath.Rel(e.realRoot, path)
		if rErr != nil {
			return nil
		}
		normChild := filepath.ToSlash(relChild)

		if e.isExcluded(name, normChild) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Match glob or case-insensitive substring
		matched, _ := filepath.Match(queryLower, strings.ToLower(name))
		if !matched && strings.Contains(strings.ToLower(name), queryLower) {
			matched = true
		}

		if matched && normChild != "." && normChild != normRel {
			info, err := d.Info()
			if err == nil {
				item := FileItem{
					Name:    name,
					Path:    normChild,
					IsDir:   d.IsDir(),
					Size:    info.Size(),
					ModTime: info.ModTime(),
				}
				if !d.IsDir() {
					item.MimeType = detectMime(name, path)
					item.IsBinary = isFileBinary(path, info.Size())
				}
				results = append(results, item)
				if len(results) >= maxResults {
					return io.EOF // Stop walk
				}
			}
		}

		return nil
	})

	if err != nil && err != io.EOF {
		return nil, err
	}
	return results, nil
}

// SearchContent searches file contents for literal or regex matches with line numbers and context.
func (e *Engine) SearchContent(query, relPath string, isRegex bool, maxResults, contextLines int) ([]SearchMatch, error) {
	realPath, _, err := e.resolvePath(relPath)
	if err != nil {
		return nil, err
	}

	if maxResults <= 0 {
		maxResults = DefaultMaxSearchHits
	}
	if contextLines < 0 {
		contextLines = 2
	}

	var reg *regexp.Regexp
	if isRegex {
		var rErr error
		reg, rErr = regexp.Compile(query)
		if rErr != nil {
			return nil, fmt.Errorf("invalid regex: %w", rErr)
		}
	}

	deadline := time.Now().Add(DefaultSearchTimeout)
	var matches []SearchMatch

	err = filepath.WalkDir(realPath, func(path string, d os.DirEntry, walkErr error) error {
		if time.Now().After(deadline) {
			return ErrSearchTimeout
		}
		if walkErr != nil {
			return nil
		}

		name := d.Name()
		relChild, rErr := filepath.Rel(e.realRoot, path)
		if rErr != nil {
			return nil
		}
		normChild := filepath.ToSlash(relChild)

		if e.isExcluded(name, normChild) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			return nil
		}

		info, err := d.Info()
		if err != nil || info.Size() > e.maxReadBytes {
			return nil
		}

		// Skip binary files
		if isFileBinary(path, info.Size()) {
			return nil
		}

		// Search file lines
		fileMatches, err := e.searchInFile(path, normChild, query, reg, isRegex, contextLines, maxResults-len(matches))
		if err == nil && len(fileMatches) > 0 {
			matches = append(matches, fileMatches...)
			if len(matches) >= maxResults {
				return io.EOF
			}
		}

		return nil
	})

	if err != nil && err != io.EOF {
		return matches, err
	}
	return matches, nil
}

func (e *Engine) searchInFile(filePath, relPath, query string, reg *regexp.Regexp, isRegex bool, contextLines, maxRemaining int) ([]SearchMatch, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	// Bounded line scanner
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 256*1024)

	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil && err != bufio.ErrTooLong {
		return nil, err
	}

	var results []SearchMatch
	queryLower := strings.ToLower(query)

	for i, line := range lines {
		matched := false
		if isRegex {
			matched = reg.MatchString(line)
		} else {
			matched = strings.Contains(strings.ToLower(line), queryLower)
		}

		if matched {
			lineNum := i + 1
			match := SearchMatch{
				Path:       relPath,
				LineNumber: lineNum,
				LineText:   line,
			}

			// Context before
			startBefore := i - contextLines
			if startBefore < 0 {
				startBefore = 0
			}
			for b := startBefore; b < i; b++ {
				match.ContextBefore = append(match.ContextBefore, lines[b])
			}

			// Context after
			endAfter := i + 1 + contextLines
			if endAfter > len(lines) {
				endAfter = len(lines)
			}
			for a := i + 1; a < endAfter; a++ {
				match.ContextAfter = append(match.ContextAfter, lines[a])
			}

			results = append(results, match)
			if len(results) >= maxRemaining {
				break
			}
		}
	}

	return results, nil
}

// ReadFileResult contains bounded file content or line range.
type ReadFileResult struct {
	Path       string   `json:"path"`
	TotalLines int      `json:"total_lines"`
	StartLine  int      `json:"start_line,omitempty"`
	EndLine    int      `json:"end_line,omitempty"`
	Content    string   `json:"content"`
	IsBinary   bool     `json:"is_binary"`
	Size       int64    `json:"size"`
	MimeType   string   `json:"mime_type"`
}

// ReadFile reads bounded content or a line range from a text file.
func (e *Engine) ReadFile(relPath string, startLine, endLine int) (*ReadFileResult, error) {
	realPath, normRel, err := e.resolvePath(relPath)
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
		return nil, fmt.Errorf("%w (file is %d bytes, max is %d bytes)", ErrFileTooLarge, info.Size(), e.maxReadBytes)
	}

	mimeType := detectMime(info.Name(), realPath)
	if isFileBinary(realPath, info.Size()) {
		return &ReadFileResult{
			Path:     normRel,
			IsBinary: true,
			Size:     info.Size(),
			MimeType: mimeType,
		}, ErrBinaryFile
	}

	f, err := os.Open(realPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var allLines []string
	scanner := bufio.NewScanner(f)
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 512*1024)

	for scanner.Scan() {
		allLines = append(allLines, scanner.Text())
	}
	if err := scanner.Err(); err != nil && err != bufio.ErrTooLong {
		return nil, err
	}

	total := len(allLines)
	res := &ReadFileResult{
		Path:       normRel,
		TotalLines: total,
		Size:       info.Size(),
		MimeType:   mimeType,
	}

	// Line range requested
	if startLine > 0 || endLine > 0 {
		if startLine <= 0 {
			startLine = 1
		}
		if endLine <= 0 || endLine > total {
			endLine = total
		}
		if startLine > endLine || startLine > total {
			return nil, ErrInvalidLineRange
		}

		res.StartLine = startLine
		res.EndLine = endLine

		selected := allLines[startLine-1 : endLine]
		res.Content = strings.Join(selected, "\n")
	} else {
		res.StartLine = 1
		res.EndLine = total
		res.Content = strings.Join(allLines, "\n")
	}

	return res, nil
}

// FileInfo returns detailed metadata for a file or directory.
func (e *Engine) FileInfo(relPath string) (*FileItem, error) {
	realPath, normRel, err := e.resolvePath(relPath)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(realPath)
	if err != nil {
		return nil, err
	}

	item := &FileItem{
		Name:    info.Name(),
		Path:    normRel,
		IsDir:   info.IsDir(),
		Size:    info.Size(),
		ModTime: info.ModTime(),
	}

	if !info.IsDir() {
		item.MimeType = detectMime(info.Name(), realPath)
		item.IsBinary = isFileBinary(realPath, info.Size())
	}

	return item, nil
}

// GetRawFile opens the file for raw streaming transfer within bounds.
func (e *Engine) GetRawFile(relPath string) (io.ReadCloser, int64, string, error) {
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

	mimeType := detectMime(info.Name(), realPath)
	return f, info.Size(), mimeType, nil
}

// isFileBinary reads the first 512 bytes of a file to check for binary bytes.
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

// detectMime guesses mime type based on extension and content.
func detectMime(name, filePath string) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".go":
		return "text/x-go; charset=utf-8"
	case ".js", ".mjs":
		return "application/javascript; charset=utf-8"
	case ".ts", ".tsx":
		return "application/typescript; charset=utf-8"
	case ".jsx":
		return "text/jsx; charset=utf-8"
	case ".json":
		return "application/json"
	case ".md", ".markdown":
		return "text/markdown; charset=utf-8"
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".txt":
		return "text/plain; charset=utf-8"
	case ".yaml", ".yml":
		return "text/yaml; charset=utf-8"
	case ".toml":
		return "text/toml; charset=utf-8"
	case ".py":
		return "text/x-python; charset=utf-8"
	case ".rs":
		return "text/x-rust; charset=utf-8"
	case ".c", ".h", ".cpp", ".hpp":
		return "text/x-c; charset=utf-8"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".svg":
		return "image/svg+xml"
	case ".webp":
		return "image/webp"
	case ".pdf":
		return "application/pdf"
	case ".mp3":
		return "audio/mpeg"
	case ".mp4":
		return "video/mp4"
	case ".wav":
		return "audio/wav"
	}

	// Sniff first 512 bytes
	if f, err := os.Open(filePath); err == nil {
		defer f.Close()
		buf := make([]byte, 512)
		if n, err := f.Read(buf); err == nil && n > 0 {
			return http.DetectContentType(buf[:n])
		}
	}

	return "application/octet-stream"
}
