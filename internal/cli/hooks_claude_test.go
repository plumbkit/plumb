package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The Stop hook is driven end to end here — probe injected, wake dir and
// timings redirected through the same env vars a user tunes with — because its
// interesting behaviour is the interaction between the workspace guard, the
// stamps, the chain record and the watcher, not any one of them alone.

// wakeSandbox points the hook's state at a temp dir and shortens the watch so a
// test that must reach the deadline takes a second, not five minutes.
func wakeSandbox(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PLUMB_WAKE_DIR", dir)
	t.Setenv("PLUMB_WAKE_WINDOW", "1")
	t.Setenv("PLUMB_WAKE_INTERVAL", "1")
	// Pin the peer ceiling too. Without it these tests read whatever
	// [collab] wake_peer_window_seconds the developer's own global config
	// happens to carry, and a watch that should end in a second would run for
	// the configured hour. 0 is also the pre-extension behaviour, which is what
	// every test written before the ceiling existed still means to assert.
	t.Setenv("PLUMB_WAKE_PEER_WINDOW", "0")
	return dir
}

// probeStep is one scripted answer from a fake wakeProbe.
type probeStep struct {
	report mailReport
	peers  int
	ok     bool
}

// scriptedProbe answers from steps in order and repeats the last one forever,
// returning a pointer to the call count.
//
// The watcher's behaviour is asserted through that COUNT rather than through
// elapsed time: how many polls it took before standing down is exactly what
// each of these rules decides, and it stays sharp on a loaded CI runner where
// wall-clock assertions would not.
func scriptedProbe(steps ...probeStep) (wakeProbe, *int) {
	calls := 0
	return func(_, _ string) (mailReport, int, bool) {
		step := steps[min(calls, len(steps)-1)]
		calls++
		return step.report, step.peers, step.ok
	}, &calls
}

// plumbWorkspace builds a directory that passes the .plumb marker test.
func plumbWorkspace(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	if err := os.Mkdir(filepath.Join(ws, ".plumb"), 0o755); err != nil {
		t.Fatal(err)
	}
	return ws
}

// nonWorkspaceDir names a directory with no .plumb marker at or above it.
//
// It cannot be a t.TempDir(): the marker test walks to the filesystem root, and
// GOTMPDIR routinely puts test temp dirs INSIDE a checkout — this repo's own CI
// does, and a plumb-ops style layout does locally — so a temp dir can sit under
// a real workspace marker and the negative case would pass or fail by
// environment. A path hung directly off the root has exactly two ancestors to
// check, neither of which is a checkout. It need not exist: the walk only
// stats for the marker.
func nonWorkspaceDir(t *testing.T) string {
	t.Helper()
	return string(filepath.Separator) + "plumb-hooks-test-not-a-workspace"
}

func TestClaudeStopHook_WakesOnPendingMail(t *testing.T) {
	dir := wakeSandbox(t)
	ws := plumbWorkspace(t)

	wake := claudeStopHook(
		claudeHookInput{Event: "Stop", SessionID: "conv-1", CWD: ws},
		func(_, _ string) (mailReport, int, bool) {
			return mailReport{Session: "grey-lynx", Count: 2, AgesSeconds: []int{31, 4}}, 0, true
		})
	if wake == nil {
		t.Fatal("pending mail did not produce a wake")
	}
	if got := wakeSentence(*wake); !strings.Contains(got, "2 unread") ||
		!strings.Contains(got, "31s") || !strings.Contains(got, "check_messages") {
		t.Errorf("wake sentence = %q", got)
	}

	// The wake is recorded so the woken turn can prove consumption, and the
	// watcher's lock is released — a leaked lock would make the next turn of
	// this session silently unwatchable.
	pending, chain, found := readRearm(filepath.Join(dir, "grey-lynx.rearm"))
	if !found || pending != 2 || chain != 1 {
		t.Errorf("rearm = (%d, %d, %v), want (2, 1, true)", pending, chain, found)
	}
	if _, err := os.Stat(filepath.Join(dir, "grey-lynx.lock")); !os.IsNotExist(err) {
		t.Error("watcher lock outlived the wake")
	}
}

// TestClaudeStopHook_StampFormat pins the stamp's key set: peer-reachability
// tooling parses these files, so the format is an interface, not a detail.
func TestClaudeStopHook_StampFormat(t *testing.T) {
	dir := wakeSandbox(t)
	ws := plumbWorkspace(t)

	claudeStopHook(
		claudeHookInput{Event: "Stop", SessionID: "conv-1", CWD: ws, StopHookActive: true},
		func(_, _ string) (mailReport, int, bool) {
			return mailReport{Session: "grey-lynx"}, 0, true
		})

	data, err := os.ReadFile(filepath.Join(dir, "grey-lynx.stamp"))
	if err != nil {
		t.Fatalf("no stamp written: %v", err)
	}
	body := string(data)
	for _, key := range []string{"epoch=", "plumb_session=grey-lynx", "conversation_id=conv-1", "cwd=" + ws, "hook=plumb-mail-wake"} {
		if !strings.Contains(body, key) {
			t.Errorf("stamp missing %q:\n%s", key, body)
		}
	}
}

// TestClaudeStopHook_UnlinkedSessionStampsByConversation covers the session
// that never linked: it is still hooked, and a stamp keyed by conversation id
// is how a peer learns that.
func TestClaudeStopHook_UnlinkedSessionStampsByConversation(t *testing.T) {
	dir := wakeSandbox(t)
	ws := plumbWorkspace(t)

	claudeStopHook(
		claudeHookInput{Event: "Stop", SessionID: "conv-9", CWD: ws},
		func(_, _ string) (mailReport, int, bool) { return mailReport{}, 0, false })

	if _, err := os.Stat(filepath.Join(dir, "conv-9.stamp")); err != nil {
		t.Errorf("no conversation-keyed stamp for an unresolved session: %v", err)
	}
}

// TestClaudeStopHook_OutsidePlumbWorkspace is PLAN-338's guarantee: the hook is
// installed user-wide, so it must change nothing at all for a session in an
// unrelated repository.
func TestClaudeStopHook_OutsidePlumbWorkspace(t *testing.T) {
	dir := wakeSandbox(t)
	probed := false

	wake := claudeStopHook(
		claudeHookInput{Event: "Stop", SessionID: "conv-1", CWD: nonWorkspaceDir(t)},
		func(_, _ string) (mailReport, int, bool) {
			probed = true
			return mailReport{Session: "grey-lynx", Count: 5}, 0, true
		})

	if wake != nil {
		t.Error("woke a session outside any plumb workspace")
	}
	if probed {
		t.Error("probed the mailbox for a session outside any plumb workspace")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("wrote %d file(s) for a session outside any plumb workspace", len(entries))
	}
}

// TestClaudeStopHook_FailsOpen: a probe that cannot answer must never hold a
// turn open, however long the watch window is.
func TestClaudeStopHook_FailsOpen(t *testing.T) {
	wakeSandbox(t)
	ws := plumbWorkspace(t)

	if wake := claudeStopHook(
		claudeHookInput{Event: "Stop", SessionID: "conv-1", CWD: ws},
		func(_, _ string) (mailReport, int, bool) { return mailReport{}, 0, false }); wake != nil {
		t.Error("a failing probe produced a wake")
	}
	if wake := claudeStopHook(claudeHookInput{Event: "Stop", SessionID: "conv-1", CWD: ws}, nil); wake != nil {
		t.Error("a nil probe produced a wake")
	}
}

// TestWakeChainContinues covers the re-arm decision on a woken turn: only a
// proven drop in the pending count, under the chain cap, earns another watcher.
func TestWakeChainContinues(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PLUMB_WAKE_DIR", dir)
	rearm := filepath.Join(dir, "s.rearm")

	write := func(body string) {
		if err := os.WriteFile(rearm, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		name   string
		record string
		report mailReport
		ok     bool
		want   bool
	}{
		{"consumed", "pending=2\nchain=1\n", mailReport{Count: 1}, true, true},
		{"nothing consumed", "pending=2\nchain=1\n", mailReport{Count: 2}, true, false},
		{"count grew", "pending=2\nchain=1\n", mailReport{Count: 3}, true, false},
		{"probe failed", "pending=2\nchain=1\n", mailReport{}, false, false},
		{"chain spent", "pending=2\nchain=10\n", mailReport{Count: 0}, true, false},
		{"unreadable counter reads as spent", "pending=2\nchain=oops\n", mailReport{Count: 0}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			write(tc.record)
			if got := wakeChainContinues(rearm, tc.report, tc.ok); got != tc.want {
				t.Errorf("wakeChainContinues = %v, want %v", got, tc.want)
			}
		})
	}

	if err := os.Remove(rearm); err != nil {
		t.Fatal(err)
	}
	if wakeChainContinues(rearm, mailReport{Count: 0}, true) {
		t.Error("a woken turn with no record re-armed — an unconsumed wake must not chain")
	}
}

// TestClaudeStopHook_WokenTurnStandsDown pins the recursion guard itself: the
// woken turn that consumed nothing clears its record and arms no watcher.
func TestClaudeStopHook_WokenTurnStandsDown(t *testing.T) {
	dir := wakeSandbox(t)
	ws := plumbWorkspace(t)
	rearm := filepath.Join(dir, "grey-lynx.rearm")
	if err := os.WriteFile(rearm, []byte("pending=1\nchain=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	wake := claudeStopHook(
		claudeHookInput{Event: "Stop", SessionID: "conv-1", CWD: ws, StopHookActive: true},
		func(_, _ string) (mailReport, int, bool) {
			return mailReport{Session: "grey-lynx", Count: 1}, 0, true // unchanged: not consumed
		})
	if wake != nil {
		t.Fatal("an unconsumed wake chained into another")
	}
	if _, err := os.Stat(rearm); !os.IsNotExist(err) {
		t.Error("stand-down left its re-arm record behind")
	}
}

// TestClaudeStopHook_WokenTurnRearmsAfterConsumption is the other half: a
// back-and-forth keeps its cadence, and the chain counter advances so it cannot
// run forever.
func TestClaudeStopHook_WokenTurnRearmsAfterConsumption(t *testing.T) {
	dir := wakeSandbox(t)
	ws := plumbWorkspace(t)
	rearm := filepath.Join(dir, "grey-lynx.rearm")
	if err := os.WriteFile(rearm, []byte("pending=3\nchain=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	wake := claudeStopHook(
		claudeHookInput{Event: "Stop", SessionID: "conv-1", CWD: ws, StopHookActive: true},
		func(_, _ string) (mailReport, int, bool) {
			return mailReport{Session: "grey-lynx", Count: 1, AgesSeconds: []int{9}}, 0, true
		})
	if wake == nil {
		t.Fatal("a consumed wake did not re-arm")
	}
	pending, chain, found := readRearm(rearm)
	if !found || pending != 1 || chain != 2 {
		t.Errorf("rearm = (%d, %d, %v), want (1, 2, true)", pending, chain, found)
	}
}

// TestAcquireWakeLock covers the single-instance guard, including the reused
// session name whose previous tenant still holds a live lock.
func TestAcquireWakeLock(t *testing.T) {
	dir := t.TempDir()

	// The holder is a plumb process in production; a test binary never is, so
	// the identity check is supplied here rather than inferred from ps.
	alwaysPlumb := func(int) bool { return true }
	first, ok := acquireWakeLockWith(dir, "grey-lynx", "conv-1", alwaysPlumb)
	if !ok {
		t.Fatal("first watcher could not take the lock")
	}
	if _, ok := acquireWakeLockWith(dir, "grey-lynx", "conv-1", alwaysPlumb); ok {
		t.Error("the same conversation stacked a second watcher")
	}
	// A different conversation answering to the same session name IS that
	// session now, so it takes the lock over.
	second, ok := acquireWakeLockWith(dir, "grey-lynx", "conv-2", alwaysPlumb)
	if !ok {
		t.Fatal("a new tenant of a reused session name was locked out")
	}
	// The ownership check, probed in the order that actually exercises it: the
	// previous tenant releasing AFTER the takeover must not remove the lock the
	// new watcher now holds. Released in the other order this assertion passes
	// vacuously, which is how it was first written.
	first.release()
	if _, err := os.Stat(filepath.Join(dir, "grey-lynx.lock")); err != nil {
		t.Error("the evicted tenant's release deleted the lock its successor holds")
	}
	second.release()

	third, ok := acquireWakeLockWith(dir, "grey-lynx", "conv-3", alwaysPlumb)
	if !ok {
		t.Fatal("lock was not reclaimable after release")
	}
	third.release()
	third.release()
}

// TestClaudeStopHook_Seams closes the gap an independent review found: every
// piece of this change was tested in isolation, and the WIRING between them was
// not. Four separate mutations at the call sites in claudeStopHook and
// hookWakeProbe left the whole suite green, because every other test either
// calls the pieces directly or injects a fake in place of them.
//
// Each subtest therefore drives the real claudeStopHook and asserts an effect
// that is only reachable through the seam under test.
func TestClaudeStopHook_Seams(t *testing.T) {
	// A workspace whose PROJECT config narrows the window to something far
	// shorter than the global one, so "which config did the Stop path use?" is
	// answerable from the number of polls alone.
	narrowWorkspace := func(t *testing.T) string {
		t.Helper()
		cfgHome := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", cfgHome)
		t.Setenv("PLUMB_WAKE_WINDOW", "") // env would mask the difference
		t.Setenv("PLUMB_WAKE_PEER_WINDOW", "")
		t.Setenv("PLUMB_WAKE_INTERVAL", "1")
		if err := os.MkdirAll(filepath.Join(cfgHome, "plumb"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfgHome, "plumb", "config.toml"),
			[]byte("[collab]\nwake_window_seconds = 60\nwake_peer_window_seconds = 0\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		ws := plumbWorkspace(t)
		if err := os.WriteFile(filepath.Join(ws, ".plumb", "config.toml"),
			[]byte("[collab]\nwake_window_seconds = 1\nwake_peer_window_seconds = 0\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return ws
	}

	t.Run("the watch window comes from the WORKSPACE, not the machine", func(t *testing.T) {
		t.Setenv("PLUMB_WAKE_DIR", t.TempDir())
		ws := narrowWorkspace(t)
		probe, calls := scriptedProbe(probeStep{report: mailReport{Session: "grey-lynx"}, ok: true})

		claudeStopHook(claudeHookInput{Event: "Stop", SessionID: "conv-1", CWD: ws}, probe)

		// One probe to decide the key, then a 1s project window at a 1s interval
		// = one more. The 60s global window would be ~61. This is the only
		// assertion that the root plumbWorkspaceRoot finds is fed to
		// runtimeWakeWindows.
		if *calls != 2 {
			t.Errorf("polled %d time(s), want 2 — the Stop path resolved its window from the "+
				"machine config instead of this workspace's", *calls)
		}
	})

	t.Run("the peer count reaches the watcher", func(t *testing.T) {
		t.Setenv("PLUMB_WAKE_DIR", t.TempDir())
		t.Setenv("PLUMB_WAKE_WINDOW", "1")
		t.Setenv("PLUMB_WAKE_PEER_WINDOW", "3")
		t.Setenv("PLUMB_WAKE_INTERVAL", "1")
		ws := plumbWorkspace(t)
		probe, calls := scriptedProbe(probeStep{report: mailReport{Session: "grey-lynx"}, peers: 1, ok: true})

		claudeStopHook(claudeHookInput{Event: "Stop", SessionID: "conv-1", CWD: ws}, probe)

		// One probe to decide the key, then three more extending to the 3s
		// ceiling. A peer count dropped between the probe and the watcher would
		// stop at the 1s base, for two.
		if *calls != 4 {
			t.Errorf("polled %d time(s), want 4 — the probe's peer count never reached the watcher", *calls)
		}
	})

	t.Run("a turn end sweeps the wake directory", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("PLUMB_WAKE_DIR", dir)
		t.Setenv("PLUMB_WAKE_WINDOW", "1")
		t.Setenv("PLUMB_WAKE_PEER_WINDOW", "0")
		t.Setenv("PLUMB_WAKE_INTERVAL", "1")
		ws := plumbWorkspace(t)

		ancient := filepath.Join(dir, "long-gone.stamp")
		if err := os.WriteFile(ancient, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-claudeWakeStampTTL - time.Hour)
		if err := os.Chtimes(ancient, when, when); err != nil {
			t.Fatal(err)
		}

		probe, _ := scriptedProbe(probeStep{report: mailReport{Session: "grey-lynx"}, ok: true})
		claudeStopHook(claudeHookInput{Event: "Stop", SessionID: "conv-1", CWD: ws}, probe)

		if _, err := os.Stat(ancient); !os.IsNotExist(err) {
			t.Error("a turn end left week-old debris behind — the Stop path never sweeps")
		}
		if _, err := os.Stat(filepath.Join(dir, "grey-lynx.stamp")); err != nil {
			t.Errorf("the sweep ate this turn's own stamp: %v", err)
		}
	})
}

// TestSweepWakeDir: nothing ever cleaned the wake directory, so it grew a stamp
// per conversation forever and a lock per watcher that died before releasing
// one. The sweep must remove that debris WITHOUT touching anything a live
// session still depends on.
func TestSweepWakeDir(t *testing.T) {
	dir := t.TempDir()
	// The shipped windows, because a lock's bound is the ceiling and these cases
	// are about which side of it an age falls on.
	t.Setenv("PLUMB_WAKE_WINDOW", "300")
	t.Setenv("PLUMB_WAKE_PEER_WINDOW", "3600")

	write := func(name string, age time.Duration) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		stamp := time.Now().Add(-age)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		return path
	}

	lock := func(name string, age time.Duration) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
		// A live watcher's lock records a pid. Whether the sweep looks at it is
		// the point: it must not, because the only way to ask is isPlumbProcess,
		// which fails open and would delete this directory whenever `ps` hiccups.
		if err := os.WriteFile(filepath.Join(path, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-age)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
		return path
	}

	stale := write("gone-otter.stamp", claudeWakeStampTTL+time.Hour)
	staleRearm := write("gone-otter.rearm", claudeWakeStampTTL+time.Hour)
	fresh := write("live-otter.stamp", time.Minute)

	// Older than any watcher the installed timeout would have let run: debris.
	abandoned := lock("gone-otter.lock", 2*time.Hour)
	// Inside the hour-long ceiling. A watcher armed under it may well still be
	// running, and a sweep from ANOTHER session's turn end must not touch it —
	// deleting it lets this session arm a second watcher.
	working := lock("busy-otter.lock", 10*time.Minute)

	sweepWakeDir(dir)

	for _, gone := range []string{stale, staleRearm, abandoned} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s survived the sweep", filepath.Base(gone))
		}
	}
	for _, kept := range []string{fresh, working} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s was swept but may still be in use: %v", filepath.Base(kept), err)
		}
	}
}

// TestSweepWakeDir_LockLifetimeFollowsTheCeiling: the bound on a lock is the
// longest a watcher could run, so it has to track the CEILING. Bounding by the
// base window would delete the lock of every peer-extended watcher on the
// machine, which is the two-watchers-for-one-session defect this file has
// already had three times.
func TestSweepWakeDir_LockLifetimeFollowsTheCeiling(t *testing.T) {
	t.Setenv("PLUMB_WAKE_WINDOW", "60")
	t.Setenv("PLUMB_WAKE_PEER_WINDOW", "3600")

	// Comfortably past the 60s base, comfortably inside the 3600s ceiling.
	dir := t.TempDir()
	path := filepath.Join(dir, "busy-otter.lock")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-30 * time.Minute)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}

	sweepWakeDir(dir)

	if _, err := os.Stat(path); err != nil {
		t.Errorf("a 30m-old lock was swept under an hour-long ceiling: %v — its watcher "+
			"may still be running, and the session would arm a second one", err)
	}
}

func TestPlumbWorkspaceRoot(t *testing.T) {
	ws := plumbWorkspace(t)
	nested := filepath.Join(ws, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	// The ROOT, not merely "inside": it is where this workspace's project config
	// is read from, so returning the nested directory would silently resolve the
	// wake windows from the wrong file (or from none at all).
	got, ok := plumbWorkspaceRoot(nested)
	if !ok {
		t.Error("a directory below the marker did not resolve as inside the workspace")
	}
	if got != ws {
		t.Errorf("plumbWorkspaceRoot(nested) = %q, want the marker root %q", got, ws)
	}
	if _, ok := plumbWorkspaceRoot(nonWorkspaceDir(t)); ok {
		t.Error("an unrelated directory resolved as inside a plumb workspace")
	}
	if _, ok := plumbWorkspaceRoot(""); ok {
		t.Error("an empty cwd resolved as inside a plumb workspace")
	}
}

func TestSessionLinkageSentence(t *testing.T) {
	got := sessionLinkageSentence("conv-1", "conversation")
	for _, want := range []string{`"conv-1"`, "session_id", "session_start("} {
		if !strings.Contains(got, want) {
			t.Errorf("linkage sentence = %q, want it to contain %q", got, want)
		}
	}
	// It states a fact rather than issuing an instruction: context framed as a
	// command can trip a client's prompt-injection defences and be shown to the
	// user instead of acted on.
	if strings.HasPrefix(got, "Pass ") || strings.HasPrefix(got, "Call ") {
		t.Errorf("linkage sentence opens as an instruction: %q", got)
	}
}

// TestWakeStampKey_RefusesPathEscape: the conversation id comes from the client
// and the lock path derived from it is removed with RemoveAll, so a key that
// could leave the wake dir is refused outright rather than sanitised.
func TestWakeStampKey_RefusesPathEscape(t *testing.T) {
	for _, bad := range []string{
		"../../../../tmp/escape", "a/b", `a\b`, "..", ".", "x..y",
	} {
		if got := wakeStampKey(mailReport{}, bad); got != "" {
			t.Errorf("wakeStampKey(%q) = %q, want refusal", bad, got)
		}
	}
	if got := wakeStampKey(mailReport{Session: "grey-lynx"}, "conv-1"); got != "grey-lynx" {
		t.Errorf("wakeStampKey resolved session = %q, want grey-lynx", got)
	}
}

// TestClaudeStopHook_RefusedKeyWritesNothing: a refused key must stand the whole
// hook down before it writes a stamp or arms a watcher.
func TestClaudeStopHook_RefusedKeyWritesNothing(t *testing.T) {
	dir := wakeSandbox(t)
	ws := plumbWorkspace(t)

	wake := claudeStopHook(
		claudeHookInput{Event: "Stop", SessionID: "../../../../tmp/plumb-escape", CWD: ws},
		func(_, _ string) (mailReport, int, bool) { return mailReport{Count: 3}, 0, true })

	if wake != nil {
		t.Error("a session whose key was refused still produced a wake")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("wrote %d file(s) for a refused key", len(entries))
	}
}

// TestAcquireWakeLock_UnstampedLockIsReclaimedByAge: standing down on a lock
// with no readable pid stops two watchers racing — but a watcher that died in
// that window would otherwise leave a lock nothing can ever claim, and a
// session that can never arm a watcher is silently unwakeable.
//
// The bound is claudeUnstampedLockGrace and deliberately NOT the watch window:
// the gap being covered is the microseconds between os.Mkdir and writing the
// pid, so scaling it with an hour-long ceiling would leave a session that lost
// that race unwakeable for the hour.
func TestAcquireWakeLock_UnstampedLockIsReclaimedByAge(t *testing.T) {
	dir := t.TempDir()
	// Deliberately a LONG ceiling: the grace must not follow it.
	t.Setenv("PLUMB_WAKE_WINDOW", "300")
	t.Setenv("PLUMB_WAKE_PEER_WINDOW", "3600")
	lock := filepath.Join(dir, "grey-lynx.lock")
	if err := os.Mkdir(lock, 0o755); err != nil {
		t.Fatal(err)
	}

	// Fresh and unstamped: another watcher may be mid-acquire, so stand down.
	if _, ok := acquireWakeLock(dir, "grey-lynx", "conv-1"); ok {
		t.Error("stole a lock another watcher may have just taken")
	}

	// Past the grace, but far INSIDE the hour-long ceiling: still debris. Tying
	// this bound to the ceiling is what this case exists to catch.
	old := time.Now().Add(-2 * claudeUnstampedLockGrace)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	got, ok := acquireWakeLock(dir, "grey-lynx", "conv-1")
	if !ok {
		t.Fatal("an abandoned unstamped lock was never reclaimable — the session is unwakeable forever")
	}
	got.release()
}

// TestWakeLock_ReleaseFailsClosed: an unreadable conv file is the successor's
// window between mkdir and its own stamp. Deleting then would remove the lock
// it has just taken — the very bug the ownership check exists to prevent.
func TestWakeLock_ReleaseFailsClosed(t *testing.T) {
	dir := t.TempDir()
	lock, ok := acquireWakeLock(dir, "grey-lynx", "conv-1")
	if !ok {
		t.Fatal("could not take the lock")
	}
	if err := os.Remove(filepath.Join(lock.dir, "conv")); err != nil {
		t.Fatal(err)
	}
	lock.release()
	if _, err := os.Stat(lock.dir); err != nil {
		t.Error("release deleted a lock whose ownership it could not prove")
	}
}

// TestIsPlumbProcess: the guard that stops plumb SIGTERMing a stranger's
// process, and stops a reused pid making a session permanently unwakeable. It
// had no test at all — terminate() short-circuits on our own pid, so nothing
// ever reached it.
func TestIsPlumbProcess(t *testing.T) {
	// This test binary is cli.test, not plumb — so a live pid that is provably
	// not plumb is available without spawning anything.
	if isPlumbProcess(os.Getpid()) {
		t.Error("the test binary was identified as plumb")
	}
	if isPlumbProcess(-1) || isPlumbProcess(0) {
		t.Error("an impossible pid was identified as a live plumb")
	}
}

// TestReclaimableLock_LiveNonPlumbPid: a lock whose recorded pid is alive but is
// NOT plumb belongs to nothing — the number was reused after a crash or reboot.
// Without this the stand-down is permanent for a resumed conversation, since the
// age escape applies only to an unstamped lock.
func TestReclaimableLock_LiveNonPlumbPid(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "grey-lynx.lock")
	if err := os.Mkdir(lock, 0o755); err != nil {
		t.Fatal(err)
	}
	// A live pid that is not plumb: this test process.
	if err := os.WriteFile(filepath.Join(lock, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lock, "conv"), []byte("conv-1"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Same conversation — the resumed-session case, where the old code stood
	// down forever.
	neverPlumb := func(int) bool { return false }
	if !reclaimableLock(lock, "conv-1", neverPlumb) {
		t.Error("a lock held by a live NON-plumb pid was not reclaimable — the session is unwakeable forever")
	}
	// The control: the same live pid, but it IS a plumb — our own watcher, so
	// standing down is correct and the lock must NOT be reclaimable.
	if reclaimableLock(lock, "conv-1", func(int) bool { return true }) {
		t.Error("stole a lock held by our own live watcher")
	}
	// And with no conversation recorded at all.
	if err := os.Remove(filepath.Join(lock, "conv")); err != nil {
		t.Fatal(err)
	}
	if !reclaimableLock(lock, "conv-1", neverPlumb) {
		t.Error("an unattributed lock held by a live non-plumb pid was not reclaimable")
	}
}

// TestTerminate_RefusesAStrangersProcess pins the safety property directly, with
// a real child process: after a crash or a reboot the pid in a lock file is
// likely to have been reused, and signalling whatever now holds it is a far
// worse failure than leaving a dead lock behind.
func TestTerminate_RefusesAStrangersProcess(t *testing.T) {
	victim := exec.Command("sleep", "30")
	if err := victim.Start(); err != nil {
		t.Skipf("cannot spawn a child process here: %v", err)
	}
	defer func() {
		_ = victim.Process.Kill()
		_ = victim.Wait()
	}()

	if terminate(victim.Process.Pid, func(int) bool { return false }) {
		t.Error("signalled a live process that is not a plumb")
	}
	if !processAlive(victim.Process.Pid) {
		t.Fatal("the stranger's process was killed")
	}

	// The control: when it IS ours, the signal must actually go.
	if !terminate(victim.Process.Pid, func(int) bool { return true }) {
		t.Error("refused to signal a stale plumb watcher")
	}
}
