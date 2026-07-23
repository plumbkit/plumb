package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// blockerChild returns a path whose parent is a regular file, so safeWrite's
// MkdirAll(parent) fails deterministically — the injection point for the
// second-write-fails rollback tests.
func blockerChild(t *testing.T, dir string) string {
	t.Helper()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(blocker, "child.go")
}

func TestApplyMovePlans_RollsBackExistingOnSecondWriteFailure(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.go")
	if err := os.WriteFile(first, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plans := []movePlan{
		{path: first, before: []byte("original\n"), after: []byte("changed\n"), mode: 0o644, existedBefore: true},
		{path: blockerChild(t, dir), after: []byte("new\n"), mode: 0o644, existedBefore: false},
	}
	applied := false
	if _, err := applyMovePlans(plans, func() { applied = true }); err == nil {
		t.Fatal("expected the second write to fail")
	}
	if applied {
		t.Error("onApplied ran despite a failed write")
	}
	if got, _ := os.ReadFile(first); string(got) != "original\n" {
		t.Errorf("first file not rolled back to its pre-move bytes: %q", got)
	}
}

func TestApplyMovePlans_RemovesCreatedFileOnRollback(t *testing.T) {
	dir := t.TempDir()
	created := filepath.Join(dir, "created.go")
	plans := []movePlan{
		{path: created, after: []byte("package x\n"), mode: 0o644, existedBefore: false},
		{path: blockerChild(t, dir), after: []byte("new\n"), mode: 0o644, existedBefore: false},
	}
	if _, err := applyMovePlans(plans, nil); err == nil {
		t.Fatal("expected the second write to fail")
	}
	if _, err := os.Stat(created); !os.IsNotExist(err) {
		t.Errorf("created file should have been removed on rollback, stat err=%v", err)
	}
}

func TestApplyMovePlans_HappyPathWritesBothAndRunsOnApplied(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.go")
	b := filepath.Join(dir, "b.go")
	if err := os.WriteFile(a, []byte("old-a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plans := []movePlan{
		{path: a, before: []byte("old-a\n"), after: []byte("new-a\n"), mode: 0o644, existedBefore: true},
		{path: b, after: []byte("new-b\n"), mode: 0o644, existedBefore: false},
	}
	ran := false
	modified, err := applyMovePlans(plans, func() { ran = true })
	if err != nil {
		t.Fatalf("applyMovePlans: %v", err)
	}
	if !ran {
		t.Error("onApplied did not run after a successful move")
	}
	if len(modified) != 2 {
		t.Errorf("want 2 modified paths, got %v", modified)
	}
	if got, _ := os.ReadFile(a); string(got) != "new-a\n" {
		t.Errorf("a not written: %q", got)
	}
	if got, _ := os.ReadFile(b); string(got) != "new-b\n" {
		t.Errorf("b not created: %q", got)
	}
}

func TestAppendDeclaration(t *testing.T) {
	tests := []struct {
		name    string
		dest    string
		decl    string
		seed    string
		existed bool
		want    string
	}{
		{"created uses seed", "", "func F(){}", "package p\n\n", false, "package p\n\nfunc F(){}\n"},
		{"existing gets blank-line separator", "package p\n\nfunc A(){}\n", "func F(){}\n", "", true, "package p\n\nfunc A(){}\n\nfunc F(){}\n"},
		{"existing without trailing newline", "x", "y", "", true, "x\n\ny\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(appendDeclaration([]byte(tc.dest), tc.decl, tc.seed, tc.existed)); got != tc.want {
				t.Errorf("appendDeclaration = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGoPackageClause(t *testing.T) {
	if got := goPackageClause([]byte("// header\npackage demo\n\nfunc F(){}\n")); got != "package demo" {
		t.Errorf("got %q, want %q", got, "package demo")
	}
	if got := goPackageClause([]byte("func F(){}\n")); got != "" {
		t.Errorf("want empty for a file with no package clause, got %q", got)
	}
}

func TestNormalizeRemovalSeam(t *testing.T) {
	tests := []struct {
		name  string
		after string // the already-deleted text, seam marked by a '|'
		want  string
	}{
		{
			name:  "collapses 4 newlines at the seam to 2",
			after: "package demo\n\n|\n\nfunc Bar() int { return 2 }\n",
			want:  "package demo\n\nfunc Bar() int { return 2 }\n",
		},
		{
			name:  "collapses 3 newlines at the seam to 2",
			after: "package demo\n\n|\nfunc Bar() int {}\n",
			want:  "package demo\n\nfunc Bar() int {}\n",
		},
		{
			name:  "leaves a single newline at the seam untouched",
			after: "package demo\n|func Bar() int {}\n",
			want:  "package demo\nfunc Bar() int {}\n",
		},
		{
			name:  "leaves two newlines (one blank line) at the seam untouched",
			after: "package demo\n\n|func Bar() int {}\n",
			want:  "package demo\n\nfunc Bar() int {}\n",
		},
		{
			name:  "no newlines border the seam",
			after: "package demo\n\nfunc| Bar() int {}\n",
			want:  "package demo\n\nfunc Bar() int {}\n",
		},
		{
			name:  "trims to a single trailing newline when the removed decl was last",
			after: "package demo\n\nfunc Foo() int { return 1 }\n\n|\n",
			want:  "package demo\n\nfunc Foo() int { return 1 }\n",
		},
		{
			name:  "empty file after removal stays empty",
			after: "\n\n|",
			want:  "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			seam := strings.IndexByte(tc.after, '|')
			if seam < 0 {
				t.Fatalf("test fixture missing seam marker '|': %q", tc.after)
			}
			after := tc.after[:seam] + tc.after[seam+1:]
			if got := string(normalizeRemovalSeam([]byte(after), seam)); got != tc.want {
				t.Errorf("normalizeRemovalSeam(%q, %d) = %q, want %q", after, seam, got, tc.want)
			}
		})
	}
}

func TestGoBuildConstraints(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []string
	}{
		{"none", "package demo\n\nfunc F(){}\n", nil},
		{"go:build", "//go:build linux\n\npackage demo\n\nfunc F(){}\n", []string{"//go:build linux"}},
		{"legacy +build", "// +build linux,amd64\n\npackage demo\n\nfunc F(){}\n", []string{"// +build linux,amd64"}},
		{"both forms", "//go:build linux\n// +build linux\n\npackage demo\n\nfunc F(){}\n", []string{"//go:build linux", "// +build linux"}},
		{"ignores a matching comment after package", "package demo\n\n// +build not-a-constraint-here\nfunc F(){}\n", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := goBuildConstraints([]byte(tc.src))
			if len(got) != len(tc.want) {
				t.Fatalf("goBuildConstraints(%q) = %v, want %v", tc.src, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("goBuildConstraints(%q)[%d] = %q, want %q", tc.src, i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestCheckGoBuildTags(t *testing.T) {
	src := []byte("//go:build linux\n\npackage demo\n\nfunc Foo(){}\n")
	sameTags := []byte("//go:build linux\n\npackage demo\n\nfunc Keep(){}\n")
	diffTags := []byte("//go:build darwin\n\npackage demo\n\nfunc Keep(){}\n")
	noTags := []byte("package demo\n\nfunc Keep(){}\n")

	if err := checkGoBuildTags("src.go", "dst.go", src, sameTags); err != nil {
		t.Errorf("identical build tags should pass, got: %v", err)
	}
	if err := checkGoBuildTags("src.go", "dst.go", noTags, noTags); err != nil {
		t.Errorf("both-absent build tags should pass, got: %v", err)
	}
	if err := checkGoBuildTags("src.go", "dst.go", src, diffTags); err == nil {
		t.Error("want refusal for differing build tags, got nil")
	}
	if err := checkGoBuildTags("src.txt", "dst.txt", src, diffTags); err != nil {
		t.Errorf("non-Go files should skip the build-tag check, got: %v", err)
	}
}

func TestFilenameConstraints(t *testing.T) {
	tests := []struct {
		name string
		want filenameSig
	}{
		{"plain.go", filenameSig{}},
		{"linux.go", filenameSig{}}, // the whole name matching a GOOS is NOT a constraint
		{"amd64.go", filenameSig{}}, // same for GOARCH
		{"handlers_linux.go", filenameSig{goos: "linux"}},
		{"handlers_darwin.go", filenameSig{goos: "darwin"}},
		{"handlers_amd64.go", filenameSig{goarch: "amd64"}},
		{"handlers_linux_amd64.go", filenameSig{goos: "linux", goarch: "amd64"}},
		{"foo_test.go", filenameSig{test: true}},
		{"foo_linux_test.go", filenameSig{goos: "linux", test: true}},
		{"foo_linux_amd64_test.go", filenameSig{goos: "linux", goarch: "amd64", test: true}},
		{"linux_test.go", filenameSig{test: true}}, // "linux" alone (after stripping _test) is still the whole name — no GOOS constraint
		{"foo_notanos.go", filenameSig{}},          // unrecognised component: no constraint
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := filenameConstraints(tc.name); got != tc.want {
				t.Errorf("filenameConstraints(%q) = %+v, want %+v", tc.name, got, tc.want)
			}
		})
	}
}

func TestCheckGoBuildTags_FilenameConstraints(t *testing.T) {
	plain := []byte("package demo\n\nfunc Foo(){}\n")

	if err := checkGoBuildTags("handlers_linux.go", "handlers_darwin.go", plain, plain); err == nil {
		t.Error("want refusal moving between different GOOS-suffixed files, got nil")
	}
	if err := checkGoBuildTags("foo.go", "foo_test.go", plain, plain); err == nil {
		t.Error("want refusal moving a production declaration into a _test.go file, got nil")
	}
	if err := checkGoBuildTags("foo_linux.go", "bar_linux.go", plain, plain); err != nil {
		t.Errorf("same GOOS suffix on both sides should pass, got: %v", err)
	}
	if err := checkGoBuildTags("plain.go", "other.go", plain, plain); err != nil {
		t.Errorf("neither file GOOS/GOARCH/test-suffixed should pass, got: %v", err)
	}

	// Comment-vs-filename asymmetry: a plain file carrying an explicit
	// //go:build constraint moved into a filename-suffixed file expressing an
	// arguably equivalent restriction. checkGoBuildTags does NOT attempt to
	// prove that equivalence — see its doc comment — so this refuses.
	commentOnly := []byte("//go:build linux\n\npackage demo\n\nfunc Foo(){}\n")
	if err := checkGoBuildTags("foo.go", "foo_linux.go", commentOnly, plain); err == nil {
		t.Error("want refusal for comment-vs-filename constraint asymmetry (by design — no cross-axis equivalence check), got nil")
	}
}
