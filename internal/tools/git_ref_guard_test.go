package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sessionGitTool builds a git tool with a session identity wired, the way
// conn_register does for a live connection.
func sessionGitTool(repo, id, name string) *Git {
	return NewGit(
		WriteDeps{WorkspaceFn: func(context.Context) string { return repo }},
		func() GitPolicy { return GitPolicy{AllowWrites: true, AllowDestructive: true} },
	).WithSession(func() string { return id }, func() string { return name })
}

// stageFile writes a file into the repo and stages it through the tool.
func stageFile(t *testing.T, tool *Git, repo, file, content string, confirm bool) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"subcommand": "add", "files": []string{file}}
	if confirm {
		args["confirm"] = true
	}
	if out, err := callGit(t, tool, args); err != nil {
		t.Fatalf("add %s: %v\n%s", file, err, out)
	}
}

func gitHeadSHA(t *testing.T, repo string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// runGitDirect mutates the repo OUTSIDE plumb's mediation — the user's shell,
// an IDE, CI.
func runGitDirect(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// TestGitRefGuard_PeerCommitRequiresConfirm is the acceptance scenario: two
// sessions interleave commits on one repo; the second session's commit is
// refused until confirmed, and the refusal/warning names the peer and the
// old→new refs.
func TestGitRefGuard_PeerCommitRequiresConfirm(t *testing.T) {
	requireGit(t)
	repo := initTestRepo(t)
	sessA := sessionGitTool(repo, "sess-a", "amber-fox")
	sessB := sessionGitTool(repo, "sess-b", "blue-heron")
	initial := gitHeadSHA(t, repo)

	// A baselines on the initial commit and stages its work.
	if _, err := callGit(t, sessA, map[string]any{"subcommand": "status"}); err != nil {
		t.Fatalf("A status: %v", err)
	}
	stageFile(t, sessA, repo, "a.txt", "from a\n", false)

	// B's first-contact commit has no baseline to compare against, so it runs
	// without friction — and makes B the attributable mover. It is path-limited
	// so it does not sweep A's staged file into B's commit.
	stageFile(t, sessB, repo, "b.txt", "from b\n", false)
	out, err := callGit(t, sessB, map[string]any{"subcommand": "commit", "message": "B commit", "files": []string{"b.txt"}})
	if err != nil {
		t.Fatalf("B commit: %v", err)
	}
	if strings.Contains(out, "plumb-warning") {
		t.Errorf("first-contact commit should not warn, got:\n%s", out)
	}
	moved := gitHeadSHA(t, repo)

	// A's commit is refused: HEAD moved under it, attributed to B.
	_, err = callGit(t, sessA, map[string]any{"subcommand": "commit", "message": "A commit", "files": []string{"a.txt"}})
	if err == nil {
		t.Fatal("expected refusal for a commit after a peer moved HEAD")
	}
	for _, want := range []string{"confirm: true", `moved by plumb session "blue-heron"`, initial[:7], moved[:7]} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q:\n%s", want, err)
		}
	}

	// With confirm the commit proceeds, and the response leads with the warning.
	out, err = callGit(t, sessA, map[string]any{"subcommand": "commit", "message": "A commit", "files": []string{"a.txt"}, "confirm": true})
	if err != nil {
		t.Fatalf("confirmed commit: %v", err)
	}
	if !strings.HasPrefix(out, "# plumb-warning: ") {
		t.Errorf("confirmed commit should lead with the plumb-warning, got:\n%s", out)
	}
	for _, want := range []string{`moved by plumb session "blue-heron"`, initial[:7], moved[:7]} {
		if !strings.Contains(out, want) {
			t.Errorf("warning missing %q:\n%s", want, out)
		}
	}
}

func TestGitRefGuard_ExpectedHead(t *testing.T) {
	requireGit(t)
	repo := initTestRepo(t)
	sessA := sessionGitTool(repo, "sess-a", "amber-fox")
	sessB := sessionGitTool(repo, "sess-b", "blue-heron")
	initial := gitHeadSHA(t, repo)

	if _, err := callGit(t, sessA, map[string]any{"subcommand": "status"}); err != nil {
		t.Fatalf("A status: %v", err)
	}
	stageFile(t, sessB, repo, "b.txt", "from b\n", false)
	if _, err := callGit(t, sessB, map[string]any{"subcommand": "commit", "message": "B commit"}); err != nil {
		t.Fatalf("B commit: %v", err)
	}
	current := gitHeadSHA(t, repo)

	// A re-baselines through the confirmed add so only expected_head is exercised.
	stageFile(t, sessA, repo, "a.txt", "from a\n", true)

	// A stale expected_head refuses even with confirm: true — regardless of sessions.
	_, err := callGit(t, sessA, map[string]any{
		"subcommand": "commit", "message": "A commit", "confirm": true, "expected_head": initial,
	})
	if err == nil || !strings.Contains(err.Error(), "expected_head mismatch") {
		t.Fatalf("expected expected_head mismatch refusal, got %v", err)
	}
	if !strings.Contains(err.Error(), current[:7]) {
		t.Errorf("mismatch error should name the current HEAD %s:\n%s", current[:7], err)
	}

	// An unresolvable expected_head refuses as well (fail closed).
	_, err = callGit(t, sessA, map[string]any{
		"subcommand": "commit", "message": "A commit", "expected_head": "deadbeefdeadbeef",
	})
	if err == nil || !strings.Contains(err.Error(), "does not resolve") {
		t.Fatalf("expected unresolvable expected_head refusal, got %v", err)
	}

	// The current HEAD — given in short form — passes, and A's baseline is
	// already current from the confirmed add, so no warning either.
	out, err := callGit(t, sessA, map[string]any{
		"subcommand": "commit", "message": "A commit", "expected_head": current[:8],
	})
	if err != nil {
		t.Fatalf("commit with matching expected_head: %v", err)
	}
	if strings.Contains(out, "plumb-warning") {
		t.Errorf("no warning expected when the baseline is current, got:\n%s", out)
	}
}

// TestGitRefGuard_ExpectedHeadWithoutSession: expected_head is enforced even
// for a tool with no session identity wired (the ledger stays out of it).
func TestGitRefGuard_ExpectedHeadWithoutSession(t *testing.T) {
	requireGit(t)
	repo := initTestRepo(t)
	tool := NewGit(
		WriteDeps{WorkspaceFn: func(context.Context) string { return repo }},
		func() GitPolicy { return GitPolicy{AllowWrites: true} },
	)
	stageFile(t, tool, repo, "one.txt", "one\n", false)
	if _, err := callGit(t, tool, map[string]any{"subcommand": "commit", "message": "one"}); err != nil {
		t.Fatalf("commit one: %v", err)
	}

	stageFile(t, tool, repo, "two.txt", "two\n", false)
	_, err := callGit(t, tool, map[string]any{"subcommand": "commit", "message": "two", "expected_head": "HEAD~1"})
	if err == nil || !strings.Contains(err.Error(), "expected_head mismatch") {
		t.Fatalf("expected expected_head mismatch, got %v", err)
	}
	if _, err := callGit(t, tool, map[string]any{"subcommand": "commit", "message": "two", "expected_head": "HEAD"}); err != nil {
		t.Fatalf("commit with expected_head=HEAD: %v", err)
	}
}

// TestGitRefGuard_SelfMoveNoFriction: a session's own commits and branch
// switches never trigger the guard — single-session use sees zero new friction.
func TestGitRefGuard_SelfMoveNoFriction(t *testing.T) {
	requireGit(t)
	repo := initTestRepo(t)
	sessA := sessionGitTool(repo, "sess-a", "amber-fox")
	if _, err := callGit(t, sessA, map[string]any{"subcommand": "status"}); err != nil {
		t.Fatalf("status: %v", err)
	}

	stageFile(t, sessA, repo, "f1.txt", "one\n", false)
	if out, err := callGit(t, sessA, map[string]any{"subcommand": "commit", "message": "one"}); err != nil {
		t.Fatalf("commit one: %v", err)
	} else if strings.Contains(out, "plumb-warning") {
		t.Errorf("own commit warned:\n%s", out)
	}
	if out, err := callGit(t, sessA, map[string]any{"subcommand": "switch", "args": []string{"--create", "topic"}}); err != nil {
		t.Fatalf("switch: %v", err)
	} else if strings.Contains(out, "plumb-warning") {
		t.Errorf("own switch warned:\n%s", out)
	}
	stageFile(t, sessA, repo, "f2.txt", "two\n", false)
	if out, err := callGit(t, sessA, map[string]any{"subcommand": "commit", "message": "two"}); err != nil {
		t.Fatalf("commit two: %v", err)
	} else if strings.Contains(out, "plumb-warning") {
		t.Errorf("own commit after own switch warned:\n%s", out)
	}
}

// TestGitRefGuard_ReadsNeverWarnAndRebaseline: read-tier ops never warn, and a
// read that observes the peer's new HEAD re-baselines the session, so the
// following write needs no confirm.
func TestGitRefGuard_ReadsNeverWarnAndRebaseline(t *testing.T) {
	requireGit(t)
	repo := initTestRepo(t)
	sessA := sessionGitTool(repo, "sess-a", "amber-fox")
	sessB := sessionGitTool(repo, "sess-b", "blue-heron")

	if _, err := callGit(t, sessA, map[string]any{"subcommand": "status"}); err != nil {
		t.Fatalf("A status: %v", err)
	}
	stageFile(t, sessB, repo, "b.txt", "from b\n", false)
	if _, err := callGit(t, sessB, map[string]any{"subcommand": "commit", "message": "B commit"}); err != nil {
		t.Fatalf("B commit: %v", err)
	}

	for _, sub := range []map[string]any{
		{"subcommand": "status"},
		{"subcommand": "log", "args": []string{"--oneline", "-3"}},
		{"subcommand": "diff", "args": []string{"HEAD"}},
	} {
		out, err := callGit(t, sessA, sub)
		if err != nil {
			t.Fatalf("A %v: %v", sub["subcommand"], err)
		}
		if strings.Contains(out, "plumb-warning") {
			t.Errorf("read-tier %v must never warn, got:\n%s", sub["subcommand"], out)
		}
	}

	// The reads re-baselined A: staging and committing need no confirm now.
	stageFile(t, sessA, repo, "a.txt", "from a\n", false)
	out, err := callGit(t, sessA, map[string]any{"subcommand": "commit", "message": "A commit"})
	if err != nil {
		t.Fatalf("A commit after re-baselining reads: %v", err)
	}
	if strings.Contains(out, "plumb-warning") {
		t.Errorf("commit after re-baseline should not warn, got:\n%s", out)
	}
}

// TestGitRefGuard_ExternalMoveNoFriction: a HEAD move plumb cannot attribute
// (no plumb session's operation produced this state) adds no friction.
func TestGitRefGuard_ExternalMoveNoFriction(t *testing.T) {
	requireGit(t)
	repo := initTestRepo(t)
	sessA := sessionGitTool(repo, "sess-a", "amber-fox")

	if _, err := callGit(t, sessA, map[string]any{"subcommand": "status"}); err != nil {
		t.Fatalf("A status: %v", err)
	}
	// A non-plumb actor moves HEAD.
	if err := os.WriteFile(filepath.Join(repo, "ext.txt"), []byte("external\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDirect(t, repo, "add", "ext.txt")
	runGitDirect(t, repo, "commit", "-m", "external commit")

	stageFile(t, sessA, repo, "a.txt", "from a\n", false)
	out, err := callGit(t, sessA, map[string]any{"subcommand": "commit", "message": "A commit"})
	if err != nil {
		t.Fatalf("A commit after external move: %v", err)
	}
	if strings.Contains(out, "plumb-warning") {
		t.Errorf("external (unattributable) move must not warn, got:\n%s", out)
	}
}

// TestGitRefGuard_PeerBranchSwitchDetected: the guard compares branch as well
// as HEAD — a peer's branch switch at the SAME commit still trips it.
func TestGitRefGuard_PeerBranchSwitchDetected(t *testing.T) {
	requireGit(t)
	repo := initTestRepo(t)
	sessA := sessionGitTool(repo, "sess-a", "amber-fox")
	sessB := sessionGitTool(repo, "sess-b", "blue-heron")

	if _, err := callGit(t, sessA, map[string]any{"subcommand": "status"}); err != nil {
		t.Fatalf("A status: %v", err)
	}
	stageFile(t, sessA, repo, "a.txt", "from a\n", false)
	if _, err := callGit(t, sessB, map[string]any{"subcommand": "switch", "args": []string{"--create", "topic"}}); err != nil {
		t.Fatalf("B switch: %v", err)
	}

	_, err := callGit(t, sessA, map[string]any{"subcommand": "commit", "message": "A commit"})
	if err == nil {
		t.Fatal("expected refusal after a peer switched the branch")
	}
	if !strings.Contains(err.Error(), `moved by plumb session "blue-heron"`) {
		t.Errorf("refusal should name the peer:\n%s", err)
	}
	if !strings.Contains(err.Error(), "now topic@") {
		t.Errorf("refusal should show the branch change, got:\n%s", err)
	}
}

// TestGitRefState_PeerMoveAttribution pins the ledger's attribution rules
// without a git binary.
func TestGitRefState_PeerMoveAttribution(t *testing.T) {
	st := &gitRefState{observed: map[string]gitRefObservation{}}
	base := gitRefObservation{head: "aaa", branch: "main"}
	next := gitRefObservation{head: "bbb", branch: "main"}
	past := gitRefObservation{head: "ccc", branch: "main"}

	// No baseline yet → no movement.
	if _, _, moved := st.peerMove("a", next); moved {
		t.Error("first contact reported movement")
	}
	st.record("a", "amber-fox", base, false)
	// Unchanged state → no movement.
	if _, _, moved := st.peerMove("a", base); moved {
		t.Error("unchanged state reported movement")
	}
	// Moved but no mover recorded → unknown → no friction.
	if _, _, moved := st.peerMove("a", next); moved {
		t.Error("unattributable move reported as peer move")
	}
	// B's own operation left the repo at next → B is the mover.
	st.record("b", "blue-heron", next, true)
	prev, mover, moved := st.peerMove("a", next)
	if !moved {
		t.Fatal("attributed peer move not detected")
	}
	if mover.name != "blue-heron" || prev != base {
		t.Errorf("peerMove = prev %v, mover %q; want prev %v, mover blue-heron", prev, mover.name, base)
	}
	// The mover itself never gets friction.
	if _, _, moved := st.peerMove("b", next); moved {
		t.Error("mover's own state reported as peer move")
	}
	// State has moved past the mover's record → unknown again.
	if _, _, moved := st.peerMove("a", past); moved {
		t.Error("state past the mover's record reported as peer move")
	}
	// A write-tier op that did not change the state is not the mover.
	st.record("c", "calm-crow", next, false)
	_, mover, moved = st.peerMove("a", next)
	if !moved || mover.name != "blue-heron" {
		t.Errorf("non-mutating op stole attribution: moved=%v mover=%q", moved, mover.name)
	}
}

func TestSweepGitRefStates(t *testing.T) {
	idle := gitRefStateFor("sweep-test-idle")
	idle.lastUsedNs.Store(time.Now().Add(-2 * repoLockIdleExpiry).UnixNano())
	live := gitRefStateFor("sweep-test-live")
	sweepGitRefStates(time.Now())
	if _, ok := gitRefStates.Load("sweep-test-idle"); ok {
		t.Error("idle ledger entry not evicted")
	}
	if _, ok := gitRefStates.Load("sweep-test-live"); !ok {
		t.Error("live ledger entry evicted")
	}
	_ = live
}

// TestArmRefGuard_ArmsNetworkTier pins PLAN-278 item 1 at the unit level: the
// ref guard's check flag must arm on the network tier as well as write and
// destructive, so expected_head is enforced on push/fetch/pull. Only read stays
// unarmed.
func TestArmRefGuard_ArmsNetworkTier(t *testing.T) {
	tool := NewGit(WriteDeps{}, func() GitPolicy { return GitPolicy{} }).
		WithSession(func() string { return "sess-a" }, func() string { return "amber-fox" })
	for _, tc := range []struct {
		name string
		tier gitTier
		want bool
	}{
		{"read", tierRead, false},
		{"write", tierWrite, true},
		{"destructive", tierDestructive, true},
		{"network", tierNetwork, true},
	} {
		g := tool.armRefGuard(gitToolArgs{ExpectedHead: "HEAD"}, tc.tier)
		if g == nil {
			t.Fatalf("%s: expected a guard when expected_head is set", tc.name)
		}
		if g.check != tc.want {
			t.Errorf("%s: guard.check = %v, want %v", tc.name, g.check, tc.want)
		}
	}
}

// TestGitRefGuard_ExpectedHeadNetworkTier is the PLAN-278 item 1 end-to-end
// pin: a stale expected_head must refuse a network-tier fetch BEFORE git runs.
// Pre-fix the network tier never armed the guard, so expected_head was ignored
// and the fetch reached git — the exact gap a force-push (the highest
// blast-radius op the tool mediates) leaves open.
func TestGitRefGuard_ExpectedHeadNetworkTier(t *testing.T) {
	requireGit(t)
	repo := initTestRepo(t)
	initial := gitHeadSHA(t, repo)
	// Move HEAD forward outside plumb so initial is stale relative to HEAD.
	runGitDirect(t, repo, "commit", "--allow-empty", "-m", "second")
	// A bare remote so a matching-pin fetch has somewhere to fetch from.
	bare := filepath.Join(t.TempDir(), "remote.git")
	runGitDirect(t, repo, "init", "--bare", bare)
	runGitDirect(t, repo, "remote", "add", "origin", bare)
	runGitDirect(t, repo, "push", "origin", "HEAD")

	net := NewGit(
		WriteDeps{WorkspaceFn: func(context.Context) string { return repo }},
		func() GitPolicy { return GitPolicy{AllowWrites: true, AllowPush: true} },
	).WithSession(func() string { return "sess-a" }, func() string { return "amber-fox" })

	// A stale expected_head refuses the fetch BEFORE git runs.
	_, err := callGit(t, net, map[string]any{
		"subcommand": "fetch", "confirm": true, "expected_head": initial,
	})
	if err == nil || !strings.Contains(err.Error(), "expected_head mismatch") {
		t.Fatalf("expected the network-tier expected_head guard to refuse a stale pin, got %v", err)
	}

	// A matching pin passes the guard and the fetch runs to completion.
	if _, err := callGit(t, net, map[string]any{
		"subcommand": "fetch", "confirm": true, "expected_head": "HEAD",
	}); err != nil {
		t.Fatalf("fetch with a matching expected_head should succeed, got %v", err)
	}
}
