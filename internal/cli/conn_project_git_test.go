package cli

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/tools"
)

// conn_project_git_test.go — the WIRING half of the ignored-project-[git]
// notice.
//
// internal/tools proves the renderer. Nothing there proves the daemon hands it
// anything (it injects its own stub), nor that what it is handed describes the
// same moment as the policy printed beside it. Both were real defects: the
// decorator could be deleted with every gate still green, and the answer was
// recomputed from a trust store nothing watches, so `plumb trust` silenced the
// notice while the cached policy stayed exactly as restrictive.

// requiredSessionStartWiring names the session_start decorators whose absence
// deletes a user-visible feature WITHOUT failing anything else — the tool still
// builds, still registers, still answers, and the feature's own unit tests still
// pass because they inject their own accessor. Each entry is the literal call
// text (the chained calls carry their dot on the PRECEDING line, so it is not
// part of the match) and what is lost when it goes.
var requiredSessionStartWiring = map[string]string{
	"WithProjectPolicy(s.projectGitStatus)": "session_start never tells an agent its project [git] block was " +
		"overruled — the notice renders for nobody, and only a stub-injecting unit test still exercises it",
}

// TestSessionStartWiring_Required fails when a required decorator is missing
// from registerAllTools. It is the guard for a whole class of silent deletion:
// an accessor injected at registration has no compile-time obligation, so a
// dropped line leaves a feature dead and every other check green.
//
// Structural, in the same shape as TestBoundaryGuardWiringComplete and
// TestToolProfileClassification, because the alternative — booting a real
// session and reading the rendered packet — pins far more than the wiring.
func TestSessionStartWiring_Required(t *testing.T) {
	src, err := os.ReadFile("conn_register.go")
	if err != nil {
		t.Fatalf("reading conn_register.go: %v", err)
	}
	body := registerAllToolsBody(string(src))
	if body == "" {
		t.Fatal("could not locate registerAllTools in conn_register.go — was it renamed?")
	}
	start := strings.Index(body, "srv.Register(tools.NewSessionStart(")
	if start < 0 {
		t.Fatal("session_start is no longer registered in registerAllTools")
	}
	for call, lost := range requiredSessionStartWiring {
		if !strings.Contains(body[start:], call) {
			t.Errorf("session_start registration is missing %s — without it, %s", call, lost)
		}
	}
}

// projectGitSession attaches a session to ws with an isolated trust store and
// applies the project config exactly as the daemon does on attach.
func projectGitSession(t *testing.T, ws string) *connSession {
	t.Helper()
	return projectGitSessionWithBase(t, ws, config.Defaults())
}

func projectGitSessionWithBase(t *testing.T, ws string, base config.Config) *connSession {
	t.Helper()
	s := &connSession{ctx: context.Background(), store: config.NewStore(base)}
	s.mutate(func(v *sessionView) { v.acquiredRoot = ws })
	s.applyProjectConfig(ws)
	return s
}

func writeProjectConfig(t *testing.T, ws, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(ws, ".plumb"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".plumb", "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestProjectGitStatus_AgreesWithTheResolvedPolicy is the consistency contract.
//
// The notice claims something about the policy printed beside it, so the two
// must be one snapshot. They were not: the policy came from the cached view
// (written only by applyProjectConfig) while the notice re-read <DataDir>/
// trust.json on every session_start. `plumb trust` writes that file and nothing
// watches it, so the sequence an agent is told to follow — see the notice, relay
// `plumb trust`, re-run session_start — ended at `Push/fetch/pull: off.` with no
// explanation and the git tool still refusing: the pre-notice bug, reached by
// obeying the fix.
//
// Asserted as an invariant across the three states, not as an implementation
// detail: whatever projectGitStatus reports about trust, gitPolicy must agree.
func TestProjectGitStatus_AgreesWithTheResolvedPolicy(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws := t.TempDir()
	writeProjectConfig(t, ws, "[git]\nallow_push = true\n")

	s := projectGitSession(t, ws)
	assertGitConsistent := func(t *testing.T, when string) {
		t.Helper()
		st := s.projectGitStatus()
		if st.Unreadable {
			t.Fatalf("%s: a valid project config must not report as unparseable", when)
		}
		if got := gitStatusKeys(st.Keys); !slices.Equal(got, []string{"git.allow_push"}) {
			t.Errorf("%s: keys = %v, want [git.allow_push]", when, got)
		}
		if st.Trusted != s.gitPolicy().AllowPush {
			t.Errorf("%s: the notice says trusted=%v while the resolved policy says allow_push=%v — "+
				"they describe different moments, so one of them is lying to the agent",
				when, st.Trusted, s.gitPolicy().AllowPush)
		}
	}

	t.Run("at attach: untrusted, and the tier is closed", func(t *testing.T) {
		assertGitConsistent(t, "at attach")
		if s.projectGitStatus().Trusted {
			t.Error("a workspace nobody trusted must not report its [git] request as in force")
		}
	})

	t.Run("after plumb trust, before re-attach: both still say untrusted", func(t *testing.T) {
		grantExecTrust(t, ws)
		assertGitConsistent(t, "after the grant")
		if s.projectGitStatus().Trusted {
			t.Error("trust took effect in the notice without taking effect in the policy — " +
				"the notice would fall silent while the git tool kept refusing")
		}
	})

	t.Run("after re-attach: both flip together", func(t *testing.T) {
		s.applyProjectConfig(ws)
		assertGitConsistent(t, "after re-attach")
		if !s.projectGitStatus().Trusted || !s.gitPolicy().AllowPush {
			t.Fatalf("a re-attach must honour the grant: trusted=%v allow_push=%v",
				s.projectGitStatus().Trusted, s.gitPolicy().AllowPush)
		}
	})
}

// TestProjectGitStatus_TrustedButEnvOverridden proves the state the renderer's
// trusted branch exists for is REACHABLE through the real config pipeline, not a
// hypothetical the notice may as well stay silent about.
//
// LoadProjectWithPolicy applies PLUMB_GIT_* LAST — deliberately, so forcing an
// untrusted [git] back to base cannot discard an override the user set for this
// process — which means env outranks a trust grant just as it outranks the
// project file. So `allow_push = true`, approved, plus PLUMB_GIT_ALLOW_PUSH=0
// resolves to trusted=true AND allow_push=false: an agent seeing
// `Push/fetch/pull: off.` beside a value it knows was approved, which is the
// original unexplained-policy bug arrived at from the trusted side.
//
// A `if st.Trusted { return "" }` short-circuit in the renderer silences exactly
// this. Deleting that line changed no test's output — the per-key comparison
// already keeps the trusted-AND-applied case quiet — which is why the state went
// unguarded. internal/tools asserts what gets RENDERED here; this asserts that
// the daemon can actually produce it.
func TestProjectGitStatus_TrustedButEnvOverridden(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws := t.TempDir()
	writeProjectConfig(t, ws, "[git]\nallow_push = true\n")
	grantExecTrust(t, ws)

	// Without the override, the grant is honoured — the control that makes the
	// assertion below about the ENV and not about a broken trust grant.
	granted := projectGitSession(t, ws)
	if !granted.projectGitStatus().Trusted || !granted.gitPolicy().AllowPush {
		t.Fatalf("precondition: the grant must be honoured, got trusted=%v allow_push=%v",
			granted.projectGitStatus().Trusted, granted.gitPolicy().AllowPush)
	}

	t.Setenv("PLUMB_GIT_ALLOW_PUSH", "0")
	s := projectGitSession(t, ws)
	st := s.projectGitStatus()
	if !st.Trusted {
		t.Fatalf("the request is trusted for this exact content; got trusted=false")
	}
	if s.gitPolicy().AllowPush {
		t.Fatal("PLUMB_GIT_ALLOW_PUSH=0 must beat the project value — it is applied after the project config")
	}
	if got := gitStatusKeys(st.Keys); !slices.Equal(got, []string{"git.allow_push"}) {
		t.Errorf("keys = %v, want [git.allow_push] — the notice cannot name what it is not handed", got)
	}
}

func gitStatusKeys(keys []tools.ProjectGitKey) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = k.Key
	}
	return out
}

// TestApplyProjectConfig_UntrustedGitStaysGlobal is the security contract this
// change must not weaken.
//
// Making the notice consistent moved the TRUST FLAG into the session view, next
// to the git block it describes. That is a new place for trust state to live, so
// the enforcement it annotates is re-pinned here at the layer the git tool
// actually reads: whatever the notice says, an untrusted workspace resolves the
// WHOLE [git] table from the global config — every field, not just the two
// tiers, because forceCapabilityFieldsToBase assigns the table wholesale.
//
// The trusted half is asserted too, so a loader that ignored the project config
// unconditionally could not pass this by being uniformly restrictive.
func TestApplyProjectConfig_UntrustedGitStaysGlobal(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws := t.TempDir()
	// Every field differs from base, and in both directions: two grants, one
	// restriction, one trailer flip, and a shortened protected-branch list.
	writeProjectConfig(t, ws, "[git]\nallow_writes = false\nallow_destructive = true\n"+
		"allow_push = true\ncommit_trailer = true\nprotected_branches = []\n")

	base := config.Defaults()
	base.Git = config.GitConfig{
		AllowWrites:       true,
		AllowDestructive:  false,
		AllowPush:         false,
		ProtectedBranches: []string{"main", "release"},
		CommitTrailer:     false,
	}
	s := projectGitSessionWithBase(t, ws, base)

	wantBase := tools.GitPolicy{
		AllowWrites:       true,
		AllowDestructive:  false,
		AllowPush:         false,
		ProtectedBranches: []string{"main", "release"},
		CommitTrailer:     false,
	}
	if got := s.gitPolicy(); !reflect.DeepEqual(got, wantBase) {
		t.Fatalf("an untrusted project [git] block leaked into the resolved policy:\n got %+v\nwant %+v", got, wantBase)
	}
	if s.projectGitStatus().Trusted {
		t.Error("the captured status must not claim trust for a workspace nobody trusted")
	}

	// And the gate is a gate, not a blanket refusal: approved, the same request
	// lands whole on the next apply.
	grantExecTrust(t, ws)
	s.applyProjectConfig(ws)
	wantProject := tools.GitPolicy{
		AllowWrites:      false,
		AllowDestructive: true,
		AllowPush:        true,
		CommitTrailer:    true,
	}
	got := s.gitPolicy()
	got.ProtectedBranches = nil // [] and nil are the same empty list here.
	if !reflect.DeepEqual(got, wantProject) {
		t.Fatalf("a trusted project [git] block was not applied:\n got %+v\nwant %+v", got, wantProject)
	}
	if len(s.gitPolicy().ProtectedBranches) != 0 {
		t.Errorf("the trusted request emptied protected_branches, got %v", s.gitPolicy().ProtectedBranches)
	}
	if !s.projectGitStatus().Trusted {
		t.Error("the captured status must report trust once the grant is in force")
	}
}

// TestProjectGitStatus_UnparseableConfig covers the last silent case. A project
// config that will not parse is skipped WHOLE, so its [git] block is as ignored
// as an untrusted one — and there is no `plumb trust` that would help, which is
// exactly why the agent has to be told something different.
func TestProjectGitStatus_UnparseableConfig(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws := t.TempDir()
	writeProjectConfig(t, ws, "[git\nallow_push = true\n")

	st := projectGitSession(t, ws).projectGitStatus()
	if !st.Unreadable {
		t.Errorf("an unparseable project config must be reported, got %+v — "+
			"silence here is the same defect as the silence the notice exists to end", st)
	}
}

// TestProjectGitStatus_RePinCarriesTheGrant pins the state the unreadable notice
// describes, and is the tripwire that stops the notice outliving it.
//
// applyProjectConfig returns on a parse error WITHOUT reverting v.git, and the
// same early return runs on a re-pin. So a session pinned to a trusted workspace
// and re-pinned into one whose config will not parse arrives holding the first
// workspace's granted tiers: an elevation the second repository was never given,
// obtained by shipping a broken config rather than a bold one. That is PLAN-309
// (pre-existing on main, priority 1) and the fail-open is in applyProjectConfig,
// not in the notice.
//
// This test asserts the CURRENT behaviour so the notice may honestly name it.
// When PLAN-309 lands, this test fails — and the clause "re-pinned here from
// another workspace, THAT workspace's values" must be deleted from
// unreadableProjectConfigNotice in the same change, or the notice starts lying in
// the opposite direction. The assertion in internal/tools names this test for
// that reason.
func TestProjectGitStatus_RePinCarriesTheGrant(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	trusted := t.TempDir()
	writeProjectConfig(t, trusted, "[git]\nallow_push = true\n")
	grantExecTrust(t, trusted)

	s := projectGitSession(t, trusted)
	if !s.projectGitStatus().Trusted || !s.gitPolicy().AllowPush {
		t.Fatalf("precondition: the grant must be in force before the re-pin, got trusted=%v allow_push=%v",
			s.projectGitStatus().Trusted, s.gitPolicy().AllowPush)
	}

	// Re-pin into a workspace whose config cannot be parsed. Nobody trusted this
	// one, and it asks for nothing that could be honoured.
	broken := t.TempDir()
	writeProjectConfig(t, broken, "[git\nallow_push = true\n")
	s.mutate(func(v *sessionView) { v.acquiredRoot = broken })
	s.applyProjectConfig(broken)

	if !s.projectGitStatus().Unreadable {
		t.Fatalf("the re-pinned workspace's config must report as unparseable, got %+v", s.projectGitStatus())
	}
	if !s.gitPolicy().AllowPush {
		t.Fatal("PLAN-309 appears fixed: the re-pin no longer carries the previous workspace's grant. " +
			"Good — now delete the 're-pinned here from another workspace' clause from " +
			"unreadableProjectConfigNotice (internal/tools/session_start_git_notice.go) and its assertions in " +
			"TestFormatProjectGitNotice, which this test exists to keep honest.")
	}
}

// TestProjectGitStatus_NoProjectConfig keeps the common case quiet.
func TestProjectGitStatus_NoProjectConfig(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	st := projectGitSession(t, t.TempDir()).projectGitStatus()
	if len(st.Keys) != 0 || st.Trusted || st.Unreadable {
		t.Errorf("a workspace with no .plumb/config.toml must report nothing, got %+v", st)
	}
}
