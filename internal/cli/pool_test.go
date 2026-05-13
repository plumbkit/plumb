package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/golimpio/plumb/internal/config"
)

// detectTestPool builds a workspacePool with Go and Python enabled, matching
// the default plumb configuration. Used by all Detect tests below.
func detectTestPool() *workspacePool {
	return &workspacePool{
		entries: make(map[string]*poolEntry),
		langs: []langConfig{
			{name: "go", cfg: config.LSPConfig{
				RootMarkers: []string{"go.mod"},
				Enabled:     true,
			}},
			{name: "python", cfg: config.LSPConfig{
				RootMarkers: []string{"pyproject.toml", "setup.py"},
				Enabled:     true,
			}},
		},
	}
}

// freshTempDir creates a temp directory under the OS-level $TMPDIR rather
// than via t.TempDir(). Reason: pool.Detect walks up to the filesystem root
// looking for ancestral markers. The plumb repo uses GOTMPDIR=.testcache to
// keep test binaries inside the project (Airlock Digital workaround), and
// go test's t.TempDir() honours GOTMPDIR — so a t.TempDir() lands inside the
// plumb source tree, and Detect finds plumb's own go.mod as an ancestor
// marker. Going via os.MkdirTemp("", …) bypasses GOTMPDIR and lands under
// /var/folders (macOS) / /tmp (Linux) where no Go module exists above.
func freshTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "plumb-detect-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestDetect_LanguageMarkerOnly(t *testing.T) {
	dir := freshTempDir(t)
	mustWrite(t, filepath.Join(dir, "go.mod"), "module test\n")

	pool := detectTestPool()
	root, lang, err := pool.Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if root != dir {
		t.Errorf("root: got %s, want %s", root, dir)
	}
	if lang != "go" {
		t.Errorf("language: got %s, want go", lang)
	}
}

func TestDetect_PlumbMarkerWithLanguage(t *testing.T) {
	dir := freshTempDir(t)
	mustMkdir(t, filepath.Join(dir, ".plumb"))
	mustWrite(t, filepath.Join(dir, "go.mod"), "module test\n")

	pool := detectTestPool()
	root, lang, err := pool.Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if root != dir {
		t.Errorf("root: got %s, want %s", root, dir)
	}
	if lang != "go" {
		t.Errorf("language: got %s, want go", lang)
	}
}

// TestDetect_PlumbMarkerWithoutLanguage is the regression test for the
// "TUI stuck on resolving" bug. A .plumb/ marker in a non-Go/non-Python
// project (e.g. a JavaScript repo) must still resolve so filesystem tools,
// stats attribution, and project config all keep working.
func TestDetect_PlumbMarkerWithoutLanguage(t *testing.T) {
	dir := freshTempDir(t)
	mustMkdir(t, filepath.Join(dir, ".plumb"))
	mustWrite(t, filepath.Join(dir, "package.json"), "{}")

	pool := detectTestPool()
	root, lang, err := pool.Detect(dir)
	if err != nil {
		t.Fatalf("Detect: unexpected error %v — .plumb/ alone should resolve as LanguageNone", err)
	}
	if root != dir {
		t.Errorf("root: got %s, want %s", root, dir)
	}
	if lang != LanguageNone {
		t.Errorf("language: got %s, want %s", lang, LanguageNone)
	}
}

func TestDetect_PlumbInParentNoLanguage(t *testing.T) {
	root := freshTempDir(t)
	sub := filepath.Join(root, "sub", "deep")
	mustMkdir(t, sub)
	mustMkdir(t, filepath.Join(root, ".plumb"))

	pool := detectTestPool()
	gotRoot, lang, err := pool.Detect(sub)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if gotRoot != root {
		t.Errorf("root: got %s, want %s", gotRoot, root)
	}
	if lang != LanguageNone {
		t.Errorf("language: got %s, want %s", lang, LanguageNone)
	}
}

// TestDetect_PlumbInChildWinsOverGoModInParent verifies the documented
// priority order: a `.plumb/` marker always beats a language marker found
// only in an ancestor, even when the ancestor has Go.
func TestDetect_PlumbInChildWinsOverGoModInParent(t *testing.T) {
	parent := freshTempDir(t)
	mustWrite(t, filepath.Join(parent, "go.mod"), "module test\n")
	child := filepath.Join(parent, "sub")
	mustMkdir(t, child)
	mustMkdir(t, filepath.Join(child, ".plumb"))

	pool := detectTestPool()
	gotRoot, lang, err := pool.Detect(child)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if gotRoot != child {
		t.Errorf("root: got %s, want %s (child should win)", gotRoot, child)
	}
	// Language detection at child walks up to parent and finds go.mod.
	if lang != "go" {
		t.Errorf("language: got %s, want go (go.mod is in ancestor)", lang)
	}
}

func TestDetect_NothingFound(t *testing.T) {
	// Use a fresh tmpdir tree with no markers anywhere up to the FS root.
	// Strictly speaking the FS root could have a marker; in practice TempDir
	// is somewhere under /tmp or /var/folders, neither of which has a go.mod
	// or .plumb on any normal dev machine. If this test ever flakes, the
	// machine has bigger problems.
	dir := freshTempDir(t)

	pool := detectTestPool()
	_, _, err := pool.Detect(dir)
	if err == nil {
		t.Fatal("Detect: want error, got nil")
	}
}

// mustWrite writes data to path, creating parent dirs as needed.
func mustWrite(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}
