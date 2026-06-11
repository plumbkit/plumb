package cli

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
)

// detectTestPoolWeak builds a pool with go (strong: go.mod) and typescript
// (strong: tsconfig.json; weak: package.json), to exercise the strong/weak
// marker split in workspace detection.
func detectTestPoolWeak() *workspacePool {
	return &workspacePool{
		entries:  make(map[poolKey]*poolEntry),
		baseCtx:  context.Background(),
		cacheTTL: time.Minute,
		langs: []langConfig{
			{name: "go", cfg: config.LSPConfig{RootMarkers: []string{"go.mod"}, Enabled: true}},
			{name: "typescript", cfg: config.LSPConfig{
				RootMarkers:     []string{"tsconfig.json"},
				WeakRootMarkers: []string{"package.json"},
				Enabled:         true,
			}},
		},
	}
}

// TestDetect_WeakMarkerAtStartDir: a directory whose only signal is a weak
// marker (package.json) resolves as that language — a real JS/TS project root.
func TestDetect_WeakMarkerAtStartDir(t *testing.T) {
	dir := freshTempDir(t)
	mustWrite(t, filepath.Join(dir, "package.json"), "{}")

	_, lang, err := detectTestPoolWeak().Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if lang != "typescript" {
		t.Errorf("language: got %s, want typescript (package.json names the dir it sits in)", lang)
	}
}

// TestDetect_WeakMarkerAtGitRoot: a weak marker at a .git boundary upgrades the
// otherwise language-less result, so a JS repo attached from a subdir resolves.
func TestDetect_WeakMarkerAtGitRoot(t *testing.T) {
	root := freshTempDir(t)
	mustMkdir(t, filepath.Join(root, ".git"))
	mustWrite(t, filepath.Join(root, "package.json"), "{}")
	sub := filepath.Join(root, "src", "components")
	mustMkdir(t, sub)

	gotRoot, lang, err := detectTestPoolWeak().Detect(sub)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if gotRoot != root {
		t.Errorf("root: got %s, want %s", gotRoot, root)
	}
	if lang != "typescript" {
		t.Errorf("language: got %s, want typescript", lang)
	}
}

// TestDetect_StrongBeatsWeak: a strong marker at the same directory wins over a
// weak one, so a Go repo carrying a tooling package.json stays Go.
func TestDetect_StrongBeatsWeak(t *testing.T) {
	dir := freshTempDir(t)
	mustWrite(t, filepath.Join(dir, "go.mod"), "module test\n")
	mustWrite(t, filepath.Join(dir, "package.json"), "{}")

	_, lang, err := detectTestPoolWeak().Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if lang != "go" {
		t.Errorf("language: got %s, want go (a strong marker must beat a weak package.json)", lang)
	}
}

// TestDetect_WeakMarkerInAncestorIgnored: a weak marker above a nearer .git
// boundary never leaks down — the inner repo resolves as LanguageNone, not the
// ancestor's weak language. This is the hijack the demotion prevents.
func TestDetect_WeakMarkerInAncestorIgnored(t *testing.T) {
	outer := freshTempDir(t)
	mustWrite(t, filepath.Join(outer, "package.json"), "{}") // stray tooling package.json up the tree
	inner := filepath.Join(outer, "service")
	mustMkdir(t, inner)
	mustMkdir(t, filepath.Join(inner, ".git"))

	gotRoot, lang, err := detectTestPoolWeak().Detect(inner)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if gotRoot != inner {
		t.Errorf("root: got %s, want %s", gotRoot, inner)
	}
	if lang != LanguageNone {
		t.Errorf("language: got %s, want %s (an ancestor package.json must not hijack the inner repo)", lang, LanguageNone)
	}
}

// TestDetect_PlumbRootWeakMarkerAtRoot: a .plumb workspace whose root carries a
// weak marker resolves as that language.
func TestDetect_PlumbRootWeakMarkerAtRoot(t *testing.T) {
	dir := freshTempDir(t)
	mustMkdir(t, filepath.Join(dir, ".plumb"))
	mustWrite(t, filepath.Join(dir, "package.json"), "{}")

	_, lang, err := detectTestPoolWeak().Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if lang != "typescript" {
		t.Errorf("language: got %s, want typescript", lang)
	}
}

// TestDetect_PlumbRootWeakMarkerInSubdirIgnored is the NoCaps repro: a .plumb
// workspace with no marker of its own and a package.json only in a SUBdirectory
// must resolve as LanguageNone — the weak marker names only the dir it sits in,
// never the workspace root above it.
func TestDetect_PlumbRootWeakMarkerInSubdirIgnored(t *testing.T) {
	dir := freshTempDir(t)
	mustMkdir(t, filepath.Join(dir, ".plumb"))
	sub := filepath.Join(dir, ".vscode")
	mustMkdir(t, sub)
	mustWrite(t, filepath.Join(sub, "package.json"), "{}")

	_, lang, err := detectTestPoolWeak().Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if lang != LanguageNone {
		t.Errorf("language: got %s, want %s (a package.json in a subdir must not name the workspace)", lang, LanguageNone)
	}
}
