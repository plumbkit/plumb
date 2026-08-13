package tools

import (
	"os/exec"
	"strings"
	"testing"
)

// gitKeys is shorthand for a project [git] request in the tests below.
func gitKeys(pairs ...any) []ProjectGitKey {
	out := make([]ProjectGitKey, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, ProjectGitKey{Key: pairs[i].(string), Value: pairs[i+1]})
	}
	return out
}

// TestFormatProjectGitNotice covers the pure notice formatter. The load-bearing
// cases are the ones the silent drop, and then the notice itself, got wrong: a
// project [git] block must be named as ignored even when its value raises no
// privilege, a project that sets no [git] key must produce no output, and a key
// whose requested value the session ALREADY resolved must not be claimed as "NOT
// in force" with a no-op remediation attached.
func TestFormatProjectGitNotice(t *testing.T) {
	const ws = "/tmp/ws"
	closed := GitPolicy{AllowWrites: true}
	tests := []struct {
		name        string
		st          ProjectGitStatus
		policy      GitPolicy
		wantEmpty   bool
		wantContain []string
		wantAbsent  []string
	}{
		{
			name:   "untrusted git keys are named, with the why and the how",
			st:     ProjectGitStatus{Keys: gitKeys("git.allow_destructive", true, "git.allow_push", true)},
			policy: closed,
			wantContain: []string{
				"IGNORED",
				".plumb/config.toml",
				"sets 2 [git] keys that are NOT in force",
				"git.allow_destructive, git.allow_push",
				"untrusted input",
				"cloning a repository ships one",
				`plumb trust "/tmp/ws"`,
				`plumb config show --workspace "/tmp/ws"`,
				"PLUMB_GIT_ALLOW_DESTRUCTIVE",
				"PLUMB_GIT_ALLOW_PUSH",
			},
		},
		{
			name:   "a single key agrees its noun and verb",
			st:     ProjectGitStatus{Keys: gitKeys("git.protected_branches", []any{"main"})},
			policy: closed,
			wantContain: []string{
				"sets 1 [git] key that is NOT in force",
				"git.protected_branches",
			},
			wantAbsent: []string{"[git] keys", "are NOT in force"},
		},
		{
			// The regression the notice exists to prevent: a present-but-false value
			// looks exactly like an absent one downstream, so presence — not privilege
			// — must drive it. Here the tier is OPEN and the project asked to close
			// it: overruled just the same, and the reader has the same question.
			name:        "allow_destructive = false against an open tier is reported",
			st:          ProjectGitStatus{Keys: gitKeys("git.allow_destructive", false)},
			policy:      GitPolicy{AllowWrites: true, AllowDestructive: true},
			wantContain: []string{"IGNORED", "git.allow_destructive"},
		},
		{
			// S1. With the tiers already granted globally, the project's request IS
			// what is in force. Naming it "NOT in force" is false, and the `plumb
			// trust` it recommends would change nothing — a common shape for anyone
			// who enabled the tiers globally and then cloned a repo that asks too.
			name: "keys whose requested value is already in force are not claimed as ignored",
			st: ProjectGitStatus{Keys: gitKeys(
				"git.allow_destructive", true,
				"git.allow_push", true,
			)},
			policy:    GitPolicy{AllowWrites: true, AllowDestructive: true, AllowPush: true},
			wantEmpty: true,
		},
		{
			// Only the key that genuinely differs is named, so every line of the
			// notice is checkable against the policy printed above it.
			name: "a mixed request names only the key that differs",
			st: ProjectGitStatus{Keys: gitKeys(
				"git.allow_destructive", true,
				"git.allow_push", true,
			)},
			policy:      GitPolicy{AllowWrites: true, AllowDestructive: true},
			wantContain: []string{"sets 1 [git] key that is NOT in force", "git.allow_push"},
			wantAbsent:  []string{"git.allow_destructive"},
		},
		{
			name:      "protected_branches matching the resolved list is not reported",
			st:        ProjectGitStatus{Keys: gitKeys("git.protected_branches", []any{"main", "release"})},
			policy:    GitPolicy{AllowWrites: true, ProtectedBranches: []string{"main", "release"}},
			wantEmpty: true,
		},
		{
			// A [git] field nobody classified here cannot be compared, so it is
			// reported rather than silently assumed satisfied.
			name:        "an unrecognised git field is reported, not assumed in force",
			st:          ProjectGitStatus{Keys: gitKeys("git.allow_pushes", true)},
			policy:      GitPolicy{AllowWrites: true, AllowPush: true},
			wantContain: []string{"IGNORED", "git.allow_pushes"},
		},
		{
			// S2. The whole [git] table is forced back, so the notice must name the
			// keys and env vars for the fields that are not tiers, and must not give a
			// privilege-raising reason for a request that RESTRICTS.
			name:   "a restricting request gets the both-directions reason and its own env var",
			st:     ProjectGitStatus{Keys: gitKeys("git.allow_writes", false, "git.commit_trailer", true)},
			policy: closed,
			wantContain: []string{
				"git.allow_writes, git.commit_trailer",
				"both directions",
				"PLUMB_GIT_ALLOW_WRITES",
				"PLUMB_GIT_COMMIT_TRAILER",
			},
		},
		{
			// B2. `plumb trust` is not watched by anything, so it cannot change this
			// session; promising otherwise walks the reader into a silent notice with
			// an unchanged policy.
			name:   "the remediation does not promise an in-session effect",
			st:     ProjectGitStatus{Keys: gitKeys("git.allow_push", true)},
			policy: closed,
			wantContain: []string{
				"next attached",
				"NOT mid-session",
				"--yes",
			},
		},
		{
			name:      "no project [git] table: silent",
			st:        ProjectGitStatus{},
			policy:    closed,
			wantEmpty: true,
		},
		{
			// Quiet in the other common case too: only non-git capability keys.
			name:      "only lsp/collab keys: silent (this is the git section)",
			st:        ProjectGitStatus{Keys: gitKeys("lsp.go.command", "/bin/sh", "collab.cross_project", true)},
			policy:    closed,
			wantEmpty: true,
		},
		{
			// Trusted means the keys ARE in force, so the policy printed above is
			// already the project's — a notice would report a working feature.
			name:      "trusted git keys: silent",
			st:        ProjectGitStatus{Keys: gitKeys("git.allow_push", true), Trusted: true},
			policy:    GitPolicy{AllowWrites: true, AllowPush: true},
			wantEmpty: true,
		},
		{
			// go-toml/v2 binds `[GIT] Allow_Push` to the same fields, so a fold
			// variant reaches the merged config and must reach the notice too — and be
			// compared against the policy under its canonical name, not reported
			// blindly.
			name:        "fold-variant table and key are matched",
			st:          ProjectGitStatus{Keys: gitKeys("GIT.Allow_Push", true)},
			policy:      closed,
			wantContain: []string{"IGNORED", "GIT.Allow_Push"},
		},
		{
			name:      "fold-variant key already in force is compared, not reported",
			st:        ProjectGitStatus{Keys: gitKeys("GIT.Allow_Push", true)},
			policy:    GitPolicy{AllowWrites: true, AllowPush: true},
			wantEmpty: true,
		},
		{
			// N3. An unparseable project config is skipped whole, so its [git] block
			// is just as ignored — with no `plumb trust` to reach for.
			name:   "an unparseable project config says so",
			st:     ProjectGitStatus{Unreadable: true},
			policy: closed,
			wantContain: []string{
				"IGNORED",
				"could not be parsed",
				"NOTHING in it is being applied",
				`plumb config show --workspace "/tmp/ws"`,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatProjectGitNotice(ws, tc.st, tc.policy)
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

// TestFormatProjectGitNotice_QuotesTheWorkspacePath pins S3. An agent copies the
// remediation verbatim; unquoted, `plumb trust /Users/me/My Project` reaches
// trust as `/Users/me/My` and the advice silently targets the wrong directory.
func TestFormatProjectGitNotice_QuotesTheWorkspacePath(t *testing.T) {
	const ws = "/Users/me/My Project"
	got := formatProjectGitNotice(ws, ProjectGitStatus{Keys: gitKeys("git.allow_push", true)}, GitPolicy{AllowWrites: true})
	for _, want := range []string{`plumb trust "/Users/me/My Project"`, `--workspace "/Users/me/My Project"`} {
		if !strings.Contains(got, want) {
			t.Errorf("want %s in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "plumb trust /Users/me/My Project") {
		t.Errorf("the command must be quoted, or a space-bearing path is truncated:\n%s", got)
	}
}

// TestSessionStart_ProjectGitNoticeSection verifies the rendering seam: that
// writeSessionGitPolicy consults the accessor, places the notice AFTER the
// resolved policy it annotates, and adds nothing when the accessor is absent or
// has nothing to report.
//
// It proves nothing about whether the daemon wires the accessor up — it injects
// its own stub. That wiring is the one deletion every other gate survives, and
// it is guarded structurally in internal/cli (TestSessionStartWiring_Required).
func TestSessionStart_ProjectGitNoticeSection(t *testing.T) {
	ws := t.TempDir()
	if out, err := exec.Command("git", "init", ws).CombinedOutput(); err != nil {
		t.Skipf("git init unavailable: %v (%s)", err, out)
	}
	policy := func() GitPolicy { return GitPolicy{AllowWrites: true} }
	render := func(tool *SessionStart) string {
		var sb strings.Builder
		tool.writeSessionGitPolicy(&sb, ws)
		return sb.String()
	}

	t.Run("wired and untrusted: notice rendered under the policy", func(t *testing.T) {
		out := render(NewSessionStart(func() string { return ws }, nil, nil, nil, nil, policy).
			WithProjectPolicy(func() ProjectGitStatus {
				return ProjectGitStatus{Keys: gitKeys("git.allow_push", true)}
			}))
		gate := strings.Index(out, "Push/fetch/pull: off.")
		notice := strings.Index(out, "IGNORED")
		if gate < 0 {
			t.Fatalf("expected the resolved policy in:\n%s", out)
		}
		if notice < 0 || !strings.Contains(out, "git.allow_push") {
			t.Fatalf("expected the ignored-[git] notice in:\n%s", out)
		}
		// The order is the claim the docs make: the reader meets the policy first,
		// then the reason it looks wrong. Reversed, the notice explains something
		// not yet on screen.
		if notice < gate {
			t.Errorf("the notice must follow the policy it annotates, got:\n%s", out)
		}
	})

	t.Run("unwired accessor: section unchanged", func(t *testing.T) {
		if out := render(NewSessionStart(func() string { return ws }, nil, nil, nil, nil, policy)); strings.Contains(out, "IGNORED") {
			t.Errorf("unwired accessor must add nothing, got:\n%s", out)
		}
	})

	t.Run("wired but project asks for nothing: section unchanged", func(t *testing.T) {
		out := render(NewSessionStart(func() string { return ws }, nil, nil, nil, nil, policy).
			WithProjectPolicy(func() ProjectGitStatus { return ProjectGitStatus{} }))
		if strings.Contains(out, "IGNORED") {
			t.Errorf("a project that asks for nothing must stay quiet, got:\n%s", out)
		}
	})
}
