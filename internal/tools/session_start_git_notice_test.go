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
				`plumb trust '/tmp/ws'`,
				`plumb config show --workspace '/tmp/ws'`,
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
			// B2/B-1. The three routes land at DIFFERENT moments, and one sentence
			// covering all of them was wrong in both directions. `plumb trust` is
			// watched by nothing, so it cannot change this session; the GLOBAL config
			// is watched, so it can; and PLUMB_GIT_* is read from the daemon PROCESS's
			// environment, so it needs a daemon restart — strictly more than the "new
			// session, or a re-pin" the old text offered as sufficient.
			name:   "the remediation states the timing per route",
			st:     ProjectGitStatus{Keys: gitKeys("git.allow_push", true)},
			policy: closed,
			wantContain: []string{
				"--yes",
				"`plumb trust` writes a file nothing watches",
				"next attached",
				"NOT mid-session",
				"Editing the GLOBAL config DOES take effect mid-session",
				"read from the DAEMON's environment",
				"the daemon has to be restarted with the variable already set",
			},
			// The generalised claim: a blanket "NOT mid-session" covering every
			// route, and an env remediation that offers a new session or a re-pin as
			// enough. Both are false; neither may come back.
			wantAbsent: []string{
				"Either takes effect when this workspace is next attached",
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
			// The ordinary trusted session: the grant is what put these values in
			// force, so a notice would report a working feature. Note what makes it
			// silent — the per-key comparison, not a Trusted exemption. That is the
			// whole of what the removed short-circuit was doing, which is why deleting
			// it changed no output anywhere and the state below went unguarded.
			name:      "trusted git keys that ARE in force: silent",
			st:        ProjectGitStatus{Keys: gitKeys("git.allow_push", true), Trusted: true},
			policy:    GitPolicy{AllowWrites: true, AllowPush: true},
			wantEmpty: true,
		},
		{
			// B-3. Trust is granted and the key is STILL not in force, because
			// LoadProjectWithPolicy applies PLUMB_GIT_* after the project config:
			// `allow_push = true`, approved, plus PLUMB_GIT_ALLOW_PUSH=0 resolves to
			// push off. A `if st.Trusted { return "" }` short-circuit silences exactly
			// this — restoring the original bug (an unexplained `Push/fetch/pull:
			// off.` against a value the agent knows was approved) from the other side.
			// This case is what fails if that short-circuit comes back.
			name:   "trusted but overridden: named, with the env as the cause",
			st:     ProjectGitStatus{Keys: gitKeys("git.allow_push", true), Trusted: true},
			policy: GitPolicy{AllowWrites: true, AllowPush: false},
			wantContain: []string{
				"OVERRIDDEN",
				"TRUSTED",
				"still NOT in force: git.allow_push",
				"`plumb trust` will NOT help",
				"PLUMB_GIT_ALLOW_PUSH",
				"applied AFTER the project config",
				"restart the daemon",
			},
			// The untrusted notice's advice is actively wrong here: the grant is
			// already given, so recommending the command would send the reader to
			// re-approve something that is not the obstacle.
			wantAbsent: []string{`plumb trust '/tmp/ws'`, "IGNORED"},
		},
		{
			// The asymmetry that keeps the trusted branch honest. A trusted [git]
			// table is applied WHOLE, and `git.env` has no counterpart in GitPolicy to
			// compare against — so reporting it would invent an override that does not
			// exist. Untrusted, the same key IS reported (the case below), because
			// there the whole table really was dropped.
			name:      "trusted and uncomparable (git.env): silent, not invented",
			st:        ProjectGitStatus{Keys: gitKeys("git.env", map[string]any{"GIT_SSH_COMMAND": "x"}), Trusted: true},
			policy:    GitPolicy{AllowWrites: true},
			wantEmpty: true,
		},
		{
			name:        "untrusted and uncomparable (git.env): reported",
			st:          ProjectGitStatus{Keys: gitKeys("git.env", map[string]any{"GIT_SSH_COMMAND": "x"})},
			policy:      GitPolicy{AllowWrites: true},
			wantContain: []string{"IGNORED", "git.env"},
		},
		{
			// Same rule for a field nobody classified: untrusted it is reported (it
			// was dropped), trusted it is not (it was applied).
			name:      "trusted and unrecognised field: silent",
			st:        ProjectGitStatus{Keys: gitKeys("git.allow_pushes", true), Trusted: true},
			policy:    GitPolicy{AllowWrites: true},
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
			// N3/B-2. An unparseable project config is skipped whole, so its [git]
			// block is just as ignored — with no `plumb trust` to reach for.
			//
			// It may now also say what the policy IS, because a failed load reverts to
			// the global config instead of leaving the last successful one standing
			// (PLAN-309). The wantAbsent list is the load-bearing half: the notice used
			// to describe a carryover — possibly another workspace's trusted grant —
			// and repeating that now would send a reader hunting an elevation that can
			// no longer happen. internal/cli's TestProjectGitStatus_RePinDropsThePreviousGrant
			// pins the behaviour these strings depend on.
			name:   "an unparseable project config says so, and that the policy is the global one",
			st:     ProjectGitStatus{Unreadable: true},
			policy: closed,
			wantContain: []string{
				"IGNORED",
				"could not be parsed",
				"skipped WHOLE",
				"The policy above is the GLOBAL one",
				"treated as no config at all",
				"previously pinned to",
				`plumb config show --workspace '/tmp/ws'`,
			},
			wantAbsent: []string{
				"NOTHING in it is being applied",
				"That does NOT mean the policy above is the global one",
				"re-pinned here from another workspace",
				"trusted [git] grant included",
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
	for _, want := range []string{`plumb trust '/Users/me/My Project'`, `--workspace '/Users/me/My Project'`} {
		if !strings.Contains(got, want) {
			t.Errorf("want %s in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "plumb trust /Users/me/My Project") {
		t.Errorf("the command must be quoted, or a space-bearing path is truncated:\n%s", got)
	}
}

// TestShellQuote_SuppressesExpansion is the other half of that: Go's %q is not a
// shell quote. It escapes quotes, backslashes and non-printables, but leaves `$`
// and backticks alone, so a path under a directory literally named `$WORK` (or
// one holding a command substitution) is rewritten by the shell before `plumb
// trust` ever sees it — the same silent wrong-target as the unquoted-space case,
// reached by a rarer spelling and with a worse tail.
func TestShellQuote_SuppressesExpansion(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"plain", "/tmp/ws", `'/tmp/ws'`},
		{"space", "/Users/me/My Project", `'/Users/me/My Project'`},
		{"dollar", "/Users/me/$WORK/proj", `'/Users/me/$WORK/proj'`},
		{"backtick", "/tmp/`id`", "'/tmp/`id`'"},
		{"double quote", `/tmp/a"b`, `'/tmp/a"b'`},
		{"single quote", "/tmp/it's", `'/tmp/it'\''s'`},
		{"backslash", `/tmp/a\b`, `'/tmp/a\b'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := shellQuote(tc.in); got != tc.want {
				t.Errorf("shellQuote(%q) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}

	// The claim is behavioural, not textual: whatever the quoting, the shell must
	// hand the command back exactly the path we put in. `printf %s` is the
	// narrowest way to observe what an argument resolved to.
	for _, ws := range []string{
		"/Users/me/My Project", "/Users/me/$WORK/proj", "/tmp/`id`", "/tmp/it's", `/tmp/a\b`, `/tmp/a"b`, "/tmp/a b$x'y",
	} {
		out, err := exec.Command("/bin/sh", "-c", "printf %s "+shellQuote(ws)).Output()
		if err != nil {
			t.Fatalf("sh -c for %q: %v", ws, err)
		}
		if string(out) != ws {
			t.Errorf("the shell resolved %s to %q, want %q — the notice would target the wrong directory",
				shellQuote(ws), out, ws)
		}
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
