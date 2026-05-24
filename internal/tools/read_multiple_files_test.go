package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// read_multiple_files calls read_file in-process, bypassing the MCP dispatch
// alias layer — so it must hand read_file its canonical "file_path" key, not
// the pre-0.7.19 "path". This guards that contract: a key drift would make
// every file report "file_path is required".
func TestReadMultipleFiles_ReadsContentAndReportsPerFileErrors(t *testing.T) {
	dir := t.TempDir()
	fileA := filepath.Join(dir, "a.txt")
	fileB := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(fileA, []byte("alpha-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileB, []byte("bravo-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing.txt")

	raw, _ := json.Marshal(map[string]any{"paths": []string{fileA, missing, fileB}})
	out, err := (&ReadMultipleFiles{}).Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if strings.Contains(out, "file_path is required") {
		t.Fatalf("inner read used the wrong key for read_file:\n%s", out)
	}
	if !strings.Contains(out, "alpha-content") || !strings.Contains(out, "bravo-content") {
		t.Fatalf("expected both file contents in output:\n%s", out)
	}
	// The missing file errors inline without blocking the readable ones.
	if !strings.Contains(out, "### ERROR:") {
		t.Fatalf("expected an inline error for the missing path:\n%s", out)
	}
}
