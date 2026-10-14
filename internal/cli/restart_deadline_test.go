package cli

import (
	"os"
	"strconv"
	"testing"
	"time"
)

// TestRunRestart_TheConfirmationDoesNotSpendTheDeadline pins PLAN-552 MINOR-4:
// restart's one deadline bounds the exit check and the successor's start, and it
// must not start before the confirmation prompt, which blocks on a human. With the
// clock started first, a confirmation slower than the launcher deadline hands
// respawn a deadline already in the past — the lock acquisition cannot wait at all,
// and a daemon still reaping at that instant leaves the account with none.
//
// The prompt is replaced by one that takes longer than the whole (shortened)
// launcher deadline, and respawn by a recorder: the deadline it receives must
// still be in the future.
func TestRunRestart_TheConfirmationDoesNotSpendTheDeadline(t *testing.T) {
	withShortTempRuntime(t)
	withAccountAuthority(t)
	t.Setenv("PLUMB_DAEMON_START_TIMEOUT", "300ms")

	// A stand-in daemon that is reaped the moment it exits, so the stop phase sees
	// it go instead of waiting out a zombie.
	daemon := startReapedSleep(t)
	stubDaemonIdentity(t, func(pid int) bool { return pid == daemon })

	loc := currentLocation()
	if err := os.MkdirAll(loc.Dir, 0o700); err != nil {
		t.Fatalf("creating %s: %v", loc.Dir, err)
	}
	if err := os.WriteFile(loc.PID, []byte(strconv.Itoa(daemon)), 0o600); err != nil {
		t.Fatalf("writing %s: %v", loc.PID, err)
	}

	oldForce, oldConfirm, oldRespawn := restartFlagForce, restartConfirm, restartRespawn
	t.Cleanup(func() { restartFlagForce, restartConfirm, restartRespawn = oldForce, oldConfirm, oldRespawn })
	restartFlagForce = false
	prompted := false
	restartConfirm = func(daemonActionPrompt) (bool, bool, error) {
		prompted = true
		time.Sleep(2 * daemonStartWait()) // the human takes longer than the whole deadline
		return true, false, nil
	}
	var got time.Time
	var at time.Time
	restartRespawn = func(_ []int, deadline time.Time) error {
		got, at = deadline, time.Now()
		return nil
	}

	if err := runRestart(nil, nil); err != nil {
		t.Fatalf("runRestart: %v", err)
	}
	if !prompted {
		t.Fatal("the restart never asked: the probe did not exercise the prompt")
	}
	if got.IsZero() {
		t.Fatal("the restart never reached respawn")
	}
	if !at.Before(got) {
		t.Fatalf("respawn was handed a deadline %s in the past: the confirmation prompt spent the launcher deadline", at.Sub(got))
	}
}
