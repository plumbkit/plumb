package cli

import (
	"os"
	"path/filepath"
	"strings"
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

func TestSynthesiseRoot_GitDirAtSeed(t *testing.T) {
	dir := freshTempDir(t)
	mustMkdir(t, filepath.Join(dir, ".git"))

	pool := detectTestPool()
	got := pool.SynthesiseRoot(dir)
	if got != dir {
		t.Errorf("SynthesiseRoot: got %s, want %s", got, dir)
	}
}

func TestSynthesiseRoot_GitDirInAncestor(t *testing.T) {
	root := freshTempDir(t)
	mustMkdir(t, filepath.Join(root, ".git"))
	sub := filepath.Join(root, "pkg", "foo")
	mustMkdir(t, sub)

	pool := detectTestPool()
	got := pool.SynthesiseRoot(sub)
	if got != root {
		t.Errorf("SynthesiseRoot: got %s, want %s", got, root)
	}
}

// TestSynthesiseRoot_NoGitFallsBackToSeed verifies the fallback: when no .git/
// exists anywhere up the tree, the seed directory itself is returned.
func TestSynthesiseRoot_NoGitFallsBackToSeed(t *testing.T) {
	// Build a subtree under os.MkdirTemp to stay away from any .git above.
	base := freshTempDir(t)
	sub := filepath.Join(base, "a", "b")
	mustMkdir(t, sub)

	pool := detectTestPool()
	got := pool.SynthesiseRoot(sub)
	// The seed is sub; no .git anywhere above it in the temp tree.
	if got != sub {
		t.Errorf("SynthesiseRoot: got %s, want %s (seed fallback)", got, sub)
	}
}

func TestCurrentEnvPATH(t *testing.T) {
	tests := []struct {
		name string
		env  []string
		want string
	}{
		{"present", []string{"HOME=/root", "PATH=/usr/bin:/bin", "USER=x"}, "/usr/bin:/bin"},
		{"absent", []string{"HOME=/root", "USER=x"}, ""},
		{"empty value", []string{"PATH="}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := currentEnvPATH(tc.env); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAugmentedPATH_PreservesExistingOrder(t *testing.T) {
	current := "/custom/bin:/usr/bin:/bin"
	result := augmentedPATH(current)
	entries := filepath.SplitList(result)
	if len(entries) < 3 {
		t.Fatalf("expected at least 3 entries, got %d: %v", len(entries), entries)
	}
	if entries[0] != "/custom/bin" {
		t.Errorf("first entry: got %q, want /custom/bin", entries[0])
	}
	if entries[1] != "/usr/bin" {
		t.Errorf("second entry: got %q, want /usr/bin", entries[1])
	}
}

func TestAugmentedPATH_DeduplicatesEntries(t *testing.T) {
	current := "/usr/local/bin:/usr/bin"
	result := augmentedPATH(current)
	entries := filepath.SplitList(result)
	seen := make(map[string]int)
	for _, e := range entries {
		seen[e]++
	}
	for p, count := range seen {
		if count > 1 {
			t.Errorf("duplicate entry %q appears %d times", p, count)
		}
	}
}

func TestAugmentedPATH_AppendsHomebrewPaths(t *testing.T) {
	// Start from a PATH that lacks Homebrew dirs entirely.
	result := augmentedPATH("/usr/bin:/bin")
	entries := filepath.SplitList(result)
	set := make(map[string]bool, len(entries))
	for _, e := range entries {
		set[e] = true
	}
	for _, want := range []string{"/usr/local/bin", "/opt/homebrew/bin"} {
		if !set[want] {
			t.Errorf("expected %q in augmented PATH, got %v", want, entries)
		}
	}
}

func TestAugmentedPATH_EmptyInput(t *testing.T) {
	result := augmentedPATH("")
	if result == "" {
		t.Error("augmentedPATH(\"\") returned empty string; expected at least Homebrew paths")
	}
	if !strings.Contains(result, "/usr/local/bin") {
		t.Errorf("expected /usr/local/bin in result, got %q", result)
	}
}

func TestEnvFor_AlwaysSetsPATH(t *testing.T) {
	cfg := config.LSPConfig{} // no overrides
	env := envFor(cfg)
	if env == nil {
		t.Fatal("envFor returned nil; expected explicit env slice with PATH set")
	}
	path := currentEnvPATH(env)
	if path == "" {
		t.Error("PATH not set in envFor result")
	}
	if !strings.Contains(path, "/usr/local/bin") {
		t.Errorf("PATH does not contain /usr/local/bin: %q", path)
	}
}

func TestEnvFor_ConfigOverrideWins(t *testing.T) {
	cfg := config.LSPConfig{
		Env: map[string]string{"PATH": "/my/custom/bin"},
	}
	env := envFor(cfg)
	path := currentEnvPATH(env)
	if path != "/my/custom/bin" {
		t.Errorf("PATH: got %q, want /my/custom/bin", path)
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
