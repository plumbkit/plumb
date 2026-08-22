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

// TestManagedBlock_DanglingSymlinkStaysASymlink is the regression for a
// review-round-1-on-PR-2 blocking defect: a project's VERY FIRST
// `plumb setup <client>` on a symlinked layout (CLAUDE.md -> AGENTS.md, this
// repo's own convention) hits the target BEFORE it has ever been created —
// a dangling symlink. paths.Canonical's missing-path fallback does not read
// symlink targets, so it answered with the LINK's own path, and Apply's
// AtomicWrite rename onto that path REPLACED THE SYMLINK ITSELF with a
// regular file — silently converting CLAUDE.md from "symlinks to AGENTS.md"
// into "is its own independent file", which then falsifies the whole
// convergence story two callers apart (a second client applying through a
// DIFFERENT symlink to what should be the same real file instead creates
// yet another independent file). Apply must instead create the file the
// dangling link NAMES (AGENTS.md) and leave the symlink itself untouched.
func TestManagedBlock_DanglingSymlinkStaysASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "AGENTS.md")
	link := filepath.Join(dir, "CLAUDE.md")

	// The symlink exists, but its target does not — the exact state of a
	// fresh checkout of this repo's own layout before anyone has ever run
	// `plumb setup`.
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("test setup: AGENTS.md must not exist yet, stat err=%v", err)
	}

	if _, err := setup.Apply(link, testBody, "v1"); err != nil {
		t.Fatalf("Apply via dangling symlink: %v", err)
	}

	linkInfo, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("Lstat link: %v", err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatal("CLAUDE.md was replaced with a regular file — it must stay a symlink even when its target was dangling")
	}

	resolved, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatalf("resolving symlink after Apply: %v", err)
	}
	resolvedInfo, err := os.Stat(resolved)
	if err != nil {
		t.Fatalf("stat resolved: %v", err)
	}
	targetInfo, err := os.Stat(target)
	if err != nil {
		t.Fatalf("AGENTS.md (the symlink's target) was not created: %v", err)
	}
	if !os.SameFile(resolvedInfo, targetInfo) {
		t.Fatalf("symlink now points at %s, want %s", resolved, target)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading target: %v", err)
	}
	if !strings.Contains(string(got), setup.RenderBlock(testBody, "v1")) {
		t.Errorf("managed block not written to the resolved target; got:\n%s", got)
	}

	// A second client applying through a DIFFERENT symlink to the same
	// (now-real) target must land on the identical file, matching
	// TestManagedBlock_Symlink's already-passing case for a non-dangling
	// target.
	link2 := filepath.Join(dir, "GEMINI.md")
	if err := os.Symlink(target, link2); err != nil {
		t.Fatal(err)
	}
	changed, err := setup.Apply(link2, testBody, "v1")
	if err != nil {
		t.Fatalf("Apply via second symlink: %v", err)
	}
	if changed {
		t.Error("applying the same current block through a second symlink to the now-real target should be a no-op")
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

// TestManagedBlock_ClientTemplateSizeGuard applies the same 25-line budget to
// every per-client template (client_templates.go) that TestManagedBlock_
// TemplateSizeGuard applies to DefaultTemplate — a per-client body earns its
// place in someone else's file only by staying short too.
func TestManagedBlock_ClientTemplateSizeGuard(t *testing.T) {
	for client, body := range setup.ClientTemplates {
		if !setup.TemplateWithinBudget(body) {
			n := setup.TemplateLineCount(body)
			t.Errorf("%s template is %d lines, want <= %d — trim it, don't raise the budget", client, n, setup.MaxTemplateLines)
		}
	}
}

// TestManagedBlock_RemoveDeletesBlockPreservingUserContent is Remove's
// counterpart to TestManagedBlock_PreservesOutsideContent: removing a block
// Apply appended after existing user prose must reconstruct that prose
// exactly, byte for byte, not just delete the marker span.
func TestManagedBlock_RemoveDeletesBlockPreservingUserContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")

	userContent := "# My Project\n\nSome hand-written notes for agents.\n"
	if err := os.WriteFile(path, []byte(userContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Apply(path, testBody, "v1"); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	removed, err := setup.Remove(path)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !removed {
		t.Fatal("Remove should report removed=true")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if string(got) != userContent {
		t.Errorf("content after Remove = %q, want original %q", got, userContent)
	}
}

// TestManagedBlock_RemoveDeletesFileWhenOnlyContentWasTheBlock covers the
// fresh-file case: a file whose ONLY content is the managed block should be
// deleted outright, not left behind as an empty file.
func TestManagedBlock_RemoveDeletesFileWhenOnlyContentWasTheBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")

	if _, err := setup.Apply(path, testBody, "v1"); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	removed, err := setup.Remove(path)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !removed {
		t.Fatal("Remove should report removed=true")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file should be gone after removing its only content, stat err=%v", err)
	}
}

// TestManagedBlock_RemoveNoBlockIsNoOp covers a file that exists but has no
// managed block: Remove must be a no-op, matching Check's StatusMissing.
func TestManagedBlock_RemoveNoBlockIsNoOp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	content := "# no managed block here\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	removed, err := setup.Remove(path)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if removed {
		t.Error("Remove on a file without a managed block should report removed=false")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Errorf("Remove must not touch a file with no managed block")
	}
}

// TestManagedBlock_RemoveAbsentFileIsNoOp covers a file that does not exist
// at all.
func TestManagedBlock_RemoveAbsentFileIsNoOp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")

	removed, err := setup.Remove(path)
	if err != nil {
		t.Fatalf("Remove on an absent file: %v", err)
	}
	if removed {
		t.Error("Remove on an absent file should report removed=false")
	}
}

// TestManagedBlock_RemoveMalformedRefuses matches Apply's own rigor: a file
// whose markers do not parse cleanly must be refused, not guessed at.
func TestManagedBlock_RemoveMalformedRefuses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	content := "some user prose\n" + setup.EndMarker + "\nmore prose\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := setup.Remove(path); err == nil {
		t.Fatal("Remove on a file with an orphan end marker must refuse")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != content {
		t.Errorf("Remove must not touch the file when refusing")
	}
}

// TestManagedBlock_RemoveFollowsSymlink mirrors TestManagedBlock_Symlink:
// Remove must resolve a symlinked instruction file (CLAUDE.md -> AGENTS.md)
// to its real target, remove the block there, and leave the symlink itself
// untouched.
func TestManagedBlock_RemoveFollowsSymlink(t *testing.T) {
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

	removed, err := setup.Remove(link)
	if err != nil {
		t.Fatalf("Remove via symlink: %v", err)
	}
	if !removed {
		t.Fatal("Remove should report removed=true")
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("Lstat link: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("CLAUDE.md was replaced or removed — it must stay a symlink")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading target: %v", err)
	}
	if string(got) != "# shared brief\n" {
		t.Errorf("target content after Remove = %q, want original brief preserved", got)
	}
}

// TestManagedBlock_RemoveThenApplyRoundTrips checks Remove and Apply compose
// cleanly: Apply, Remove, Apply again should reproduce the exact bytes the
// first Apply produced — Remove leaves nothing behind that would make a
// fresh Apply diverge.
func TestManagedBlock_RemoveThenApplyRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")

	if _, err := setup.Apply(path, testBody, "v1"); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := setup.Remove(path); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := setup.Apply(path, testBody, "v1"); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("Apply after Remove diverged:\nfirst:  %q\nsecond: %q", first, second)
	}
}

// TestManagedBlock_MalformedOrphanStartRefusesRatherThanCorrupt is the
// regression for the destructive sequence: a user deletes just the end
// marker, leaving an orphan start. A permissive scanner treats that as
// "no block" and APPENDS a fresh one; the next Apply then pairs the orphan
// start with the new block's end marker and silently deletes everything
// between — including user prose that was never inside any block. Apply must
// refuse instead.
func TestManagedBlock_MalformedOrphanStartRefusesRatherThanCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")

	if _, err := setup.Apply(path, testBody, "v1"); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Delete the end marker, leaving an orphan start, then add prose that must
	// never be lost.
	corrupted := strings.Replace(string(data), setup.EndMarker, "", 1)
	corrupted += "\nImportant user prose that must never be lost.\n"
	if err := os.WriteFile(path, []byte(corrupted), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := setup.Apply(path, testBody, "v1"); err == nil {
		t.Fatal("Apply on a file with an orphan start marker must refuse (error), not write")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != corrupted {
		t.Errorf("Apply must not touch the file when refusing;\nwant:\n%s\ngot:\n%s", corrupted, after)
	}
	if !strings.Contains(string(after), "Important user prose that must never be lost.") {
		t.Fatal("user prose was lost")
	}

	status, err := setup.Check(path, testBody, "v1")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if status != setup.StatusMalformed {
		t.Errorf("status = %v, want %v", status, setup.StatusMalformed)
	}
}

// TestManagedBlock_MalformedOrphanEndRefuses covers the other half: an end
// marker with no preceding start (the user deleted the start marker instead).
func TestManagedBlock_MalformedOrphanEndRefuses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	content := "some user prose\n" + setup.EndMarker + "\nmore prose\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	status, err := setup.Check(path, testBody, "v1")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if status != setup.StatusMalformed {
		t.Errorf("status = %v, want %v", status, setup.StatusMalformed)
	}
	if _, err := setup.Apply(path, testBody, "v1"); err == nil {
		t.Fatal("Apply on a file with an orphan end marker must refuse")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != content {
		t.Errorf("Apply must not touch the file when refusing")
	}
}

// TestManagedBlock_MalformedEndBeforeStartRefuses covers an end marker that
// textually precedes any start marker in the file.
func TestManagedBlock_MalformedEndBeforeStartRefuses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	content := setup.EndMarker + "\n" + setup.RenderBlock(testBody, "v1") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	status, err := setup.Check(path, testBody, "v1")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if status != setup.StatusMalformed {
		t.Errorf("status = %v, want %v", status, setup.StatusMalformed)
	}
	if _, err := setup.Apply(path, testBody, "v1"); err == nil {
		t.Fatal("Apply must refuse when an end marker precedes any start")
	}
}

// TestManagedBlock_QuotedMarkerInProseDoesNotGrowOnRepeatedApply is the
// regression for a permissive scanner that latches onto the FIRST textual
// occurrence of the marker prefix: a file that merely quotes the marker text
// inline (documenting the mechanism, say) grew a fresh block on every single
// Apply (measured 1→2→3→4). Marker recognition must be line-anchored — a
// candidate only counts if the WHOLE line is exactly the marker — so an
// inline quote is never mistaken for a real one.
func TestManagedBlock_QuotedMarkerInProseDoesNotGrowOnRepeatedApply(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	userDoc := "# Docs\n\nOur markers look like `<!-- plumb:managed:start v1 -->` and `<!-- plumb:managed:end -->` inline.\n"
	if err := os.WriteFile(path, []byte(userDoc), 0o644); err != nil {
		t.Fatal(err)
	}

	for i := range 4 {
		if _, err := setup.Apply(path, testBody, "v1"); err != nil {
			t.Fatalf("Apply #%d: %v", i+1, err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Count LINE-EXACT markers, not the raw substring: the quoted mention in
	// userDoc contains the marker text too, so a substring count would always
	// read 2 even when the scanner correctly ignored the quote.
	if n := countMarkerLines(string(data)); n != 1 {
		t.Errorf("expected exactly one real start marker after 4 Applies, found %d:\n%s", n, data)
	}
	if !strings.Contains(string(data), userDoc) {
		t.Errorf("user doc lost; got:\n%s", data)
	}
}

// countMarkerLines counts lines that are EXACTLY a start-marker line —
// mirroring the line-anchored matching scanBlocks applies — so a marker
// quoted mid-sentence elsewhere in the file is not counted as a real one.
func countMarkerLines(content string) int {
	n := 0
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "<!-- plumb:managed:start ") && strings.HasSuffix(line, " -->") {
			n++
		}
	}
	return n
}

// TestManagedBlock_MultipleWellFormedBlocksAreMalformed is the regression for
// silent data loss when two well-formed blocks coexist (e.g. after a bug, or
// a hand-merge): Apply used to rewrite only the first, leaving a stale second
// block that Check never saw — it reported Current. Two blocks, however each
// is individually well-formed, must be flagged rather than silently resolved.
func TestManagedBlock_MultipleWellFormedBlocksAreMalformed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	one := setup.RenderBlock("body one", "v1")
	two := setup.RenderBlock("body two", "v1")
	content := one + "\n\nsome prose\n\n" + two + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	status, err := setup.Check(path, testBody, "v1")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if status != setup.StatusMalformed {
		t.Errorf("status = %v, want %v (two blocks present)", status, setup.StatusMalformed)
	}

	if _, err := setup.Apply(path, testBody, "v1"); err == nil {
		t.Fatal("Apply must refuse when more than one managed block is present")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != content {
		t.Errorf("Apply must not touch the file when refusing")
	}
}
