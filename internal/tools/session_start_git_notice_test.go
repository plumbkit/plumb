package tools

import (
	"os/exec"
	"strings"
	"testing"
)

// TestFormatProjectGitNotice covers the pure notice formatter. The load-bearing
// cases are the two that the silent drop got wrong: a project [git] block must
// be named as ignored EVEN WHEN its value raises no privilege
// (allow_destructive = false is indistinguishable from absent in the decoded
// config, and the user who wrote it has the same "why did nothing happen?"), and
// a project that sets no [git] key at all must produce no output whatsoever.
func TestFormatProjectGitNotice(t *testing.T) {
	const ws = "/tmp/ws"
	tests := []struct {
		name        string
		keys        []string
		trusted     bool
		wantEmpty   bool
		wantContain []string
		wantAbsent  []string
	}{
		{
			name: "untrusted git keys are named, with the why and the how",
			keys: []string{"git.allow_destructive", "git.allow_push"},
			wantContain: []string{
				"IGNORED",
				".plumb/config.toml",
				"sets 2 [git] keys that are NOT in force",
				"git.allow_destructive, git.allow_push",
				"untrusted input",
				"cloning a repository ships one",
				"plumb trust /tmp/ws",
				"plumb config show --workspace /tmp/ws",
				"PLUMB_GIT_ALLOW_DESTRUCTIVE",
				"PLUMB_GIT_ALLOW_PUSH",
			},
		},
		{
			name: "a single key agrees its noun and verb",
			keys: []string{"git.protected_branches"},
			wantContain: []string{
				"sets 1 [git] key that is NOT in force",
				"git.protected_branches",
			},
			wantAbsent: []string{"[git] keys", "are NOT in force"},
		},
		{
			// The regression this whole change exists to prevent: a present-but-false
			// value looks exactly like an absent one downstream, so presence — not
			// value — must drive the notice.
			name:        "allow_destructive = false is still reported as ignored",
			keys:        []string{"git.allow_destructive"},
			wantContain: []string{"IGNORED", "git.allow_destructive"},
		},
		{
			name:      "no project [git] table: silent",
			keys:      nil,
			wantEmpty: true,
		},
		{
			// Quiet in the other common case too: only non-git capability keys.
			name:      "only lsp/collab keys: silent (this is the git section)",
			keys:      []string{"lsp.go.command", "collab.cross_project"},
			wantEmpty: true,
		},
		{
			// Trusted means the keys ARE in force, so the policy printed above is
			// already the project's — a notice would report a working feature.
			name:      "trusted git keys: silent",
			keys:      []string{"git.allow_push"},
			trusted:   true,
			wantEmpty: true,
		},
		{
			// go-toml/v2 binds `[GIT] Allow_Push` to the same fields, so a fold
			// variant reaches the merged config and must reach the notice too.
			name:        "fold-variant table and key are matched",
			keys:        []string{"GIT.Allow_Push"},
			wantContain: []string{"IGNORED", "GIT.Allow_Push"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatProjectGitNotice(ws, tc.keys, tc.trusted)
			if tc.wantEmpty {
				if got != "" {
					t.Errorf("want no notice, got:\n%s", got)
				}
				return
			}
			if got == "" {
				t.Fatal("want a notice, got none")
			}
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

// TestSessionStart_ProjectGitNoticeSection verifies the notice is wired into the
// rendered git section — that it reaches an agent, not just a unit test — and
// that an unwired accessor leaves the section exactly as it was.
func TestSessionStart_ProjectGitNoticeSection(t *testing.T) {
	ws := t.TempDir()
	if out, err := exec.Command("git", "init", ws).CombinedOutput(); err != nil {
		t.Skipf("git init unavailable: %v (%s)", err, out)
	}
	policy := func() GitPolicy { return GitPolicy{AllowWrites: true} }

	t.Run("wired and untrusted: notice rendered under the policy", func(t *testing.T) {
		tool := NewSessionStart(func() string { return ws }, nil, nil, nil, nil, policy).
			WithProjectPolicy(func(string) ([]string, bool) {
				return []string{"git.allow_push"}, false
			})
		var sb strings.Builder
		tool.writeSessionGitPolicy(&sb, ws)
		out := sb.String()
		if !strings.Contains(out, "Push/fetch/pull: off.") {
			t.Fatalf("expected the resolved policy in:\n%s", out)
		}
		if !strings.Contains(out, "IGNORED") || !strings.Contains(out, "git.allow_push") {
			t.Errorf("expected the ignored-[git] notice in:\n%s", out)
		}
	})

	t.Run("unwired accessor: section unchanged", func(t *testing.T) {
		tool := NewSessionStart(func() string { return ws }, nil, nil, nil, nil, policy)
		var sb strings.Builder
		tool.writeSessionGitPolicy(&sb, ws)
		if out := sb.String(); strings.Contains(out, "IGNORED") {
			t.Errorf("unwired accessor must add nothing, got:\n%s", out)
		}
	})

	t.Run("wired but project asks for nothing: section unchanged", func(t *testing.T) {
		tool := NewSessionStart(func() string { return ws }, nil, nil, nil, nil, policy).
			WithProjectPolicy(func(string) ([]string, bool) { return nil, false })
		var sb strings.Builder
		tool.writeSessionGitPolicy(&sb, ws)
		if out := sb.String(); strings.Contains(out, "IGNORED") {
			t.Errorf("a project that asks for nothing must stay quiet, got:\n%s", out)
		}
	})
}
