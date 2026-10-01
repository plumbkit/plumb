//go:build integration

package cli

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
)

// TestIntegration_PoolGoplsWorktreeUnderGoWork starts gopls through the pool for
// both roots of the #521 layout — a main checkout listed in an enclosing go.work,
// and a worktree of the same module inside it — and asks each for a symbol that
// exists in BOTH copies. Each server must answer from its own root's copy: the
// worktree's through GOWORK=off, the main checkout's through the workspace it
// is listed in (the direction that must keep working).
//
// Before the fix the worktree's gopls resolved the enclosing go.work, in which
// the worktree is not a module, and answered from the main checkout or not at
// all.
func TestIntegration_PoolGoplsWorktreeUnderGoWork(t *testing.T) {
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not on PATH")
	}
	isolateGoWorkEnv(t)
	mainDir, wt := goWorkWorktreeFixture(t)
	mustWrite(t, filepath.Join(mainDir, "a.go"), "package wt521\n\nfunc SharedIssue521() {}\n")
	mustWrite(t, filepath.Join(wt, "b.go"), "package wt521\n\nfunc SharedIssue521() {}\n")
	// A compile error in each copy: the diagnostics half of #521 was a "clean"
	// post-write pass over worktree code that did not build.
	for _, d := range []string{mainDir, wt} {
		mustWrite(t, filepath.Join(d, "c.go"), "package wt521\n\nimport \"os\"\n")
	}

	pool := &workspacePool{
		entries:   make(map[poolKey]*poolEntry),
		baseCtx:   context.Background(),
		cacheTTL:  time.Minute,
		idleGrace: time.Minute,
		langs: []langConfig{{name: "go", cfg: config.LSPConfig{
			Command:     "gopls",
			RootMarkers: []string{"go.mod", "go.work"},
			Enabled:     true,
		}}},
	}
	defer pool.close()

	for _, tc := range []struct {
		name, root, wantFile string
		wantGoWorkOff        bool
	}{
		{"worktree", wt, filepath.Join(wt, "b.go"), true},
		{"main checkout", mainDir, filepath.Join(mainDir, "a.go"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, err := pool.acquireLang(context.Background(), tc.root, "go", true)
			if err != nil {
				t.Fatalf("acquire: %v", err)
			}
			if off := pool.goWorkOffFor(tc.root, "go") != ""; off != tc.wantGoWorkOff {
				t.Fatalf("goWorkOffFor(%s) set = %v, want %v", tc.name, off, tc.wantGoWorkOff)
			}
			waitEntryReady(t, e, 30*time.Second)
			got := waitWorkspaceSymbolFiles(t, e, "SharedIssue521", 30*time.Second)
			if len(got) != 1 || got[0] != tc.wantFile {
				t.Fatalf("workspace/symbol SharedIssue521 from the %s root = %v, want exactly [%s]", tc.name, got, tc.wantFile)
			}
			waitUnusedImportDiagnostic(t, e, filepath.Join(tc.root, "c.go"), 30*time.Second)
		})
	}
}

// waitUnusedImportDiagnostic waits for the server to publish the unused-import
// error in file, which it does only for a file in a module it loaded.
func waitUnusedImportDiagnostic(t *testing.T, e *poolEntry, file string, within time.Duration) {
	t.Helper()
	uri := protocol.FileURI(file)
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		for _, d := range e.inv.Diagnostics(uri) {
			if strings.Contains(d.Message, "imported and not used") {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no unused-import diagnostic published for %s: the server did not load that copy of the module", file)
}

// waitWorkspaceSymbolFiles polls workspace/symbol until it returns an exact-name
// match (gopls loads the workspace asynchronously after initialized) and returns
// the files the matches are in. An empty result at the deadline is returned, not
// fatal, so the caller's assertion names what the server did answer.
func waitWorkspaceSymbolFiles(t *testing.T, e *poolEntry, name string, within time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		syms, err := e.proxy.get().WorkspaceSymbols(context.Background(), protocol.WorkspaceSymbolParams{Query: name})
		var files []string
		for _, s := range syms {
			if s.Name == name {
				files = append(files, paths.URIToPath(s.Location.URI))
			}
		}
		if (err == nil && len(files) > 0) || time.Now().After(deadline) {
			return files
		}
		time.Sleep(100 * time.Millisecond)
	}
}
