//go:build integration

package smoke_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// stubServerDeadline is the [lsp_query] timeout this scenario pins, standing in
// for the 30s default. Kept small so the test costs seconds rather than a
// minute; nothing here depends on its magnitude, only on the RELATIONSHIPS the
// assertions state against it.
const stubServerDeadline = 6 * time.Second

// buildStubLSP compiles the never-answers language server from testdata. It is
// built from an explicit file path because the go tool excludes testdata/ from
// package patterns by design — which is what keeps it out of the module build.
func buildStubLSP(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "stublsp")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", bin, filepath.Join("testdata", "stublsp", "main.go"))
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the stub language server: %v\n%s", err, out)
	}
	return bin
}

// TestColdLanguageServerStillAnswersReadSymbol is the end-to-end half of the
// PLAN-390 proof, and the reason this file exists rather than only a unit test:
// it drives the actual failure mode — a language server that finishes its
// handshake and then never answers documentSymbol — through a live daemon over
// the MCP wire, deterministically, instead of waiting for a loaded CI runner to
// reproduce it by luck.
//
// This is what a cold gopls loading a package graph looks like from plumb's
// side, and it is exactly the case read_symbol's tree-sitter fallback was
// written for. Before the fix the tool spent the entire [lsp_query] budget
// waiting, then invoked the fallback on that expired context — which cannot
// start a parse — and returned the timeout anyway. Two assertions pin the fix:
// the call SUCCEEDS from tree-sitter, and it does so STRICTLY INSIDE the
// server-side budget, so a client whose patience is that budget still gets an
// answer.
func TestColdLanguageServerStillAnswersReadSymbol(t *testing.T) {
	plumbBin := buildPlumb(t)
	stub := buildStubLSP(t)
	fixture := makeFixture(t)
	tmpHome := mkTmpHome(t)

	// The override goes in the GLOBAL config: a project file may not redirect a
	// language-server command, and should not (docs/project-config-trust.md).
	globalDir := filepath.Join(tmpHome, ".config", "plumb")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal("create global config dir:", err)
	}
	cfg := fmt.Sprintf("[lsp.go]\ncommand = %q\nargs = []\n\n[lsp_query]\ntimeout = %q\n",
		stub, stubServerDeadline.String())
	if err := os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal("write global config:", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), tierDTimeout+lspToolTimeout)
	defer cancel()

	c := newMCPClient(t, ctx, plumbBin, tmpHome, fixture)
	c.initialize(t, fixture)
	c.call(t, "session_start", map[string]any{"workspace": fixture}, sessionStartTimeout)

	mainGo := filepath.Join(fixture, "main.go")
	start := time.Now()
	out := c.call(t, "read_symbol", map[string]any{"path": mainGo, "name": "Greet"}, lspToolTimeout)
	elapsed := time.Since(start)

	if strings.Contains(out, "did not respond within") {
		t.Fatalf("a language server that is merely slow must degrade to the tree-sitter "+
			"fallback, not surface a timeout (after %v):\n%s", elapsed, out)
	}
	for _, want := range []string{"Greet", "fmt.Sprintf"} {
		if !strings.Contains(out, want) {
			t.Errorf("read_symbol did not return the symbol body — missing %q (after %v):\n%s",
				want, elapsed, out)
		}
	}
	if elapsed >= stubServerDeadline {
		t.Errorf("read_symbol answered after %v, at or past the whole %v [lsp_query] budget. "+
			"A caller whose patience equals that budget still never sees the fallback — "+
			"the PLAN-390 inversion.", elapsed, stubServerDeadline)
	}
}
