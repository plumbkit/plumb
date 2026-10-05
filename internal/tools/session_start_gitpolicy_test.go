package tools

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// session_start_gitpolicy_test.go: the git policy session_start reports — the
// pure formatter and the packet section built from it.

// TestFormatGitPolicy covers the pure policy formatter: the shell-avoidance
// steer appears only when writes are enabled, and the "trust it over any cached
// note" line is always present (it is the line that contradicts a stale
// "git is read-only" memory at the point of orientation).
func TestFormatGitPolicy(t *testing.T) {
	const trust = "trust it over any cached note"
	const steer = "commit through the `git` tool, not the shell"
	tests := []struct {
		name        string
		policy      GitPolicy
		wantContain []string
		wantAbsent  []string
	}{
		{
			name:   "default: writes on, destructive/push off",
			policy: GitPolicy{AllowWrites: true, ProtectedBranches: []string{"main", "master"}},
			wantContain: []string{
				"Commits & staging ENABLED", steer,
				"Destructive (reset/checkout/rebase): off.",
				"Push/fetch/pull: off.",
				"Protected branches: main, master.",
				trust,
			},
		},
		{
			name:   "all gates on",
			policy: GitPolicy{AllowWrites: true, AllowDestructive: true, AllowPush: true, ProtectedBranches: []string{"main"}},
			wantContain: []string{
				"Destructive (reset/checkout/rebase): on.",
				"Push/fetch/pull: on.",
				"Protected branches: main.",
				trust,
			},
		},
		{
			name:        "writes disabled",
			policy:      GitPolicy{AllowWrites: false},
			wantContain: []string{"Read-only", "`[git] allow_writes = false`", trust},
			wantAbsent:  []string{"Commits & staging ENABLED", steer},
		},
		{
			name:        "writes on, no protected branches",
			policy:      GitPolicy{AllowWrites: true},
			wantContain: []string{"Commits & staging ENABLED", trust},
			wantAbsent:  []string{"Protected branches:"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatGitPolicy(tc.policy)
			for _, want := range tc.wantContain {
				if !strings.Contains(got, want) {
					t.Errorf("want %q in:\n%s", want, got)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("did not want %q in:\n%s", absent, got)
				}
			}
		})
	}
}

// TestSessionStart_GitPolicySection verifies the section is wired into Execute:
// rendered inside a git repo when the policy is wired, and omitted both when
// gitPolicyFn is nil and when the workspace is not a git repo.
func TestSessionStart_GitPolicySection(t *testing.T) {
	const header = "## Git (via the `git` tool"
	writesOn := func() GitPolicy {
		return GitPolicy{AllowWrites: true, ProtectedBranches: []string{"main", "master"}}
	}
	gitInit := func(t *testing.T) string {
		t.Helper()
		ws := t.TempDir()
		if out, err := exec.Command("git", "init", ws).CombinedOutput(); err != nil {
			t.Skipf("git init unavailable: %v (%s)", err, out)
		}
		return ws
	}

	t.Run("rendered in a git repo when policy wired", func(t *testing.T) {
		ws := gitInit(t)
		tool := NewSessionStart(func(context.Context) string { return ws }, nil, nil, nil, func() string { return "" }, writesOn)
		out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !strings.Contains(out, header) {
			t.Errorf("want git policy section in a git repo\n%s", out)
		}
		if !strings.Contains(out, "Commits & staging ENABLED") {
			t.Errorf("want ENABLED policy body\n%s", out)
		}
	})

	t.Run("omitted when gitPolicyFn is nil", func(t *testing.T) {
		ws := gitInit(t)
		tool := NewSessionStart(func(context.Context) string { return ws }, nil, nil, nil, func() string { return "" }, nil)
		out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if strings.Contains(out, header) {
			t.Errorf("git policy section should be omitted when gitPolicyFn is nil\n%s", out)
		}
	})

	t.Run("omitted outside a git repo", func(t *testing.T) {
		// A path with no git repo above it: `git -C <missing> branch` errors, so
		// gitBranch returns "" and the section is gated off. t.TempDir() alone
		// won't do — in this repo the test temp root lives inside the worktree,
		// so git would resolve the enclosing plumb repo and report a branch.
		ws := filepath.Join(t.TempDir(), "no-such-dir")
		tool := NewSessionStart(func(context.Context) string { return ws }, nil, nil, nil, func() string { return "" }, writesOn)
		out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if strings.Contains(out, header) {
			t.Errorf("git policy section should be omitted outside a git repo\n%s", out)
		}
	})
}
