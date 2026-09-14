package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/session"
)

// How long a watcher watches, and everything that decides it: the watch loop's
// own rules, the window pair behind them, the peer count that extends them, and
// the installed timeout that must outlive the lot. The companion to
// hooks_claude_window.go; the handlers, stamps, locks and sweep are tested in
// hooks_claude_test.go.

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
			// The key matches what the probe resolves, so the superseded-key
			// stand-down is out of the way of every case here.
			wake := watchForPeerMail(
				claudeHookInput{SessionID: "conv-1"}, "grey-lynx",
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

// TestWatchForPeerMail_RetiresWhenItsKeyIsSuperseded: a turn ending while the
// daemon is down keys by conversation id; the next turn, with the daemon back,
// keys by the resolved session name and arms a SECOND watcher under a different
// lock. Without this the first one would extend to the ceiling alongside it —
// two wakes per message and two independent chain counters — and the longer the
// ceiling, the longer that duplicate lives.
func TestWatchForPeerMail_RetiresWhenItsKeyIsSuperseded(t *testing.T) {
	t.Setenv("PLUMB_WAKE_INTERVAL", "1")
	probe, calls := scriptedProbe(probeStep{report: mailReport{Session: "grey-lynx"}, peers: 1, ok: true})

	wake := watchForPeerMail(
		claudeHookInput{SessionID: "conv-1"},
		"conv-1", // armed before linkage resolved, so keyed by conversation id
		mailReport{}, 0, false, probe,
		wakeWindowPair{base: time.Second, peak: 60 * time.Second})

	if wake != nil {
		t.Error("a superseded watcher produced a wake")
	}
	if *calls != 1 {
		t.Errorf("polled %d time(s), want 1 — a watcher that learns it is keyed by the wrong "+
			"name must retire, not extend to the ceiling beside the watcher that owns that name", *calls)
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

// TestHookWakeProbe_ReportsLivePeers is the composition hookPeerCount's own test
// cannot reach: that hookWakeProbe actually asks for a peer count and returns
// it. A probe hardcoded to 0 peers passes every other test in this file, and
// silently restores the fixed window this change exists to replace.
func TestHookWakeProbe_ReportsLivePeers(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws := t.TempDir()

	me, err := session.Register(session.Info{Name: "grey-lynx", Folder: ws, ExternalID: "conv-1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Unregister(me.ID) })

	report, peers, ok := hookWakeProbe("conv-1", ws)
	if !ok || report.Session != "grey-lynx" {
		t.Fatalf("probe = (%+v, ok=%v), want the linked session resolved", report, ok)
	}
	if peers != 0 {
		t.Errorf("peers = %d with nobody else on this workspace, want 0 — a solo session "+
			"must not pay for the extended window", peers)
	}

	peer, err := session.Register(session.Info{Name: "swift-heron", Folder: ws})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Unregister(peer.ID) })

	if _, peers, _ := hookWakeProbe("conv-1", ws); peers != 1 {
		t.Errorf("peers = %d with one live peer on this workspace, want 1 — the watcher "+
			"never extends, so the long window is dead code", peers)
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
