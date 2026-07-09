package cli

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
)

func goOnlyPool() *workspacePool {
	return &workspacePool{
		entries:  make(map[poolKey]*poolEntry),
		baseCtx:  context.Background(),
		cacheTTL: time.Minute,
		langs: []langConfig{
			{name: "go", cfg: config.LSPConfig{RootMarkers: []string{"go.mod"}, Enabled: true}},
		},
	}
}

// TestExtLangAt_PythonAtRoot: a directory of .py files sniffs as Python when
// pyright is active — the reported gitlab/ism-app case (a repo with no manifest).
func TestExtLangAt_PythonAtRoot(t *testing.T) {
	dir := freshTempDir(t)
	mustWrite(t, filepath.Join(dir, "app.py"), "print('hi')\n")
	if lang := detectTestPool().extLangAt(dir); lang != "python" {
		t.Errorf("extLangAt: got %q, want python", lang)
	}
}

// TestExtLangAt_PythonInSubdir: the sniff descends into subdirectories, so a
// repo whose .py files live under src/ still resolves as Python.
func TestExtLangAt_PythonInSubdir(t *testing.T) {
	dir := freshTempDir(t)
	mustWrite(t, filepath.Join(dir, "src", "pkg", "mod.py"), "x = 1\n")
	if lang := detectTestPool().extLangAt(dir); lang != "python" {
		t.Errorf("extLangAt: got %q, want python", lang)
	}
}

// TestExtLangAt_InactiveLanguageEmpty: the sniff only fires for an ACTIVE
// language. A pool without Python returns "" for a .py directory.
func TestExtLangAt_InactiveLanguageEmpty(t *testing.T) {
	dir := freshTempDir(t)
	mustWrite(t, filepath.Join(dir, "app.py"), "print('hi')\n")
	if lang := goOnlyPool().extLangAt(dir); lang != "" {
		t.Errorf("extLangAt: got %q, want \"\" (python not active)", lang)
	}
}

// TestExtLangAt_DepthBound: a source file buried below extScanDepth is not
// found.
func TestExtLangAt_DepthBound(t *testing.T) {
	dir := freshTempDir(t)
	// dir/a/b/c/deep.py is 3 levels below the root; extScanDepth is 2.
	mustWrite(t, filepath.Join(dir, "a", "b", "c", "deep.py"), "x = 1\n")
	if lang := detectTestPool().extLangAt(dir); lang != "" {
		t.Errorf("extLangAt: got %q, want \"\" (below depth bound)", lang)
	}
}

// TestExtLangAt_DominantLanguageWins: with more .py than .go files, Python wins;
// on an equal count the deterministic order puts Go first.
func TestExtLangAt_DominantLanguageWins(t *testing.T) {
	many := freshTempDir(t)
	mustWrite(t, filepath.Join(many, "a.py"), "")
	mustWrite(t, filepath.Join(many, "b.py"), "")
	mustWrite(t, filepath.Join(many, "main.go"), "package main\n")
	if lang := detectTestPool().extLangAt(many); lang != "python" {
		t.Errorf("extLangAt (2 py, 1 go): got %q, want python", lang)
	}

	tie := freshTempDir(t)
	mustWrite(t, filepath.Join(tie, "a.py"), "")
	mustWrite(t, filepath.Join(tie, "main.go"), "package main\n")
	if lang := detectTestPool().extLangAt(tie); lang != "go" {
		t.Errorf("extLangAt (1 py, 1 go tie): got %q, want go (go-first)", lang)
	}
}

// TestDetect_GitRepoWithPyStaysNoneAtDetect locks the architecture: Detect walks
// up only and never content-sniffs — a .py git repo resolves as LanguageNone at
// Detect, and the content sniff happens later, at attach, after child discovery.
func TestDetect_GitRepoWithPyStaysNoneAtDetect(t *testing.T) {
	dir := freshTempDir(t)
	mustMkdir(t, filepath.Join(dir, ".git"))
	mustWrite(t, filepath.Join(dir, "app.py"), "print('hi')\n")
	_, lang, err := detectTestPool().Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if lang != LanguageNone {
		t.Errorf("Detect language: got %q, want %s (sniff must not run in Detect)", lang, LanguageNone)
	}
}
