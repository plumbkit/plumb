//go:build integration

package golangcilint_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/quality/golangcilint"
)

// TestIntegration_RealBinary drives the real golangci-lint binary on a file
// with a known vet issue and asserts a finding comes back. It guards against a
// silent invocation regression — e.g. the v1 --out-format=json flag (removed in
// v2) that made Analyse return zero findings for every file — and against the
// trailing run-summary golangci-lint writes after the JSON on stdout.
func TestIntegration_RealBinary(t *testing.T) {
	if _, err := exec.LookPath("golangci-lint"); err != nil {
		t.Skip("golangci-lint not on PATH")
	}

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module lintcheck\n\ngo 1.21\n")
	// fmt.Printf verb/argument mismatch — flagged by govet's printf analyser,
	// which is in golangci-lint's default linter set.
	src := filepath.Join(dir, "main.go")
	writeFile(t, src, "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Printf(\"%d\", \"not a number\")\n}\n")

	// golangci-lint resolves the module from the working directory.
	t.Chdir(dir)

	findings, err := golangcilint.New().Analyse(context.Background(), []string{src})
	if err != nil {
		t.Fatalf("Analyse returned error: %v", err)
	}
	if len(findings) == 0 {
		t.Fatal("expected at least one finding from the real golangci-lint binary, got none (flag or output-parsing regression?)")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
