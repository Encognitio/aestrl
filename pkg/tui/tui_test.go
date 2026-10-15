package tui

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Encognitio/aestrl/pkg/engine"
)

func TestTUIRun(t *testing.T) {
	tempDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(tempDir, "file1.txt"), []byte("Hello world in file 1"), 0644)
	subDir := filepath.Join(tempDir, "subdir")
	_ = os.Mkdir(subDir, 0755)
	_ = os.WriteFile(filepath.Join(subDir, "file2.txt"), []byte("Hello in sub directory"), 0644)

	eng, err := engine.New(tempDir, nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	invoker := &EngineInvoker{Eng: eng}

	// Commands:
	// "t\n\n" (show tree, press enter)
	// "m\n\n" (show manifest, press enter)
	// "q\n" (quit)
	in := bytes.NewBufferString("t\n\nm\n\nq\n")
	out := &bytes.Buffer{}

	err = Run(invoker, "Test Share", in, out)
	if err != nil {
		t.Fatalf("TUI Run failed: %v", err)
	}

	outStr := out.String()
	if !strings.Contains(outStr, "AESTRL INSPECT — Test Share") {
		t.Fatalf("expected title in output, got:\n%s", outStr)
	}
	if !strings.Contains(outStr, "Exiting Aestrl inspection.") {
		t.Fatalf("expected clean exit message, got:\n%s", outStr)
	}
}
