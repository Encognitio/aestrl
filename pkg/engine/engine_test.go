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

	// Create structure:
	// src/main.go
	// src/util.go
	// docs/readme.md
	// .git/config (should be excluded)
	// .env (should be excluded)
	// secret.key (should be excluded)
	// binary.dat (binary content)
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0755); err != nil {
		t.Fatal(err)
	}

	mainGo := `package main

import "fmt"

func main() {
	token := "AestrlToken123"
	fmt.Println("Hello Aestrl", token)
}
`
	if err := os.WriteFile(filepath.Join(dir, "src", "main.go"), []byte(mainGo), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "readme.md"), []byte("# Aestrl Docs\nWelcome to Aestrl"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("[core]\nrepoformatversion = 0"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=12345"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret.key"), []byte("RSA PRIVATE KEY"), 0644); err != nil {
		t.Fatal(err)
	}
	binaryData := []byte{0x00, 0x01, 0x02, 0xFF, 0xFE, 0x00, 0x41}
	if err := os.WriteFile(filepath.Join(dir, "binary.dat"), binaryData, 0644); err != nil {
		t.Fatal(err)
	}

	cleanup := func() {
		os.RemoveAll(dir)
	}
	return dir, cleanup
}

func TestEngine_ListingAndExclusions(t *testing.T) {
	dir, cleanup := setupTestDir(t)
	defer cleanup()

	eng, err := New(dir, nil, 0)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	items, err := eng.ListDirectory("")
	if err != nil {
		t.Fatalf("ListDirectory failed: %v", err)
	}

	names := make(map[string]bool)
	for _, it := range items {
		names[it.Name] = true
	}

	if !names["src"] || !names["docs"] {
		t.Errorf("expected src and docs, got %+v", names)
	}
	// Check that excluded items are NOT present
	if names[".git"] || names[".env"] || names["secret.key"] {
		t.Errorf("excluded files were returned in listing: %+v", names)
	}
}

func TestEngine_TraversalProtection(t *testing.T) {
	dir, cleanup := setupTestDir(t)
	defer cleanup()

	eng, err := New(dir, nil, 0)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	// Try reading outside path
	_, err = eng.ReadFile("../../../etc/passwd", 0, 0)
	if err != ErrAccessDenied {
		t.Errorf("expected ErrAccessDenied for traversal, got: %v", err)
	}

	// Try symlink pointing outside
	outsideDir, err := os.MkdirTemp("", "outside-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(outsideDir)

	outsideFile := filepath.Join(outsideDir, "secret_outside.txt")
	_ = os.WriteFile(outsideFile, []byte("outside content"), 0644)

	symlinkPath := filepath.Join(dir, "escapelink")
	if err := os.Symlink(outsideFile, symlinkPath); err == nil {
		_, err := eng.ReadFile("escapelink", 0, 0)
		if err != ErrAccessDenied {
			t.Errorf("expected ErrAccessDenied for symlink escape, got: %v", err)
		}
	}
}

func TestEngine_ReadFileAndRange(t *testing.T) {
	dir, cleanup := setupTestDir(t)
	defer cleanup()

	eng, err := New(dir, nil, 0)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	// Read full file
	res, err := eng.ReadFile("src/main.go", 0, 0)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if !strings.Contains(res.Content, "package main") {
		t.Errorf("content does not contain package main: %s", res.Content)
	}

	// Read line range
	rangeRes, err := eng.ReadFile("src/main.go", 5, 7)
	if err != nil {
		t.Fatalf("ReadFile line range failed: %v", err)
	}
	if !strings.Contains(rangeRes.Content, "func main()") {
		t.Errorf("range does not contain func main(): %s", rangeRes.Content)
	}
}

func TestEngine_SearchContent(t *testing.T) {
	dir, cleanup := setupTestDir(t)
	defer cleanup()

	eng, err := New(dir, nil, 0)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	matches, err := eng.SearchContent("AestrlToken123", "", false, 10, 1)
	if err != nil {
		t.Fatalf("SearchContent failed: %v", err)
	}

	if len(matches) == 0 {
		t.Fatalf("expected search match for AestrlToken123, got 0")
	}

	if matches[0].Path != "src/main.go" {
		t.Errorf("expected match in src/main.go, got: %s", matches[0].Path)
	}
	if len(matches[0].ContextBefore) == 0 {
		t.Errorf("expected context lines before match")
	}
}

func TestEngine_BinaryDetection(t *testing.T) {
	dir, cleanup := setupTestDir(t)
	defer cleanup()

	eng, err := New(dir, nil, 0)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	_, err = eng.ReadFile("binary.dat", 0, 0)
	if err != ErrBinaryFile {
		t.Errorf("expected ErrBinaryFile for binary.dat, got: %v", err)
	}
}

func TestEngine_Tree(t *testing.T) {
	dir, cleanup := setupTestDir(t)
	defer cleanup()

	eng, err := New(dir, nil, 0)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	tree, err := eng.Tree("", 5, 100)
	if err != nil {
		t.Fatalf("Tree failed: %v", err)
	}
	if tree == nil || len(tree.Children) == 0 {
		t.Fatalf("expected tree children, got: %+v", tree)
	}
}
