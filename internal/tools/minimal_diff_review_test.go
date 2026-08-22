package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/topology"
	goext "github.com/plumbkit/plumb/internal/topology/extractors/golang"
)

// newReviewTool builds the tool pinned to ws with no topology store (a nil
// storeFn): the diff-only checks need none, and the topology-backed checks then
// degrade and say so in the limits section.
func newReviewTool(ws string) *MinimalDiffReview {
	return NewMinimalDiffReview(nil).WithWorkspace(func(context.Context) string { return ws })
}

func callReview(t *testing.T, tool *MinimalDiffReview, args map[string]any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	return tool.Execute(context.Background(), raw)
}

func TestMinimalDiffReview_Schema(t *testing.T) {
	var tool *MinimalDiffReview
	if tool.Name() != "minimal_diff_review" {
		t.Fatalf("Name() = %q", tool.Name())
	}
	var schema map[string]any
	if err := json.Unmarshal(tool.InputSchema(), &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if schema["additionalProperties"] != false {
		t.Errorf("schema must declare additionalProperties:false")
	}
	if !strings.Contains(tool.Description(), "block a write") {
		t.Errorf("description should state findings never block a write")
	}
}

func TestMinimalDiffReview_RejectsBadMode(t *testing.T) {
	tool := newReviewTool(t.TempDir())
	if _, err := callReview(t, tool, map[string]any{"mode": "bogus"}); err == nil {
		t.Fatalf("want an error for an invalid mode")
	}
}

func TestMinimalDiffReview_RejectsOptionBaseRef(t *testing.T) {
	// base_ref precedes the "--" pathspec separator, so a dash-leading value
	// would reach git as an option — "--output" writes an arbitrary file and
	// "--ext-diff" runs a configured command. It must be rejected up front.
	dir, _ := setupReviewRepo(t)
	writeFileT(t, dir, "wrap.go", "package pkg\n\nfunc Wrap(a int) int {\n\treturn Target(a)\n}\n")
	tool := newReviewTool(dir)
	target := filepath.Join(t.TempDir(), "pwned")
	_, err := callReview(t, tool, map[string]any{"base_ref": "--output=" + target})
	if err == nil {
		t.Fatalf("want an error for a dash-leading base_ref")
	}
	if _, statErr := os.Stat(target); statErr == nil {
		t.Fatalf("the option-injected output file was created — base_ref reached git as an option")
	}
}

func TestMinimalDiffReview_UnattachedWorkspace(t *testing.T) {
	tool := NewMinimalDiffReview(nil).WithWorkspace(func(context.Context) string { return "" })
	_, err := callReview(t, tool, nil)
	if err == nil || !IsWorkspaceBoundaryError(err) {
		t.Fatalf("want an UnattachedWorkspaceError, got %v", err)
	}
}

func TestMinimalDiffReview_DegradesOutsideGitRepo(t *testing.T) {
	// CI redirects test temp dirs into the checkout's .testcache/, which IS
	// inside a git repo — so "a bare temp dir" alone does not guarantee the
	// not-a-repo premise. A ceiling at the temp dir's parent stops git's
	// upward discovery regardless of where the runner puts TMPDIR. The path
	// is symlink-resolved first (macOS /var → /private/var): git compares
	// ceiling entries textually against resolved paths.
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	tool := newReviewTool(dir)
	out, err := callReview(t, tool, nil)
	if err != nil {
		t.Fatalf("outside a git repo should degrade cleanly, got error: %v", err)
	}
	if !strings.Contains(out, "not a git repository") {
		t.Errorf("want a clean not-a-repo message, got: %s", out)
	}
}

// setupReviewRepo makes a git repo with a committed tracked file, returning its
// path. A run helper is returned for further git operations.
func setupReviewRepo(t *testing.T) (dir string, run func(args ...string)) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir = t.TempDir()
	run = func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init")
	run("config", "user.email", "t@example.com")
	run("config", "user.name", "T")
	writeFileT(t, dir, "existing.go", "package pkg\n\nfunc Existing() {}\n")
	run("add", "existing.go")
	run("commit", "-m", "init")
	return dir, run
}

func writeFileT(t *testing.T, dir, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMinimalDiffReview_ReviewsUntrackedNewFile_Changed(t *testing.T) {
	dir, _ := setupReviewRepo(t)
	// A brand-new, unstaged file with a thin forwarding wrapper.
	writeFileT(t, dir, "wrap.go", "package pkg\n\nfunc Wrap(a int, b string) error {\n\treturn Target(a, b)\n}\n")
	tool := newReviewTool(dir)
	out, err := callReview(t, tool, nil) // default mode=changed, base_ref=HEAD
	if err != nil {
		t.Fatalf("review error: %v", err)
	}
	if !strings.Contains(out, "thin-wrapper") {
		t.Errorf("expected a thin-wrapper finding for the untracked new file, got:\n%s", out)
	}
	if !strings.Contains(out, "advisory") {
		t.Errorf("output should mark itself advisory:\n%s", out)
	}
}

func TestMinimalDiffReview_StagedMode(t *testing.T) {
	dir, run := setupReviewRepo(t)
	writeFileT(t, dir, "wrap.go", "package pkg\n\nfunc Wrap(a int) int {\n\treturn Target(a)\n}\n")
	run("add", "wrap.go")
	tool := newReviewTool(dir)
	out, err := callReview(t, tool, map[string]any{"mode": "staged"})
	if err != nil {
		t.Fatalf("staged review error: %v", err)
	}
	if !strings.Contains(out, "thin-wrapper") {
		t.Errorf("staged mode should review the indexed file, got:\n%s", out)
	}
}

func TestMinimalDiffReview_VerificationGapOnModifiedTrackedFile(t *testing.T) {
	dir, _ := setupReviewRepo(t)
	// Modify the tracked source file with logic, add no test.
	writeFileT(t, dir, "existing.go", "package pkg\n\nfunc Existing() {\n\tif cond() {\n\t\tact()\n\t}\n}\n")
	tool := newReviewTool(dir)
	out, err := callReview(t, tool, nil)
	if err != nil {
		t.Fatalf("review error: %v", err)
	}
	if !strings.Contains(out, "verification-gap") {
		t.Errorf("expected a verification-gap finding, got:\n%s", out)
	}
	if !strings.Contains(out, "topology_affected") {
		t.Errorf("verification-gap should recommend the follow-up calls, got:\n%s", out)
	}
}

func TestMinimalDiffReview_CleanWhenNoChanges(t *testing.T) {
	dir, _ := setupReviewRepo(t)
	tool := newReviewTool(dir)
	out, err := callReview(t, tool, nil)
	if err != nil {
		t.Fatalf("review error: %v", err)
	}
	if !strings.Contains(out, "findings: none") {
		t.Errorf("a clean tree should report no findings, got:\n%s", out)
	}
	if !strings.Contains(out, "not analysed / limits") {
		t.Errorf("output should always carry the limits section, got:\n%s", out)
	}
}

func TestMinimalDiffReview_BoundedOutputOnLargeDiff(t *testing.T) {
	dir, _ := setupReviewRepo(t)
	// Many untracked thin wrappers → many findings, capped by max_findings.
	var b strings.Builder
	b.WriteString("package pkg\n")
	for i := range 12 {
		fmt.Fprintf(&b, "\nfunc Wrap%d(a int) int {\n\treturn Target%d(a)\n}\n", i, i)
	}
	writeFileT(t, dir, "many.go", b.String())
	tool := newReviewTool(dir)
	out, err := callReview(t, tool, map[string]any{"max_findings": 3})
	if err != nil {
		t.Fatalf("review error: %v", err)
	}
	if strings.Count(out, "thin-wrapper") > 3 {
		t.Errorf("findings should be capped at 3, got:\n%s", out)
	}
	if !strings.Contains(out, "truncated") {
		t.Errorf("a capped review should note truncation, got:\n%s", out)
	}
}

func TestMinimalDiffReview_IncludeSuggestionsFalse(t *testing.T) {
	dir, _ := setupReviewRepo(t)
	writeFileT(t, dir, "wrap.go", "package pkg\n\nfunc Wrap(a int) int {\n\treturn Target(a)\n}\n")
	tool := newReviewTool(dir)
	out, err := callReview(t, tool, map[string]any{"include_suggestions": false})
	if err != nil {
		t.Fatalf("review error: %v", err)
	}
	if strings.Contains(out, "smaller alternative:") {
		t.Errorf("suggestions were disabled but an alternative was printed:\n%s", out)
	}
}

// --- B12: path-scoping and boundary enforcement ---

func TestMinimalDiffReview_ScopedToRequestedFile(t *testing.T) {
	dir, _ := setupReviewRepo(t)
	writeFileT(t, dir, "wrap_one.go", "package pkg\n\nfunc WrapOne(a int) int {\n\treturn TargetOne(a)\n}\n")
	writeFileT(t, dir, "wrap_two.go", "package pkg\n\nfunc WrapTwo(a int) int {\n\treturn TargetTwo(a)\n}\n")
	tool := newReviewTool(dir)
	out, err := callReview(t, tool, map[string]any{"files": []string{"wrap_one.go"}})
	if err != nil {
		t.Fatalf("review error: %v", err)
	}
	if !strings.Contains(out, "wrap_one.go") {
		t.Errorf("expected the scoped file's finding, got:\n%s", out)
	}
	if strings.Contains(out, "wrap_two.go") {
		t.Errorf("files scoping leaked the unscoped file's finding, got:\n%s", out)
	}
}

func TestMinimalDiffReview_BoundaryRejectsFileOutsideWorkspace(t *testing.T) {
	dir, _ := setupReviewRepo(t)
	outside := filepath.Join(t.TempDir(), "outside.go")
	if err := os.WriteFile(outside, []byte("package pkg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := newReviewTool(dir).WithBoundary(testBoundaryGuard(dir))
	_, err := callReview(t, tool, map[string]any{"files": []string{outside}})
	if err == nil {
		t.Fatalf("want a boundary error for a files entry outside the workspace")
	}
	if !IsWorkspaceBoundaryError(err) {
		t.Fatalf("want a WorkspaceBoundaryError (rejected before any git invocation), got: %v", err)
	}
}

// --- B13: topology-backed checks over a real indexed store ---

func TestMinimalDiffReview_SingleUseFinding_RealTopologyStore(t *testing.T) {
	dir, _ := setupReviewRepo(t)
	// worker.go is a brand-new untracked file: a caller and a single-use helper
	// it alone calls. The caller's body has two statements so it is not itself
	// mistaken for a thin forwarding wrapper — this isolates the single-use
	// finding under test.
	src := "package pkg\n\n" +
		"func CallHelper() int {\n\tx := helperOnce()\n\treturn x + 1\n}\n\n" +
		"func helperOnce() int {\n\treturn 42\n}\n"
	writeFileT(t, dir, "worker.go", src)

	store, err := topology.Open(dir, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024}, []topology.Extractor{goext.New()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	uri := "file://" + filepath.Join(dir, "worker.go")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if nodes, _ := store.SymbolsInFile(context.Background(), uri); len(nodes) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("topology did not index worker.go within 5s")
		}
		time.Sleep(20 * time.Millisecond)
	}

	tool := NewMinimalDiffReview(func() *topology.Store { return store }).WithWorkspace(func(context.Context) string { return dir })
	out, err := callReview(t, tool, nil)
	if err != nil {
		t.Fatalf("review error: %v", err)
	}
	if !strings.Contains(out, "single-use-abstraction") {
		t.Errorf("expected a single-use-abstraction finding via the real topology store, got:\n%s", out)
	}
	if !strings.Contains(out, "helperOnce") {
		t.Errorf("expected the finding to name helperOnce, got:\n%s", out)
	}
}

// --- B14b: untracked binary file alongside a real change ---

func TestMinimalDiffReview_UntrackedBinaryFile_SkippedNoCrash(t *testing.T) {
	dir, _ := setupReviewRepo(t)
	// Binary content (contains NUL bytes), named like a generated image asset.
	binContent := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x01, 0x02, 0x00, 0x03}
	if err := os.WriteFile(filepath.Join(dir, "image.png"), binContent, 0o644); err != nil {
		t.Fatal(err)
	}
	writeFileT(t, dir, "wrap.go", "package pkg\n\nfunc Wrap(a int) int {\n\treturn Target(a)\n}\n")
	tool := newReviewTool(dir)
	out, err := callReview(t, tool, nil)
	if err != nil {
		t.Fatalf("review error (binary file should not crash the review): %v", err)
	}
	if !strings.Contains(out, "thin-wrapper") {
		t.Errorf("the Go change should still be reviewed alongside a binary file, got:\n%s", out)
	}
	if strings.Contains(out, "image.png") {
		t.Errorf("the binary file should be skipped, not referenced in findings, got:\n%s", out)
	}
}

// --- B16: a nonexistent base_ref reports a clean error ---

func TestMinimalDiffReview_BadBaseRef_CleanError(t *testing.T) {
	dir, _ := setupReviewRepo(t)
	tool := newReviewTool(dir)
	_, err := callReview(t, tool, map[string]any{"base_ref": "no-such-ref"})
	if err == nil {
		t.Fatalf("want an error for a nonexistent base_ref")
	}
	if !strings.Contains(err.Error(), "no-such-ref") {
		t.Errorf("error should mention the bad ref, got: %v", err)
	}
	if !strings.Contains(err.Error(), "minimal_diff_review: git diff:") {
		t.Errorf("error should come from the clean git-diff stderr branch, not a raw Go error dump; got: %v", err)
	}
}

// --- B1: a zero-byte untracked file produces no phantom diff/finding ---

func TestSynthesiseNewFileDiff_EmptyFile_NoPhantomHunk(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "empty.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out := synthesiseNewFileDiff(dir, "empty.txt")
	if !strings.Contains(out, "diff --git a/empty.txt b/empty.txt") || !strings.Contains(out, "new file mode 100644") {
		t.Errorf("expected the file-creation header, got: %q", out)
	}
	if strings.Contains(out, "@@") {
		t.Errorf("an empty file has no lines to add — expected no hunk header, got: %q", out)
	}
	if strings.Contains(out, "\n+") {
		t.Errorf("an empty file must not synthesise a phantom added line, got: %q", out)
	}
}

func TestMinimalDiffReview_UntrackedEmptyFile_NoCrashGoChangeStillReviewed(t *testing.T) {
	dir, _ := setupReviewRepo(t)
	writeFileT(t, dir, "empty.txt", "")
	writeFileT(t, dir, "wrap.go", "package pkg\n\nfunc Wrap(a int) int {\n\treturn Target(a)\n}\n")
	tool := newReviewTool(dir)
	out, err := callReview(t, tool, nil)
	if err != nil {
		t.Fatalf("review error (empty untracked file should not crash the review): %v", err)
	}
	if !strings.Contains(out, "thin-wrapper") {
		t.Errorf("the Go change should still be reviewed alongside an empty untracked file, got:\n%s", out)
	}
}

// TestMinimalDiffReview_GeneratedBundleDoesNotHideLaterFiles is the regression
// test for a silent, positionally biased truncation.
//
// The budget used to be spent as a byte PREFIX of the whole diff. git emits a
// diff in path order, so a generated bundle sorting early (dist/ before
// existing.go) consumed all of it and every later file was cut — the report
// said only that a byte count had been exceeded, never which files it had
// therefore not looked at. A review that skipped the source reads exactly like
// a review that found nothing wrong with it.
//
// Both halves are asserted on purpose: that the later file WAS reviewed, and
// that the bundle is NAMED. Either alone would pass a fix that traded one
// silence for another.
func TestMinimalDiffReview_GeneratedBundleDoesNotHideLaterFiles(t *testing.T) {
	dir, run := setupReviewRepo(t)

	// A generated bundle, larger than the whole review budget on its own, at a
	// path that sorts BEFORE the source file below.
	if err := os.MkdirAll(filepath.Join(dir, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	var bundle strings.Builder
	for i := range 40000 {
		fmt.Fprintf(&bundle, "var _chunk%d = function(a){return a+%d};\n", i, i)
	}
	if bundle.Len() <= maxReviewDiffBytes {
		t.Fatalf("fixture too small to exercise the cap: %d bytes", bundle.Len())
	}
	writeFileT(t, dir, "dist/bundle.js", bundle.String())
	run("add", "dist/bundle.js")

	// A real source change, after the bundle in path order, that has a finding.
	writeFileT(t, dir, "existing.go", "package pkg\n\nfunc Existing() {\n\tif cond() {\n\t\tact()\n\t}\n}\n")

	out, err := callReview(t, newReviewTool(dir), nil)
	if err != nil {
		t.Fatalf("review error: %v", err)
	}
	if !strings.Contains(out, "verification-gap") {
		t.Errorf("the source file after the bundle was not reviewed — the bundle ate the budget:\n%s", out)
	}
	if !strings.Contains(out, "dist/bundle.js") {
		t.Errorf("the skipped bundle must be named, not silently dropped:\n%s", out)
	}
}

// TestCapPerFile_DropsWholeFilesOnly pins the property that makes the cap safe
// to analyse: what survives is always a COMPLETE file diff. A byte cut could
// land mid-hunk, leaving the parser a fragment of the last file and the report
// no way to say so.
func TestCapPerFile_DropsWholeFilesOnly(t *testing.T) {
	big := strings.Repeat("+padding line to exceed the per-file budget\n", 5000)
	raw := "diff --git a/big.js b/big.js\n--- a/big.js\n+++ b/big.js\n@@ -0,0 +1 @@\n" + big +
		"diff --git a/small.go b/small.go\n--- a/small.go\n+++ b/small.go\n@@ -0,0 +1 @@\n+package p\n"

	kept, capped := capPerFile(raw, 1024, maxReviewDiffBytes)

	if len(capped) != 1 || capped[0].Path != "big.js" {
		t.Fatalf("want big.js reported as capped, got %+v", capped)
	}
	if capped[0].GlobalCapped {
		t.Errorf("big.js was dropped by its OWN size, not the global backstop: %+v", capped[0])
	}
	if strings.Contains(kept, "padding line") {
		t.Error("an over-budget file must be dropped whole, not truncated")
	}
	if !strings.Contains(kept, "+package p") {
		t.Errorf("the small file must survive, got:\n%s", kept)
	}
	if !strings.HasPrefix(kept, "diff --git a/small.go") {
		t.Errorf("kept text must start at a file header, got:\n%s", kept)
	}
}

// TestCapPerFile_GlobalBudgetAlsoSpentInWholeFiles guards the backstop: when
// many individually-small files exceed the total, the overflow is still dropped
// file by file and reported, never sliced mid-hunk.
func TestCapPerFile_GlobalBudgetAlsoSpentInWholeFiles(t *testing.T) {
	var raw strings.Builder
	for i := range 10 {
		fmt.Fprintf(&raw, "diff --git a/f%d.go b/f%d.go\n@@ -0,0 +1 @@\n%s", i, i, strings.Repeat("+x\n", 50))
	}
	kept, capped := capPerFile(raw.String(), 1<<20, 600)

	if len(capped) == 0 {
		t.Fatal("want some files reported as dropped by the total budget")
	}
	for _, c := range capped {
		if c.Path == "" {
			t.Errorf("a dropped file must be named, got %+v", capped)
		}
		if !c.GlobalCapped {
			t.Errorf("file dropped by the total budget must be marked GlobalCapped, got %+v", c)
		}
	}
	if n := strings.Count(kept, "diff --git "); n*100 > 600+100 {
		t.Errorf("kept %d files, over the total budget", n)
	}
	if kept != "" && !strings.HasPrefix(kept, "diff --git ") {
		t.Error("kept text must start at a file header")
	}
}

// TestWriteCappedFiles_ReasonMatchesWhyItWasDropped guards the fix for a
// wrong-reason report: a file dropped by the global 1 MiB backstop can be well
// under the 128 KiB per-file budget on its own, so it must never be reported as
// "over the per-file budget" — that reason belongs only to a file whose OWN
// diff exceeded the per-file cap.
func TestWriteCappedFiles_ReasonMatchesWhyItWasDropped(t *testing.T) {
	var sb strings.Builder
	writeCappedFiles(&sb, []cappedFile{
		{Path: "big.js", Bytes: 200 * 1024, GlobalCapped: false},
		{Path: "small.go", Bytes: 50, GlobalCapped: true},
	})
	out := sb.String()

	if !strings.Contains(out, "big.js") || !strings.Contains(out, "per-file budget") {
		t.Errorf("big.js must be reported as over the per-file budget, got:\n%s", out)
	}
	bigLine := lineContaining(out, "big.js")
	if strings.Contains(bigLine, "total-diff budget") {
		t.Errorf("big.js was NOT dropped by the total budget, wrong reason attached:\n%s", bigLine)
	}

	if !strings.Contains(out, "small.go") || !strings.Contains(out, "total-diff budget") {
		t.Errorf("small.go must be reported as dropped by the total budget, got:\n%s", out)
	}
	smallLine := lineContaining(out, "small.go")
	if strings.Contains(smallLine, "per-file budget") {
		t.Errorf("small.go was dropped by the GLOBAL backstop, not its own size — wrong reason attached:\n%s", smallLine)
	}
}

// TestWriteCappedFiles_ListIsCapped guards against an unbounded capped-file
// list turning the note into most of the response when many files are dropped.
func TestWriteCappedFiles_ListIsCapped(t *testing.T) {
	capped := make([]cappedFile, 25)
	for i := range capped {
		capped[i] = cappedFile{Path: fmt.Sprintf("f%d.go", i), Bytes: 200 * 1024}
	}
	var sb strings.Builder
	writeCappedFiles(&sb, capped)
	out := sb.String()

	if n := strings.Count(out, ".go ("); n != maxCappedFilesListed {
		t.Errorf("want exactly %d files spelled out, got %d:\n%s", maxCappedFilesListed, n, out)
	}
	if !strings.Contains(out, "and 15 more") {
		t.Errorf("want the remaining 15 files summarised, got:\n%s", out)
	}
}

// lineContaining returns the first line of s containing needle, for asserting
// on one entry's reason without matching another entry's line by accident.
func lineContaining(s, needle string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}
