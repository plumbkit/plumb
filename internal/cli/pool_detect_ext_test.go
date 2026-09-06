package cli

import (
	"context"
	"fmt"
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

// TestFileLanguage_RecognisedButUnservedFileCastsNoVote pins the boundary
// between the two registries the sniff sits between. langsupport recognises
// .svelte (that is what lets file_outline and the topology census say something
// true about it), but the sniff counts votes for LANGUAGE SERVERS, and plumb
// configures none that serves a Svelte single-file component. So the file is
// known and still casts no vote — adding the langsupport row must not quietly
// enrol it in some other server's tally.
//
// The day plumb ships a Svelte adapter, or normaliseLangName folds svelte into
// an existing one, this assertion is the thing that has to be revisited on
// purpose rather than discovered afterwards.
func TestFileLanguage_RecognisedButUnservedFileCastsNoVote(t *testing.T) {
	pool := defaultsPool(t, "python", "typescript", "html")
	for _, name := range []string{"App.svelte", "Card.vue"} {
		if got := pool.fileLanguage(name); got != "" {
			t.Errorf("fileLanguage(%q) = %q, want \"\" — no configured server owns it", name, got)
		}
	}

	dir := freshTempDir(t)
	mustWrite(t, filepath.Join(dir, "app.py"), "x = 1\n")
	for i := range 20 {
		mustWrite(t, filepath.Join(dir, "src", fmt.Sprintf("C%02d.svelte", i)), "<script></script>\n")
	}
	if got := pool.extLangAt(dir); got != "python" {
		t.Errorf("extLangAt = %q, want python — 20 .svelte files are recognised but "+
			"serve no language server, so the one .py file is the only vote cast", got)
	}
}
