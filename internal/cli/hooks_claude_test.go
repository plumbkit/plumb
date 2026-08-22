package cli

import (
	"os"
	"path/filepath"
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
	return dir
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
		func(_, _ string) (mailReport, bool) {
			return mailReport{Session: "grey-lynx", Count: 2, AgesSeconds: []int{31, 4}}, true
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
		func(_, _ string) (mailReport, bool) {
			return mailReport{Session: "grey-lynx"}, true
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
		func(_, _ string) (mailReport, bool) { return mailReport{}, false })

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
		func(_, _ string) (mailReport, bool) {
			probed = true
			return mailReport{Session: "grey-lynx", Count: 5}, true
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
		func(_, _ string) (mailReport, bool) { return mailReport{}, false }); wake != nil {
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
		func(_, _ string) (mailReport, bool) {
			return mailReport{Session: "grey-lynx", Count: 1}, true // unchanged: not consumed
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
		func(_, _ string) (mailReport, bool) {
			return mailReport{Session: "grey-lynx", Count: 1, AgesSeconds: []int{9}}, true
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

	first, ok := acquireWakeLock(dir, "grey-lynx", "conv-1")
	if !ok {
		t.Fatal("first watcher could not take the lock")
	}
	if _, ok := acquireWakeLock(dir, "grey-lynx", "conv-1"); ok {
		t.Error("the same conversation stacked a second watcher")
	}
	// A different conversation answering to the same session name IS that
	// session now, so it takes the lock over.
	second, ok := acquireWakeLock(dir, "grey-lynx", "conv-2")
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

	third, ok := acquireWakeLock(dir, "grey-lynx", "conv-3")
	if !ok {
		t.Fatal("lock was not reclaimable after release")
	}
	third.release()
	third.release()
}

func TestInsidePlumbWorkspace(t *testing.T) {
	ws := plumbWorkspace(t)
	nested := filepath.Join(ws, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if !insidePlumbWorkspace(nested) {
		t.Error("a directory below the marker did not resolve as inside the workspace")
	}
	if insidePlumbWorkspace(nonWorkspaceDir(t)) {
		t.Error("an unrelated directory resolved as inside a plumb workspace")
	}
	if insidePlumbWorkspace("") {
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
		func(_, _ string) (mailReport, bool) { return mailReport{Count: 3}, true })

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
	t.Setenv("PLUMB_WAKE_WINDOW", "10")
	t.Setenv("PLUMB_WAKE_INTERVAL", "100000")
	if got := wakeInterval(); got != 10*time.Second {
		t.Errorf("wakeInterval = %v, want it clamped to the 10s window", got)
	}
	t.Setenv("PLUMB_WAKE_INTERVAL", "3")
	if got := wakeInterval(); got != 3*time.Second {
		t.Errorf("wakeInterval = %v, want the configured 3s", got)
	}
}

// TestClaudeHookEntries_TimeoutTracksATunedWindow: the entry's timeout is
// derived from the window this process would watch for, so tuning the window
// and re-installing keeps the client's cancel above the watcher's deadline.
func TestClaudeHookEntries_TimeoutTracksATunedWindow(t *testing.T) {
	t.Setenv("PLUMB_WAKE_WINDOW", "900")
	for _, e := range claudeHookEntries("/opt/plumb") {
		if e.event != "Stop" {
			continue
		}
		timeout, _ := e.handler["timeout"].(float64)
		if timeout <= 900 {
			t.Errorf("Stop timeout %.0fs does not outlive a tuned 900s window", timeout)
		}
		return
	}
	t.Fatal("no Stop entry in the Claude Code pack")
}

// TestAcquireWakeLock_UnstampedLockIsReclaimedByAge: standing down on a lock
// with no readable pid stops two watchers racing — but a watcher that died in
// that window would otherwise leave a lock nothing can ever claim, and a
// session that can never arm a watcher is silently unwakeable. No live watcher
// outlives its own window, so an unstamped lock older than one is debris.
func TestAcquireWakeLock_UnstampedLockIsReclaimedByAge(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PLUMB_WAKE_WINDOW", "1")
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
