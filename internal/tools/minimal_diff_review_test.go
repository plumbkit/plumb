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
)

// newReviewTool builds the tool pinned to ws with no topology store (a nil
// storeFn): the diff-only checks need none, and the topology-backed checks then
// degrade and say so in the limits section.
func newReviewTool(ws string) *MinimalDiffReview {
	return NewMinimalDiffReview(nil).WithWorkspace(func() string { return ws })
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
	tool := NewMinimalDiffReview(nil).WithWorkspace(func() string { return "" })
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
	for i := 0; i < 12; i++ {
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
