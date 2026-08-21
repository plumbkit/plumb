package setup_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/setup"
)

const testBody = "line one\nline two"

func TestManagedBlock_FreshFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")

	changed, err := setup.Apply(path, testBody, "v1")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !changed {
		t.Fatal("Apply on a fresh file should report changed=true")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	want := setup.RenderBlock(testBody, "v1") + "\n"
	if string(got) != want {
		t.Errorf("content = %q, want %q", got, want)
	}

	// Exactly one block: only one start marker in the file.
	if n := strings.Count(string(got), "<!-- plumb:managed:start"); n != 1 {
		t.Errorf("expected exactly one start marker, found %d", n)
	}
}

func TestManagedBlock_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")

	if _, err := setup.Apply(path, testBody, "v1"); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back after first Apply: %v", err)
	}

	changed, err := setup.Apply(path, testBody, "v1")
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if changed {
		t.Error("second Apply with an identical template should report changed=false")
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back after second Apply: %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("running Apply twice produced different content:\nfirst:  %q\nsecond: %q", first, second)
	}
}

func TestManagedBlock_PreservesOutsideContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")

	userContent := "# My Project\n\nSome hand-written notes for agents.\n"
	if err := os.WriteFile(path, []byte(userContent), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := setup.Apply(path, testBody, "v1"); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if !strings.Contains(string(got), userContent) {
		t.Errorf("user content not preserved verbatim; got:\n%s", got)
	}
	if !strings.Contains(string(got), setup.RenderBlock(testBody, "v1")) {
		t.Errorf("managed block not present; got:\n%s", got)
	}

	// Re-applying a new version must still leave the user's prose untouched
	// and must not duplicate it.
	if _, err := setup.Apply(path, "updated body", "v2"); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	got2, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back after second Apply: %v", err)
	}
	if !strings.Contains(string(got2), userContent) {
		t.Errorf("user content lost after a second Apply; got:\n%s", got2)
	}
	if strings.Count(string(got2), "# My Project") != 1 {
		t.Errorf("user content duplicated; got:\n%s", got2)
	}
	if strings.Count(string(got2), "<!-- plumb:managed:start") != 1 {
		t.Errorf("expected exactly one block after re-applying; got:\n%s", got2)
	}
	if strings.Contains(string(got2), "line one") {
		t.Errorf("stale block content survived the rewrite; got:\n%s", got2)
	}
}

func TestManagedBlock_Symlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "AGENTS.md")
	link := filepath.Join(dir, "CLAUDE.md")

	if err := os.WriteFile(target, []byte("# shared brief\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if _, err := setup.Apply(link, testBody, "v1"); err != nil {
		t.Fatalf("Apply via symlink: %v", err)
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("Lstat link: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("CLAUDE.md was replaced with a regular file — it must stay a symlink")
	}

	// Compare identity, not the path string: on macOS t.TempDir() paths often
	// live under a /var -> /private/var symlink, so a string comparison
	// against the resolved path is a portability trap (see os.SameFile).
	resolved, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatalf("resolving symlink: %v", err)
	}
	resolvedInfo, err := os.Stat(resolved)
	if err != nil {
		t.Fatalf("stat resolved: %v", err)
	}
	targetInfo, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat target: %v", err)
	}
	if !os.SameFile(resolvedInfo, targetInfo) {
		t.Fatalf("symlink now points at %s, want %s", resolved, target)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading target: %v", err)
	}
	if !strings.Contains(string(got), "# shared brief") {
		t.Errorf("target's original content lost; got:\n%s", got)
	}
	if !strings.Contains(string(got), setup.RenderBlock(testBody, "v1")) {
		t.Errorf("managed block not written to the symlink target; got:\n%s", got)
	}

	// Applying through the OTHER symlink name (GEMINI.md -> AGENTS.md, as in
	// this repo) must resolve to the same file and stay a no-op once current.
	link2 := filepath.Join(dir, "GEMINI.md")
	if err := os.Symlink(target, link2); err != nil {
		t.Fatal(err)
	}
	changed, err := setup.Apply(link2, testBody, "v1")
	if err != nil {
		t.Fatalf("Apply via second symlink: %v", err)
	}
	if changed {
		t.Error("applying the same current block through a second symlink to the same target should be a no-op")
	}
}

func TestManagedBlock_CheckDetectsMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")

	status, err := setup.Check(path, testBody, "v1")
	if err != nil {
		t.Fatalf("Check on an absent file: %v", err)
	}
	if status != setup.StatusMissing {
		t.Errorf("status = %v, want %v", status, setup.StatusMissing)
	}

	if err := os.WriteFile(path, []byte("# no managed block here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	status, err = setup.Check(path, testBody, "v1")
	if err != nil {
		t.Fatalf("Check on a markerless file: %v", err)
	}
	if status != setup.StatusMissing {
		t.Errorf("status = %v, want %v", status, setup.StatusMissing)
	}
}

func TestManagedBlock_CheckDetectsVersionBump(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")

	if _, err := setup.Apply(path, testBody, "v1"); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// The installed block is v1; checking against a newer template version
	// must report it as stale, not current or missing.
	status, err := setup.Check(path, "a new body entirely", "v2")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if status != setup.StatusStale {
		t.Errorf("status = %v, want %v", status, setup.StatusStale)
	}

	// --sync's operation is just Apply with the current template.
	if _, err := setup.Apply(path, "a new body entirely", "v2"); err != nil {
		t.Fatalf("Apply (sync): %v", err)
	}
	status, err = setup.Check(path, "a new body entirely", "v2")
	if err != nil {
		t.Fatalf("Check after sync: %v", err)
	}
	if status != setup.StatusCurrent {
		t.Errorf("status after sync = %v, want %v", status, setup.StatusCurrent)
	}
}

func TestManagedBlock_CheckDetectsModified(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")

	if _, err := setup.Apply(path, testBody, "v1"); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Hand-edit inside the markers without touching the version.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := []byte(strings.Replace(string(data), "line one", "a user rewrote this line", 1))
	if err := os.WriteFile(path, edited, 0o644); err != nil {
		t.Fatal(err)
	}

	status, err := setup.Check(path, testBody, "v1")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if status != setup.StatusModified {
		t.Errorf("status = %v, want %v", status, setup.StatusModified)
	}
}

func TestManagedBlock_TemplateSizeGuard(t *testing.T) {
	if !setup.TemplateWithinBudget(setup.DefaultTemplate) {
		n := setup.TemplateLineCount(setup.DefaultTemplate)
		t.Errorf("DefaultTemplate is %d lines, want <= %d — trim it, don't raise the budget", n, setup.MaxTemplateLines)
	}
}
