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
