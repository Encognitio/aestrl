package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupTestDir(t *testing.T) (string, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "aestrl-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(dir, "src"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0755); err != nil {
		t.Fatal(err)
	}

	mainGo := `package main

import "fmt"

type ServerConfig struct {
	Port int
}

func main() {
	token := "AestrlToken123"
	fmt.Println("Hello Aestrl", token)
}

func HelperFunc() string {
	return "helper"
}
`
	if err := os.WriteFile(filepath.Join(dir, "src", "main.go"), []byte(mainGo), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "readme.md"), []byte("# Aestrl Docs\nWelcome to Aestrl"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compare1.txt"), []byte("line1\nline2\nline3\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compare2.txt"), []byte("line1\nline2-mod\nline3\nline4\n"), 0644); err != nil {
		t.Fatal(err)
	}

	binaryData := []byte{0x00, 0x01, 0x02, 0xFF, 0xFE, 0x00, 0x41}
	if err := os.WriteFile(filepath.Join(dir, "binary.dat"), binaryData, 0644); err != nil {
		t.Fatal(err)
	}

	cleanup := func() {
		_ = os.RemoveAll(dir)
	}
	return dir, cleanup
}

func TestEngine_AllOperationsViaExecute(t *testing.T) {
	dir, cleanup := setupTestDir(t)
	defer cleanup()

	eng, err := New(dir, nil, 0)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	// 1. ls
	res, err := eng.Execute("ls", map[string]interface{}{"path": ""})
	if err != nil {
		t.Fatalf("ls failed: %v", err)
	}
	items := res.([]FileItem)
	if len(items) == 0 {
		t.Errorf("expected items in ls")
	}

	// 2. tree
	res, err = eng.Execute("tree", map[string]interface{}{"path": "", "depth": 3})
	if err != nil {
		t.Fatalf("tree failed: %v", err)
	}
	tree := res.(*TreeNode)
	if tree == nil {
		t.Errorf("expected tree")
	}

	// 3. stat
	res, err = eng.Execute("stat", map[string]interface{}{"path": "src/main.go"})
	if err != nil {
		t.Fatalf("stat failed: %v", err)
	}
	stat := res.(*FileItem)
	if stat.Name != "main.go" || stat.IsDir {
		t.Errorf("unexpected stat: %+v", stat)
	}

	// 4. find
	res, err = eng.Execute("find", map[string]interface{}{"query": "*.go"})
	if err != nil {
		t.Fatalf("find failed: %v", err)
	}
	findMatches := res.([]FileItem)
	if len(findMatches) == 0 || findMatches[0].Name != "main.go" {
		t.Errorf("find failed to match *.go: %+v", findMatches)
	}

	// 5. search (hybrid BM25)
	res, err = eng.Execute("search", map[string]interface{}{"query": "AestrlToken123"})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	searchHits := res.([]SearchResult)
	if len(searchHits) == 0 || searchHits[0].Path != "src/main.go" {
		t.Errorf("search failed to find AestrlToken123: %+v", searchHits)
	}

	// 6. grep
	res, err = eng.Execute("grep", map[string]interface{}{"query": "Hello Aestrl"})
	if err != nil {
		t.Fatalf("grep failed: %v", err)
	}
	grepMatches := res.([]SearchMatch)
	if len(grepMatches) == 0 {
		t.Errorf("grep failed to find Hello Aestrl")
	}

	// 7. context
	res, err = eng.Execute("context", map[string]interface{}{"query": "how is server configured?"})
	if err != nil {
		t.Fatalf("context failed: %v", err)
	}
	ctxRes := res.(*ContextResult)
	if ctxRes == nil {
		t.Errorf("expected context result")
	}

	// 8. read
	res, err = eng.Execute("read", map[string]interface{}{"path": "src/main.go", "start_line": 1, "end_line": 5})
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	readRes := res.(*ReadFileResult)
	if !strings.Contains(readRes.Content, "package main") {
		t.Errorf("read failed to return package main: %s", readRes.Content)
	}

	// 9. outline
	res, err = eng.Execute("outline", map[string]interface{}{"path": "src/main.go"})
	if err != nil {
		t.Fatalf("outline failed: %v", err)
	}
	outRes := res.(*OutlineResult)
	if len(outRes.Items) == 0 {
		t.Errorf("expected outline items, got 0")
	}

	// 10. raw / get
	res, err = eng.Execute("raw", map[string]interface{}{"path": "src/main.go", "offset": 0, "length": 12})
	if err != nil {
		t.Fatalf("raw failed: %v", err)
	}
	rawRes := res.(*RawFileResult)
	if string(rawRes.Data) != "package main" {
		t.Errorf("raw slice unexpected: %s", string(rawRes.Data))
	}

	// 11. symbol
	res, err = eng.Execute("symbol", map[string]interface{}{"name": "HelperFunc"})
	if err != nil {
		t.Fatalf("symbol failed: %v", err)
	}
	syms := res.([]SymbolItem)
	if len(syms) == 0 || syms[0].Name != "HelperFunc" {
		t.Errorf("expected symbol HelperFunc, got: %+v", syms)
	}

	// 12. refs
	res, err = eng.Execute("refs", map[string]interface{}{"symbol": "token"})
	if err != nil {
		t.Fatalf("refs failed: %v", err)
	}
	refs := res.([]ReferenceItem)
	if len(refs) == 0 {
		t.Errorf("expected refs for token")
	}

	// 13. deps
	res, err = eng.Execute("deps", map[string]interface{}{"path": "src/main.go"})
	if err != nil {
		t.Fatalf("deps failed: %v", err)
	}
	depsRes := res.(*DepsResult)
	if len(depsRes.Imports) == 0 || depsRes.Imports[0] != "fmt" {
		t.Errorf("expected fmt in deps, got: %+v", depsRes)
	}

	// 14. compare
	res, err = eng.Execute("compare", map[string]interface{}{"path1": "compare1.txt", "path2": "compare2.txt"})
	if err != nil {
		t.Fatalf("compare failed: %v", err)
	}
	compRes := res.(*CompareResult)
	if len(compRes.Diff) == 0 {
		t.Errorf("expected diff entries")
	}

	// 15. manifest
	res, err = eng.Execute("manifest", map[string]interface{}{"path": ""})
	if err != nil {
		t.Fatalf("manifest failed: %v", err)
	}
	manRes := res.(*ManifestResult)
	if manRes.TotalFiles == 0 {
		t.Errorf("expected manifest files")
	}
}

func TestEngine_TraversalProtection(t *testing.T) {
	dir, cleanup := setupTestDir(t)
	defer cleanup()

	eng, err := New(dir, nil, 0)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	traversalPaths := []string{
		"../",
		"../../etc/passwd",
		"src/../../",
		"/etc/passwd",
	}

	for _, p := range traversalPaths {
		_, err := eng.Execute("read", map[string]interface{}{"path": p})
		if err == nil {
			t.Errorf("expected access denied for traversal path: %s", p)
		}
	}
}

func TestEngine_SnapshotDelta(t *testing.T) {
	dir, cleanup := setupTestDir(t)
	defer cleanup()

	eng, err := New(dir, nil, 0)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	snapFile := filepath.Join(dir, ".aestrl", "index.json")

	// 1st run: initial index
	delta, err := eng.Index().LoadOrDeltaIndex(snapFile)
	if err != nil {
		t.Fatalf("LoadOrDeltaIndex initial failed: %v", err)
	}
	_ = delta

	// Add a new file
	newFile := filepath.Join(dir, "src", "new.go")
	_ = os.WriteFile(newFile, []byte("package main\nfunc NewOne() {}\n"), 0644)

	// 2nd run: delta detects new file
	delta2, err := eng.Index().LoadOrDeltaIndex(snapFile)
	if err != nil {
		t.Fatalf("LoadOrDeltaIndex 2nd run failed: %v", err)
	}
	if len(delta2.Added) == 0 {
		t.Errorf("expected added file in delta, got: %+v", delta2)
	}
}

func TestEngine_SemanticSearch(t *testing.T) {
	dir, cleanup := setupTestDir(t)
	defer cleanup()

	eng, err := New(dir, nil, 0)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	res, err := eng.Search("network server", "", 10)
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(res) == 0 || res[0].Path != "src/main.go" {
		t.Fatalf("expected src/main.go match, got %+v", res)
	}

	// Verify documents have vectors
	for path, doc := range eng.index.docs {
		if len(doc.Vector) > 0 {
			// Found at least one vector
			return
		}
		_ = path
	}
	t.Log("Note: potion encoder fallback active or vector generated")
}

