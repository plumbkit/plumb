package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// git_amend_wait_test.go covers PLAN-493: `amend` (folding a fix into HEAD, with
// the published-history guard) and `wait` (the caller asking for the child's real
// result instead of a detach notice, which is what a slow pre-commit hook made
// impossible). The detach notice itself stays the default: a call that does not
// ask to wait must still return promptly.

// slowHookPolicy is a policy whose detach deadline fires long before the hook
// below finishes, so the two paths are distinguishable inside a fast test.
func slowHookPolicy() GitPolicyFn {
	return func() GitPolicy {
		return GitPolicy{AllowWrites: true, DetachAfter: 200 * time.Millisecond, WriteTimeout: 30 * time.Second}
	}
}

// writePreCommitHook installs a hook that waits, then prints `message` to stderr
// and exits with `code`.
func writePreCommitHook(t *testing.T, dir, message string, sleep int, code int) {
	t.Helper()
	script := "#!/bin/sh\nsleep " + strconv.Itoa(sleep) + "\necho '" + message + "' >&2\nexit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(filepath.Join(dir, ".git", "hooks", "pre-commit"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestGit_CommitAmendFoldsIntoHead(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)
	before := strings.TrimSpace(gitReadOutput(t, dir, "rev-list", "--count", "HEAD"))
	if err := os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init\nfolded\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "init.txt")

	tool := NewGit(WriteDeps{}, func() GitPolicy { return GitPolicy{AllowWrites: true} })
	out, err := callGit(t, tool, map[string]any{
		"subcommand": "commit", "message": "initial commit (amended)", "amend": true, "repo": dir,
	})
	if err != nil {
		t.Fatalf("amend: %v", err)
	}
	if after := strings.TrimSpace(gitReadOutput(t, dir, "rev-list", "--count", "HEAD")); after != before {
		t.Errorf("commit count went %s -> %s: amend added a commit instead of folding into HEAD", before, after)
	}
	if subj := strings.TrimSpace(gitReadOutput(t, dir, "log", "-1", "--format=%s")); subj != "initial commit (amended)" {
		t.Errorf("amended subject = %q, want the new message", subj)
	}
	if !strings.Contains(out, "initial commit (amended)") {
		t.Errorf("the call did not report the amended commit: %q", out)
	}
	if body := gitReadOutput(t, dir, "show", "HEAD:init.txt"); !strings.Contains(body, "folded") {
		t.Errorf("the staged change is not in the amended commit:\n%s", body)
	}
}

func TestGit_CommitAmendKeepsMessageWhenNoneGiven(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init\nfolded\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "init.txt")

	tool := NewGit(WriteDeps{}, func() GitPolicy { return GitPolicy{AllowWrites: true} })
	if _, err := callGit(t, tool, map[string]any{
		"subcommand": "commit", "amend": true, "repo": dir,
	}); err != nil {
		t.Fatalf("amend without a message must keep HEAD's message: %v", err)
	}
	if subj := strings.TrimSpace(gitReadOutput(t, dir, "log", "-1", "--format=%s")); subj != "initial commit" {
		t.Errorf("subject = %q, want HEAD's original message kept", subj)
	}
	if body := gitReadOutput(t, dir, "show", "HEAD:init.txt"); !strings.Contains(body, "folded") {
		t.Errorf("the staged change is not in the amended commit:\n%s", body)
	}
}

func TestGit_CommitAmendRefusedWhenHeadIsPublished(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)
	gitRun(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init\nfolded\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "init.txt")

	tool := NewGit(WriteDeps{}, func() GitPolicy { return GitPolicy{AllowWrites: true} })
	_, err := callGit(t, tool, map[string]any{
		"subcommand": "commit", "message": "rewrite", "amend": true, "repo": dir,
	})
	if err == nil {
		t.Fatal("amending a published HEAD must be refused")
	}
	for _, want := range []string{"origin/main", "force-push"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal is missing %q; got: %v", want, err)
		}
	}
	if subj := strings.TrimSpace(gitReadOutput(t, dir, "log", "-1", "--format=%s")); subj != "initial commit" {
		t.Errorf("the refused amend still rewrote HEAD (subject %q)", subj)
	}

	// Positive control: with the remote ref gone the same amend is allowed, so the
	// guard is about publication and not about amending at all.
	gitRun(t, dir, "update-ref", "-d", "refs/remotes/origin/main")
	if _, err := callGit(t, tool, map[string]any{
		"subcommand": "commit", "message": "rewrite", "amend": true, "repo": dir,
	}); err != nil {
		t.Fatalf("amend after the remote ref was removed: %v", err)
	}
}

func TestGit_AmendAppliesToCommitOnly(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)
	tool := NewGit(WriteDeps{}, func() GitPolicy { return GitPolicy{AllowWrites: true, AllowDestructive: true} })
	_, err := callGit(t, tool, map[string]any{
		"subcommand": "add", "files": []string{filepath.Join(dir, "init.txt")}, "amend": true, "repo": dir,
	})
	if err == nil || !strings.Contains(err.Error(), "applies to commit") {
		t.Fatalf("amend on a non-commit subcommand = %v, want a refusal naming commit", err)
	}
}

func TestGit_WaitReturnsTheCommitResultInsteadOfDetaching(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)
	writePreCommitHook(t, dir, "hook ran", 1, 0)
	if err := os.WriteFile(filepath.Join(dir, "init.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "init.txt")

	tool := NewGit(WriteDeps{}, slowHookPolicy())
	out, err := callGit(t, tool, map[string]any{
		"subcommand": "commit", "message": "slow but fine", "wait": true, "repo": dir,
	})
	if err != nil {
		t.Fatalf("wait:true must return the real result: %v", err)
	}
	if strings.Contains(out, "STILL RUNNING") {
		t.Errorf("wait:true still detached:\n%s", out)
	}
	if !strings.Contains(out, "slow but fine") {
		t.Errorf("the commit result is missing from the response: %q", out)
	}
	if n := strings.TrimSpace(gitReadOutput(t, dir, "rev-list", "--count", "HEAD")); n != "2" {
		t.Errorf("commit count = %s, want 2 (the commit must have landed)", n)
	}
}

// TestGit_WaitSurfacesARefusingHooksOutput is the card's other half: when the
// hook refuses, the caller needs the hook's own words, not "STILL RUNNING" plus
// a manual lint re-run.
func TestGit_WaitSurfacesARefusingHooksOutput(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)
	writePreCommitHook(t, dir, "hook says: gocyclo too high", 1, 1)
	if err := os.WriteFile(filepath.Join(dir, "init.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "init.txt")

	tool := NewGit(WriteDeps{}, slowHookPolicy())
	_, err := callGit(t, tool, map[string]any{
		"subcommand": "commit", "message": "refused", "wait": true, "repo": dir,
	})
	if err == nil {
		t.Fatal("a refusing hook must fail the commit")
	}
	if !strings.Contains(err.Error(), "gocyclo too high") {
		t.Errorf("the hook's own output is missing from the failure:\n%v", err)
	}
	if n := strings.TrimSpace(gitReadOutput(t, dir, "rev-list", "--count", "HEAD")); n != "1" {
		t.Errorf("commit count = %s, want 1 (the refused commit must not have landed)", n)
	}
}

// TestGit_WithoutWaitStillDetaches keeps the default: the notice is what a
// caller who did not ask to wait still gets, and the next call on the
// repository reports the outcome.
func TestGit_WithoutWaitStillDetaches(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)
	writePreCommitHook(t, dir, "hook ran", 1, 0)
	if err := os.WriteFile(filepath.Join(dir, "init.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "init.txt")

	tool := NewGit(WriteDeps{}, slowHookPolicy())
	out, err := callGit(t, tool, map[string]any{"subcommand": "commit", "message": "detached", "repo": dir})
	if err != nil {
		t.Fatalf("a detached commit is a success result, not an error: %v", err)
	}
	if !strings.Contains(out, "STILL RUNNING") {
		t.Errorf("without wait the call must return the detach notice:\n%s", out)
	}
	if err := waitGitBackground(context.Background(), dir); err != nil {
		t.Fatalf("waiting for the detached child: %v", err)
	}
	// The next call on this repository reports what the detached child did.
	out, err = callGit(t, tool, map[string]any{"subcommand": "log", "args": []string{"-1", "--format=%s"}, "repo": dir})
	if err != nil {
		t.Fatalf("log after a detached commit: %v", err)
	}
	if !strings.Contains(out, "detached") {
		t.Errorf("the outcome of the detached commit was not reported on the next call:\n%s", out)
	}
}

func TestGit_WaitOnAReadIsHarmless(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)
	tool := NewGit(WriteDeps{}, slowHookPolicy())
	out, err := callGit(t, tool, map[string]any{
		"subcommand": "log", "args": []string{"-1", "--format=%s"}, "wait": true, "repo": dir,
	})
	if err != nil {
		t.Fatalf("wait on a read: %v", err)
	}
	if !strings.Contains(out, "initial commit") {
		t.Errorf("read with wait returned %q", out)
	}
}

func TestGitSchema_AdvertisesAmendAndWait(t *testing.T) {
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(gitSchema, &schema); err != nil {
		t.Fatalf("gitSchema is not valid JSON: %v", err)
	}
	for _, name := range []string{"amend", "wait"} {
		if _, ok := schema.Properties[name]; !ok {
			t.Errorf("git schema does not advertise %q", name)
		}
	}
}
