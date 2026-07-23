package tools

import (
	"os"
	"path/filepath"
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
