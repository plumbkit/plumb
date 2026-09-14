package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/session"
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

// watchForPeerMail had no direct test at all before the adaptive window: only
// its definition and its single call site existed, so the stand-down state
// machine and the window-expiry path were both entirely uncovered — and they
// are exactly the code a longer window stresses.
//
// Each case asserts the PROBE COUNT, which is what the rule under test actually
// decides. watchForPeerMail sleeps in real time, so the windows here are kept
// small; the counts stay correct on a slow runner because every rule is
// evaluated per poll rather than against the clock.
func TestWatchForPeerMail(t *testing.T) {
	const mail = 1

	for _, tc := range []struct {
		name      string
		windows   wakeWindowPair
		peers     int // what the probe that armed the watcher saw
		ok        bool
		steps     []probeStep
		wantCalls int
		wantWake  bool
		why       string
	}{
		{
			name:      "expires with no mail",
			windows:   wakeWindowPair{base: time.Second},
			ok:        true,
			steps:     []probeStep{{ok: true}},
			wantCalls: 1,
			why:       "a quiet mailbox must end the watch at the base window",
		},
		{
			name:      "a peer extends past the base window",
			windows:   wakeWindowPair{base: time.Second, peak: 3 * time.Second},
			peers:     1,
			ok:        true,
			steps:     []probeStep{{peers: 1, ok: true}},
			wantCalls: 3,
			why:       "a live peer can still send, so the watch runs to the ceiling",
		},
		{
			name:      "no peer keeps the base window",
			windows:   wakeWindowPair{base: time.Second, peak: 10 * time.Second},
			peers:     0,
			ok:        true,
			steps:     []probeStep{{ok: true}},
			wantCalls: 1,
			why:       "nobody can write to this mailbox, so the ceiling must not be paid for",
		},
		{
			name:      "a departed peer ends the watch one base window later",
			windows:   wakeWindowPair{base: time.Second, peak: 10 * time.Second},
			peers:     1,
			ok:        true,
			steps:     []probeStep{{peers: 1, ok: true}, {ok: true}},
			wantCalls: 2,
			why:       "the deadline stops sliding when the peer goes, rather than running to the ceiling",
		},
		{
			name:      "a zero ceiling restores the single fixed window",
			windows:   wakeWindowPair{base: time.Second, peak: 0},
			peers:     5,
			ok:        true,
			steps:     []probeStep{{peers: 5, ok: true}},
			wantCalls: 1,
			why:       "peak=0 is the documented opt-out and must ignore peers entirely",
		},
		{
			name:      "an unresolved session never extends",
			windows:   wakeWindowPair{base: time.Second, peak: 60 * time.Second},
			peers:     3, // a stale count from before the session stopped resolving
			ok:        false,
			steps:     []probeStep{{peers: 3, ok: false}},
			wantCalls: 1,
			why: "a probe that cannot resolve has no mailbox to wake for; extending would " +
				"hold this session's lock for the whole ceiling with no way to stand down",
		},
		{
			name:    "two consecutive misses stand the watcher down",
			windows: wakeWindowPair{base: 5 * time.Second, peak: 60 * time.Second},
			peers:   1,
			ok:      true,
			steps: []probeStep{
				{peers: 1, ok: true}, // extends the deadline to ~6s
				{ok: false},          // gone = 1
				{ok: false},          // gone = 2 — stand down well before the deadline
			},
			wantCalls: 3,
			why:       "the client exited; nothing kills a reparented watcher but this check",
		},
		{
			name:    "a single missed poll does not retire a live watcher",
			windows: wakeWindowPair{base: 5 * time.Second},
			peers:   1,
			ok:      true,
			steps: []probeStep{
				{peers: 1, ok: true},
				{ok: false}, // a daemon blip, not a dead session
				{report: mailReport{Count: mail}, peers: 1, ok: true},
			},
			wantCalls: 3,
			wantWake:  true,
			why:       "one miss is why the stand-down needs TWO consecutive misses",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PLUMB_WAKE_INTERVAL", "1")
			probe, calls := scriptedProbe(tc.steps...)
			wake := watchForPeerMail(
				claudeHookInput{SessionID: "conv-1"},
				mailReport{}, tc.peers, tc.ok, probe, tc.windows)

			if got := wake != nil; got != tc.wantWake {
				t.Errorf("woke = %v, want %v — %s", got, tc.wantWake, tc.why)
			}
			if *calls != tc.wantCalls {
				t.Errorf("polled %d time(s), want %d — %s", *calls, tc.wantCalls, tc.why)
			}
		})
	}
}

// TestHookPeerCount is the real-session-store half of the adaptive window: the
// watcher extends only while this count is above zero, so a count that were
// always 0 would silently restore the old fixed window, and one that were always
// positive would hold a watcher process for the full ceiling on every solo
// session on the machine.
func TestHookPeerCount(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	const here, elsewhere = "/tmp/plumb-peer-here", "/tmp/plumb-peer-elsewhere"
	register := func(name, folder string) {
		t.Helper()
		info, err := session.Register(session.Info{Name: name, Folder: folder})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { session.Unregister(info.ID) })
	}
	register("grey-lynx", here)
	register("swift-heron", here)
	register("calm-stag", elsewhere)

	for _, tc := range []struct {
		name      string
		workspace string
		self      string
		want      int
		why       string
	}{
		{"excludes self", here, "grey-lynx", 1, "a session is not its own peer; counting itself would extend every solo watch"},
		{"counts both when self is unknown", here, "", 2, ""},
		{
			"a different root is not a peer", elsewhere, "calm-stag", 0,
			"mail is workspace-scoped, so a session that cannot send here must not extend the watch",
		},
		{"an unclean path still matches", here + "/", "grey-lynx", 1, "roots are compared cleaned"},
		{"no workspace", "", "", 0, "an unresolved probe must read as no peers, not as all of them"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hookPeerCount(tc.workspace, tc.self); got != tc.want {
				t.Errorf("hookPeerCount(%q, %q) = %d, want %d — %s", tc.workspace, tc.self, got, tc.want, tc.why)
			}
		})
	}
}

// TestRuntimeWakeWindows_ProjectMayNarrowNotWiden pins the asymmetry the whole
// config story rests on.
//
// The Stop handler is installed once, machine-wide, with ONE timeout derived
// from the global ceiling. A project asking for a longer window would not get
// one — the client would kill the watcher at that timeout mid-watch, with
// nothing in any output saying so. Clamping is the honest version of a limit
// that exists whether or not plumb enforces it; narrowing is always safe,
// because a shorter watch simply ends early.
func TestRuntimeWakeWindows_ProjectMayNarrowNotWiden(t *testing.T) {
	cfgHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	// The env overrides are applied to global and project alike, so they would
	// mask the very difference under test.
	t.Setenv("PLUMB_WAKE_WINDOW", "")
	t.Setenv("PLUMB_WAKE_PEER_WINDOW", "")

	if err := os.MkdirAll(filepath.Join(cfgHome, "plumb"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgHome, "plumb", "config.toml"),
		[]byte("[collab]\nwake_window_seconds = 100\nwake_peer_window_seconds = 1000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A precondition, asserted rather than skipped on. If the global config ever
	// stops resolving through XDG_CONFIG_HOME this test would otherwise go quietly
	// green while asserting nothing — the clamp cases below compare against the
	// ceiling this line establishes.
	if got := globalWakeWindows(); got.base != 100*time.Second || got.peak != 1000*time.Second {
		t.Fatalf("global config did not resolve through XDG_CONFIG_HOME: windows = %v, want 100s/1000s", got)
	}

	project := func(t *testing.T, body string) string {
		t.Helper()
		ws := plumbWorkspace(t)
		if err := os.WriteFile(filepath.Join(ws, ".plumb", "config.toml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return ws
	}

	t.Run("narrowing is honoured", func(t *testing.T) {
		ws := project(t, "[collab]\nwake_window_seconds = 10\nwake_peer_window_seconds = 50\n")
		got := runtimeWakeWindows(ws)
		if got.base != 10*time.Second || got.peak != 50*time.Second {
			t.Errorf("windows = %v, want the project's own 10s/50s", got)
		}
	})

	t.Run("widening is clamped to the global ceiling", func(t *testing.T) {
		ws := project(t, "[collab]\nwake_window_seconds = 8000\nwake_peer_window_seconds = 9000\n")
		got := runtimeWakeWindows(ws)
		if got.base != 1000*time.Second || got.peak != 1000*time.Second {
			t.Errorf("windows = %v, want both clamped to the 1000s global ceiling — "+
				"a longer watch is killed by the installed timeout, not honoured", got)
		}
	})

	t.Run("no project config keeps the global pair", func(t *testing.T) {
		got := runtimeWakeWindows(plumbWorkspace(t))
		if got.base != 100*time.Second || got.peak != 1000*time.Second {
			t.Errorf("windows = %v, want the global 100s/1000s", got)
		}
	})
}

// TestSweepWakeDir: nothing ever cleaned the wake directory, so it grew a stamp
// per conversation forever and a lock per watcher that died before releasing
// one. The sweep must remove that debris WITHOUT touching anything a live
// session still depends on.
func TestSweepWakeDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PLUMB_WAKE_WINDOW", "1")
	t.Setenv("PLUMB_WAKE_PEER_WINDOW", "0")

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

	stale := write("gone-otter.stamp", claudeWakeStampTTL+time.Hour)
	staleRearm := write("gone-otter.rearm", claudeWakeStampTTL+time.Hour)
	fresh := write("live-otter.stamp", time.Minute)

	// A lock whose recorded pid is this test binary: alive, but not a plumb, so
	// the existing ladder calls it debris.
	dead := filepath.Join(dir, "dead-otter.lock")
	if err := os.Mkdir(dead, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dead, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}

	// A lock taken moments ago with no pid yet — another watcher mid-acquire.
	// Sweeping it would delete a lock that is about to be held.
	acquiring := filepath.Join(dir, "busy-otter.lock")
	if err := os.Mkdir(acquiring, 0o755); err != nil {
		t.Fatal(err)
	}

	sweepWakeDir(dir)

	for _, gone := range []string{stale, staleRearm, dead} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s survived the sweep", filepath.Base(gone))
		}
	}
	for _, kept := range []string{fresh, acquiring} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s was swept but is still in use: %v", filepath.Base(kept), err)
		}
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

// TestWakeInterval_ClampedToWindow: an interval longer than the window would
// park the watcher past its own deadline still holding the session's lock,
// since the loop re-checks the deadline only after sleeping.
func TestWakeInterval_ClampedToWindow(t *testing.T) {
	t.Setenv("PLUMB_WAKE_INTERVAL", "100000")
	if got := wakeInterval(10 * time.Second); got != 10*time.Second {
		t.Errorf("wakeInterval = %v, want it clamped to the 10s window", got)
	}
	t.Setenv("PLUMB_WAKE_INTERVAL", "3")
	if got := wakeInterval(10 * time.Second); got != 3*time.Second {
		t.Errorf("wakeInterval = %v, want the configured 3s", got)
	}
}

// TestClaudeHookEntries_TimeoutTracksATunedWindow: the entry's timeout is
// derived from the window this process would watch for, so tuning the window
// and re-installing keeps the client's cancel above the watcher's deadline.
// It must track the CEILING, not the base window: a timeout covering only the
// base would kill exactly the peer-extended watch the ceiling exists to allow,
// and kill it with nothing in any output saying so.
func TestClaudeHookEntries_TimeoutTracksATunedWindow(t *testing.T) {
	stopTimeout := func(t *testing.T) float64 {
		t.Helper()
		for _, e := range claudeHookEntries("/opt/plumb") {
			if e.event != "Stop" {
				continue
			}
			timeout, ok := e.handler["timeout"].(float64)
			if !ok {
				t.Fatalf("Stop timeout = %v, want a number", e.handler["timeout"])
			}
			return timeout
		}
		t.Fatal("no Stop entry in the Claude Code pack")
		return 0
	}

	t.Run("no peer extension", func(t *testing.T) {
		t.Setenv("PLUMB_WAKE_WINDOW", "900")
		t.Setenv("PLUMB_WAKE_PEER_WINDOW", "0")
		if got := stopTimeout(t); got != 900+claudeStopTimeoutSlack.Seconds() {
			t.Errorf("Stop timeout = %.0fs, want the 900s window plus slack", got)
		}
	})

	t.Run("ceiling outlives the base window", func(t *testing.T) {
		t.Setenv("PLUMB_WAKE_WINDOW", "900")
		t.Setenv("PLUMB_WAKE_PEER_WINDOW", "4000")
		if got := stopTimeout(t); got != 4000+claudeStopTimeoutSlack.Seconds() {
			t.Errorf("Stop timeout = %.0fs, want the 4000s CEILING plus slack — a timeout "+
				"derived from the base window kills every peer-extended watch", got)
		}
	})

	// A ceiling below the base is not a shorter watch: the base is always
	// watched, so the timeout must still cover it.
	t.Run("ceiling below the base window", func(t *testing.T) {
		t.Setenv("PLUMB_WAKE_WINDOW", "900")
		t.Setenv("PLUMB_WAKE_PEER_WINDOW", "10")
		if got := stopTimeout(t); got != 900+claudeStopTimeoutSlack.Seconds() {
			t.Errorf("Stop timeout = %.0fs, want the 900s base plus slack", got)
		}
	})
}

// TestAcquireWakeLock_UnstampedLockIsReclaimedByAge: standing down on a lock
// with no readable pid stops two watchers racing — but a watcher that died in
// that window would otherwise leave a lock nothing can ever claim, and a
// session that can never arm a watcher is silently unwakeable. No live watcher
// outlives its own window, so an unstamped lock older than one is debris.
func TestAcquireWakeLock_UnstampedLockIsReclaimedByAge(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PLUMB_WAKE_WINDOW", "1")
	t.Setenv("PLUMB_WAKE_PEER_WINDOW", "0") // the age bound is the ceiling; pin it
	lock := filepath.Join(dir, "grey-lynx.lock")
	if err := os.Mkdir(lock, 0o755); err != nil {
		t.Fatal(err)
	}

	// Fresh and unstamped: another watcher may be mid-acquire, so stand down.
	if _, ok := acquireWakeLock(dir, "grey-lynx", "conv-1"); ok {
		t.Error("stole a lock another watcher may have just taken")
	}

	// Older than any window a live watcher could still be inside: debris.
	old := time.Now().Add(-2 * (time.Second + claudeStopTimeoutSlack))
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
