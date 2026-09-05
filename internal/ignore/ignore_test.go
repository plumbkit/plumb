package ignore

import (
	"os"
	"path/filepath"
	"testing"
)

// ── DoubleStarMatch ──────────────────────────────────────────────────────────

func TestDoubleStarMatch(t *testing.T) {
	cases := []struct {
		pattern string
		name    string
		want    bool
	}{
		// Simple globs (no **)
		{"*.go", "foo.go", true},
		{"*.go", "foo.ts", false},
		{"foo", "foo", true},
		{"foo", "bar", false},

		// ** prefix
		{"**/*.go", "foo.go", true},
		{"**/*.go", "a/b/foo.go", true},
		{"**/*.go", "a/foo.go", true},
		{"**/*.go", "foo.ts", false},

		// Trailing **
		{"vendor/**", "vendor/foo.go", true},
		{"vendor/**", "vendor/a/b/c.go", true},
		{"vendor/**", "notvendor/foo.go", false},

		// Middle **
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/b", true},
		{"a/**/b", "a/x/y/b", true},
		{"a/**/b", "c/b", false},

		// Exact match passthrough
		{"vendor", "vendor", true},
		{"vendor", "notvendor", false},
	}

	for _, tc := range cases {
		got := DoubleStarMatch(tc.pattern, tc.name)
		if got != tc.want {
			t.Errorf("DoubleStarMatch(%q, %q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}

// ── parseLine ──────────────────────────────────────────────────────────

func TestParseIgnoreLine(t *testing.T) {
	cases := []struct {
		raw      string
		wantOk   bool
		negate   bool
		dirOnly  bool
		rooted   bool
		hasSlash bool
		glob     string
	}{
		{"", false, false, false, false, false, ""},
		{"# comment", false, false, false, false, false, ""},
		{"*.log", true, false, false, false, false, "*.log"},
		{"!important.log", true, true, false, false, false, "important.log"},
		{"vendor/", true, false, true, false, false, "vendor"},
		{"/build", true, false, false, true, false, "build"},
		{"docs/api", true, false, false, false, true, "docs/api"},
		{"**/*.go", true, false, false, false, true, "**/*.go"},
	}

	for _, tc := range cases {
		p, ok := parseLine(tc.raw)
		if ok != tc.wantOk {
			t.Errorf("parseLine(%q): ok=%v want %v", tc.raw, ok, tc.wantOk)
			continue
		}
		if !ok {
			continue
		}
		if p.negate != tc.negate || p.dirOnly != tc.dirOnly || p.rooted != tc.rooted ||
			p.hasSlash != tc.hasSlash || p.glob != tc.glob {
			t.Errorf("parseLine(%q) = %+v, want negate=%v dirOnly=%v rooted=%v hasSlash=%v glob=%q",
				tc.raw, p, tc.negate, tc.dirOnly, tc.rooted, tc.hasSlash, tc.glob)
		}
	}
}

// ── Stack.IsIgnored ────────────────────────────────────────────────────

func TestIgnoreStack(t *testing.T) {
	dir := t.TempDir()

	// Write root .gitignore.
	gitignore := "*.log\nvendor/\n/build\n"
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(gitignore), 0o644); err != nil {
		t.Fatal(err)
	}

	var st Stack
	st = st.Load(dir)

	cases := []struct {
		rel   string
		isDir bool
		want  bool
	}{
		{"foo.log", false, true},
		{"foo.go", false, false},
		{"vendor", true, true},
		{"vendor", false, false},   // vendor/ only matches dirs
		{"build", false, true},     // /build matches file or dir at root
		{"a/build", false, false},  // rooted — doesn't match subdir
		{"a/foo.log", false, true}, // *.log matches anywhere
	}

	for _, tc := range cases {
		abs := filepath.Join(dir, filepath.FromSlash(tc.rel))
		got := st.IsIgnored(abs, tc.isDir)
		if got != tc.want {
			t.Errorf("IsIgnored(%q, isDir=%v) = %v, want %v", tc.rel, tc.isDir, got, tc.want)
		}
	}
}

// TestIgnoreStack_ChildNegationOverridesParent pins the rule the Stack
// type comment has always claimed and never implemented: rules from parent
// directories are inherited, and a child directory can override them. A parent
// ignoring *.py must not make a child's !keep.py unreachable.
func TestIgnoreStack_ChildNegationOverridesParent(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.py\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, ".gitignore"), []byte("!keep.py\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var st Stack
	st = st.Load(dir)
	st = st.Load(sub)

	if got := st.IsIgnored(filepath.Join(sub, "keep.py"), false); got {
		t.Error("sub/keep.py: child !keep.py must re-include it, got ignored")
	}
	if got := st.IsIgnored(filepath.Join(sub, "other.py"), false); !got {
		t.Error("sub/other.py: parent *.py still applies, got not ignored")
	}
	if got := st.IsIgnored(filepath.Join(dir, "keep.py"), false); !got {
		t.Error("keep.py at root: the child's negation must not reach up, got not ignored")
	}
}

// TestIgnoreStack_ExcludedParentCannotBeReincluded pins the other half of
// gitignore(5): once a directory is excluded, a negation inside it has nothing
// to apply to, because git never descends there to read it.
func TestIgnoreStack_ExcludedParentCannotBeReincluded(t *testing.T) {
	dir := t.TempDir()
	build := filepath.Join(dir, "build")
	if err := os.MkdirAll(build, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("build/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(build, ".gitignore"), []byte("!keep.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var st Stack
	st = st.Load(dir)
	st = st.Load(build)

	if got := st.IsIgnored(build, true); !got {
		t.Fatal("build/ itself must be ignored")
	}
	if got := st.IsIgnored(filepath.Join(build, "keep.txt"), false); !got {
		t.Error("build/keep.txt: an excluded directory cannot be re-included from inside, got not ignored")
	}
}

// TestIgnoreStack_PathOutsideSetDirIsNotMatched guards the widened lookup: now
// that every set in the stack is consulted rather than only the first to match,
// a set must still only speak for paths beneath its own directory. Without the
// containment check a sibling file matches a deeper set's base-name pattern.
func TestIgnoreStack_PathOutsideSetDirIsNotMatched(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, ".gitignore"), []byte("*.log\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var st Stack
	st = st.Load(dir)
	st = st.Load(sub)

	if got := st.IsIgnored(filepath.Join(dir, "outside.log"), false); got {
		t.Error("outside.log sits above sub/, so sub/.gitignore must not reach it")
	}
	if got := st.IsIgnored(filepath.Join(sub, "inside.log"), false); !got {
		t.Error("sub/inside.log must still be ignored by sub/.gitignore")
	}
}

// TestGitignore_BracesStayLiteral guards the blast radius of the tools
// package's brace expansion: .gitignore has no brace syntax, so expansion must
// NOT have leaked into ignore matching. A gitignore line "*.{go,md}" ignores
// only a file literally named that. The package split now makes the leak
// structurally impossible — expansion lives in internal/tools and this package
// cannot see it — but the assertion is the one that would fail first if a
// future change moved brace handling down here.
func TestGitignore_BracesStayLiteral(t *testing.T) {
	p := pattern{glob: "*.{go,md}"}
	if p.matchesPath("a.go", false) {
		t.Error("gitignore brace pattern must not expand: a.go was ignored")
	}
	if !p.matchesPath("a.{go,md}", false) {
		t.Error("gitignore brace pattern must match the literal name")
	}
}
