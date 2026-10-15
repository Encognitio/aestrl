package engine

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// OutlineItem represents a structural code/document element.
type OutlineItem struct {
	Path       string `json:"path"`
	LineNumber int    `json:"line_number"`
	Kind       string `json:"kind"` // "function", "class", "type", "struct", "heading", "interface"
	Name       string `json:"name"`
	Signature  string `json:"signature,omitempty"`
}

// OutlineResult packages outline items for a file or directory.
type OutlineResult struct {
	Path  string        `json:"path"`
	Items []OutlineItem `json:"items"`
}

// SymbolItem represents a defined symbol.
type SymbolItem struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Path       string `json:"path"`
	LineNumber int    `json:"line_number"`
	LineText   string `json:"line_text"`
}

// ReferenceItem represents a usage or caller of a symbol.
type ReferenceItem struct {
	Symbol     string `json:"symbol"`
	Path       string `json:"path"`
	LineNumber int    `json:"line_number"`
	LineText   string `json:"line_text"`
}

// DepsResult represents detected package imports and dependencies.
type DepsResult struct {
	Path         string              `json:"path"`
	Imports      []string            `json:"imports"`
	Dependencies map[string][]string `json:"dependencies,omitempty"` // file -> imports
}

var (
	// Language patterns for definition extraction
	reGoFunc      = regexp.MustCompile(`^func\s+(?:\([^)]+\)\s+)?([A-Za-z0-9_]+)`)
	reGoType      = regexp.MustCompile(`^type\s+([A-Za-z0-9_]+)\s+(struct|interface|[A-Za-z0-9_]+)`)
	rePyDef       = regexp.MustCompile(`^\s*(?:async\s+)?def\s+([A-Za-z0-9_]+)`)
	rePyClass     = regexp.MustCompile(`^\s*class\s+([A-Za-z0-9_]+)`)
	reJSFunc      = regexp.MustCompile(`^\s*(?:export\s+)?(?:async\s+)?function\s+([A-Za-z0-9_]+)`)
	reJSClass     = regexp.MustCompile(`^\s*(?:export\s+)?class\s+([A-Za-z0-9_]+)`)
	reJSConst     = regexp.MustCompile(`^\s*(?:export\s+)?const\s+([A-Za-z0-9_]+)\s*=\s*(?:async\s*)?\(`)
	reRustFn      = regexp.MustCompile(`^\s*(?:pub\s+)?(?:async\s+)?fn\s+([A-Za-z0-9_]+)`)
	reRustStruct  = regexp.MustCompile(`^\s*(?:pub\s+)?struct\s+([A-Za-z0-9_]+)`)
	reHeading        = regexp.MustCompile(`^(#{1,6})\s+(.+)`)
	reImportGo       = regexp.MustCompile(`^\s*"([^"]+)"`)
	reImportGoSingle = regexp.MustCompile(`^\s*import\s+(?:[A-Za-z0-9_.]+\s+)?"([^"]+)"`)
	reImportPy       = regexp.MustCompile(`^\s*(?:import\s+([A-Za-z0-9_.,\s]+)|from\s+([A-Za-z0-9_.]+)\s+import)`)
	reImportJS    = regexp.MustCompile(`(?:import\s+.*?from\s+['"]([^'"]+)['"]|require\(['"]([^'"]+)['"]\))`)
	reImportRust  = regexp.MustCompile(`^\s*use\s+([^;]+);`)
	reImportC     = regexp.MustCompile(`^\s*#include\s+[<"]([^>"]+)[>"]`)
)

// Outline extracts headings, functions, types, and classes from files.
func (e *Engine) Outline(relPath string) (*OutlineResult, error) {
	realPath, normalizedRel, err := e.resolvePath(relPath)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(realPath)
	if err != nil {
		return nil, err
	}

	var items []OutlineItem

	if !info.IsDir() {
		items = e.outlineFile(realPath, normalizedRel)
	} else {
		_ = filepath.Walk(realPath, func(path string, fi os.FileInfo, err error) error {
			if err != nil || fi.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(e.realRoot, path)
			if err != nil {
				return nil
			}
			normRel := filepath.ToSlash(rel)
			if e.isExcluded(fi.Name(), normRel) || isFileBinary(path, fi.Size()) {
				return nil
			}
			fileItems := e.outlineFile(path, normRel)
			items = append(items, fileItems...)
			return nil
		})
	}

	return &OutlineResult{
		Path:  normalizedRel,
		Items: items,
	}, nil
}

func (e *Engine) outlineFile(filePath, relPath string) []OutlineItem {
	f, err := os.Open(filePath)
	if err != nil {
		return nil
	}
	defer f.Close()

	var items []OutlineItem
	scanner := bufio.NewScanner(f)
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		// Markdown Headings
		if m := reHeading.FindStringSubmatch(trimmed); len(m) > 2 {
			items = append(items, OutlineItem{
				Path:       relPath,
				LineNumber: lineNum,
				Kind:       "heading",
				Name:       m[2],
				Signature:  m[1],
			})
			continue
		}

		// Go
		if m := reGoFunc.FindStringSubmatch(trimmed); len(m) > 1 {
			items = append(items, OutlineItem{
				Path:       relPath,
				LineNumber: lineNum,
				Kind:       "function",
				Name:       m[1],
				Signature:  trimmed,
			})
			continue
		}
		if m := reGoType.FindStringSubmatch(trimmed); len(m) > 2 {
			items = append(items, OutlineItem{
				Path:       relPath,
				LineNumber: lineNum,
				Kind:       m[2],
				Name:       m[1],
				Signature:  trimmed,
			})
			continue
		}

		// Python
		if m := rePyClass.FindStringSubmatch(trimmed); len(m) > 1 {
			items = append(items, OutlineItem{
				Path:       relPath,
				LineNumber: lineNum,
				Kind:       "class",
				Name:       m[1],
				Signature:  trimmed,
			})
			continue
		}
		if m := rePyDef.FindStringSubmatch(trimmed); len(m) > 1 {
			items = append(items, OutlineItem{
				Path:       relPath,
				LineNumber: lineNum,
				Kind:       "function",
				Name:       m[1],
				Signature:  trimmed,
			})
			continue
		}

		// JS/TS
		if m := reJSClass.FindStringSubmatch(trimmed); len(m) > 1 {
			items = append(items, OutlineItem{
				Path:       relPath,
				LineNumber: lineNum,
				Kind:       "class",
				Name:       m[1],
				Signature:  trimmed,
			})
			continue
		}
		if m := reJSFunc.FindStringSubmatch(trimmed); len(m) > 1 {
			items = append(items, OutlineItem{
				Path:       relPath,
				LineNumber: lineNum,
				Kind:       "function",
				Name:       m[1],
				Signature:  trimmed,
			})
			continue
		}
		if m := reJSConst.FindStringSubmatch(trimmed); len(m) > 1 {
			items = append(items, OutlineItem{
				Path:       relPath,
				LineNumber: lineNum,
				Kind:       "function",
				Name:       m[1],
				Signature:  trimmed,
			})
			continue
		}

		// Rust
		if m := reRustFn.FindStringSubmatch(trimmed); len(m) > 1 {
			items = append(items, OutlineItem{
				Path:       relPath,
				LineNumber: lineNum,
				Kind:       "function",
				Name:       m[1],
				Signature:  trimmed,
			})
			continue
		}
		if m := reRustStruct.FindStringSubmatch(trimmed); len(m) > 1 {
			items = append(items, OutlineItem{
				Path:       relPath,
				LineNumber: lineNum,
				Kind:       "struct",
				Name:       m[1],
				Signature:  trimmed,
			})
			continue
		}
	}

	return items
}

// Symbol searches for symbol definitions matching name.
func (e *Engine) Symbol(name, relPath string) ([]SymbolItem, error) {
	out, err := e.Outline(relPath)
	if err != nil {
		return nil, err
	}

	lowerName := strings.ToLower(name)
	var symbols []SymbolItem

	for _, item := range out.Items {
		if item.Kind == "heading" {
			continue
		}
		if name == "" || strings.EqualFold(item.Name, name) || strings.Contains(strings.ToLower(item.Name), lowerName) {
			symbols = append(symbols, SymbolItem{
				Name:       item.Name,
				Kind:       item.Kind,
				Path:       item.Path,
				LineNumber: item.LineNumber,
				LineText:   item.Signature,
			})
		}
	}
	return symbols, nil
}

// Refs finds occurrences and callers of symbol across files.
func (e *Engine) Refs(symbol, relPath string) ([]ReferenceItem, error) {
	if symbol == "" {
		return nil, fmt.Errorf("symbol name required")
	}

	matches, err := e.Grep(`\b`+regexp.QuoteMeta(symbol)+`\b`, relPath, true, 0, 100)
	if err != nil {
		// Fallback to literal search if regex fails
		matches, err = e.Grep(symbol, relPath, false, 0, 100)
		if err != nil {
			return nil, err
		}
	}

	var refs []ReferenceItem
	for _, m := range matches {
		refs = append(refs, ReferenceItem{
			Symbol:     symbol,
			Path:       m.Path,
			LineNumber: m.LineNumber,
			LineText:   strings.TrimSpace(m.LineText),
		})
	}
	return refs, nil
}

// Deps parses package imports and dependencies across files.
func (e *Engine) Deps(relPath string) (*DepsResult, error) {
	realPath, normalizedRel, err := e.resolvePath(relPath)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(realPath)
	if err != nil {
		return nil, err
	}

	depMap := make(map[string][]string)
	allImportsSet := make(map[string]bool)

	parseFileDeps := func(filePath, rel string) {
		f, err := os.Open(filePath)
		if err != nil {
			return
		}
		defer f.Close()

		var fileDeps []string
		scanner := bufio.NewScanner(f)
		inMultiImport := false

		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())

			// Go multi-import block
			if line == "import (" {
				inMultiImport = true
				continue
			}
			if inMultiImport {
				if line == ")" {
					inMultiImport = false
					continue
				}
				if m := reImportGo.FindStringSubmatch(line); len(m) > 1 {
					fileDeps = append(fileDeps, m[1])
					allImportsSet[m[1]] = true
				}
				continue
			}

			// Single line Go import
			if strings.HasPrefix(line, "import ") {
				if m := reImportGoSingle.FindStringSubmatch(line); len(m) > 1 {
					fileDeps = append(fileDeps, m[1])
					allImportsSet[m[1]] = true
				}
			}

			// JS / TS import/require
			if m := reImportJS.FindStringSubmatch(line); len(m) > 1 {
				dep := m[1]
				if dep == "" && len(m) > 2 {
					dep = m[2]
				}
				if dep != "" {
					fileDeps = append(fileDeps, dep)
					allImportsSet[dep] = true
				}
			}

			// Python import
			if m := reImportPy.FindStringSubmatch(line); len(m) > 1 {
				dep := m[1]
				if dep == "" && len(m) > 2 {
					dep = m[2]
				}
				if dep != "" {
					fileDeps = append(fileDeps, dep)
					allImportsSet[dep] = true
				}
			}

			// Rust use
			if m := reImportRust.FindStringSubmatch(line); len(m) > 1 {
				fileDeps = append(fileDeps, m[1])
				allImportsSet[m[1]] = true
			}

			// C/C++ #include
			if m := reImportC.FindStringSubmatch(line); len(m) > 1 {
				fileDeps = append(fileDeps, m[1])
				allImportsSet[m[1]] = true
			}
		}

		if len(fileDeps) > 0 {
			depMap[rel] = fileDeps
		}
	}

	if !info.IsDir() {
		parseFileDeps(realPath, normalizedRel)
	} else {
		_ = filepath.Walk(realPath, func(path string, fi os.FileInfo, err error) error {
			if err != nil || fi.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(e.realRoot, path)
			if err != nil {
				return nil
			}
			normRel := filepath.ToSlash(rel)
			if e.isExcluded(fi.Name(), normRel) || isFileBinary(path, fi.Size()) {
				return nil
			}
			parseFileDeps(path, normRel)
			return nil
		})
	}

	var allImports []string
	for imp := range allImportsSet {
		allImports = append(allImports, imp)
	}

	return &DepsResult{
		Path:         normalizedRel,
		Imports:      allImports,
		Dependencies: depMap,
	}, nil
}

func extractSymbols(lines []string) []string {
	var syms []string
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		if m := reGoFunc.FindStringSubmatch(trimmed); len(m) > 1 {
			syms = append(syms, m[1])
		} else if m := reGoType.FindStringSubmatch(trimmed); len(m) > 1 {
			syms = append(syms, m[1])
		} else if m := rePyDef.FindStringSubmatch(trimmed); len(m) > 1 {
			syms = append(syms, m[1])
		} else if m := rePyClass.FindStringSubmatch(trimmed); len(m) > 1 {
			syms = append(syms, m[1])
		} else if m := reJSFunc.FindStringSubmatch(trimmed); len(m) > 1 {
			syms = append(syms, m[1])
		} else if m := reJSClass.FindStringSubmatch(trimmed); len(m) > 1 {
			syms = append(syms, m[1])
		}
	}
	return syms
}
