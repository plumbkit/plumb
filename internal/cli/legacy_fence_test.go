package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The daemon's half of the legacy fence: what a `plumb daemon` holds, and what it
// refuses to start beside. Split from legacy_test.go by responsibility (PLAN-530
// review findings).

// TestLegacyLocationsIncluding_CoversTheHandedLocations pins the union the daemon's
// refusal and its census reason about: a location a LAUNCHER fenced belongs in it
// even though this process's own environment never names it, because in canonical
// mode the launcher is the client and the daemon runs with the service profile's
// environment. Emptied, the handed arm silently narrows what the daemon protects.
func TestLegacyLocationsIncluding_CoversTheHandedLocations(t *testing.T) {
	withShortTempRuntime(t)
	withAccountAuthority(t)

	handed := filepath.Join(t.TempDir(), "cache", "plumb", "plumb.daemon.lock")
	if err := os.MkdirAll(filepath.Dir(handed), 0o700); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(handed), err)
	}
	want := filepath.Dir(handed)
	got := legacyLocationsIncluding([]heldLegacyLock{{Path: handed}})
	if !slices.ContainsFunc(got, func(l legacyLocation) bool { return l.Dir == want }) {
		dirs := make([]string, 0, len(got))
		for _, l := range got {
			dirs = append(dirs, l.Dir)
		}
		t.Fatalf("legacyLocationsIncluding does not cover the handed location %s: got %v", want, dirs)
	}
}

// TestHoldLegacyLocks_HoldsTheDaemonsOwnEnvironmentToo is the MUST-FIX finding of
// the independent review of #661, pinned.
//
// In canonical mode the launcher and the daemon do not share an environment: the
// launcher is the client (whose HOME may be spoofed) and hands over the locks THAT
// environment named, while the daemon runs with the service profile's environment.
// Holding only the handed set leaves the daemon's OWN legacy location free — and
// that is where an ordinary pre-527 proxy dials and respawns: an old binary knows
// nothing of the account authority, so the legacy lifetime lock is the only thing
// that stops it starting a successor beside the canonical daemon.
func TestHoldLegacyLocks_HoldsTheDaemonsOwnEnvironmentToo(t *testing.T) {
	withShortTempRuntime(t)
	withAccountAuthority(t)

	// The DAEMON's own environment: what the service profile gives it.
	daemonHome := t.TempDir()
	t.Setenv("HOME", daemonHome)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(daemonHome, "cache"))
	own := legacyLocations()
	if len(own) == 0 {
		t.Fatal("the daemon's own environment names no legacy location on this platform")
	}
	ownLock := own[0].Lock

	// The LAUNCHER's environment: a different HOME, whose location it fenced and
	// handed over, exactly as acquireLegacyLocks + encodeLegacyHandover do.
	spoofHome := t.TempDir()
	spoof := legacyLocationAt(filepath.Join(spoofHome, "cache", "plumb"))
	if err := os.MkdirAll(spoof.Dir, 0o700); err != nil {
		t.Fatalf("creating %s: %v", spoof.Dir, err)
	}
	handed, err := acquireLegacyLock(spoof.Lock)
	if err != nil {
		t.Fatalf("acquireLegacyLock(%s): %v", spoof.Lock, err)
	}
	defer func() { _ = handed.File.Close() }()
	t.Setenv(legacyLocksEnv, fmt.Sprintf(`[{"fd":%d,"path":%q}]`, handed.File.Fd(), spoof.Lock))

	adopted, err := holdLegacyLocks()
	if err != nil {
		t.Fatalf("holdLegacyLocks with a handover: %v", err)
	}

	// An old launcher under the DAEMON's own environment must not be able to win
	// this daemon's own location.
	if old, err := acquireLegacyLock(ownLock); err == nil {
		_ = old.File.Close()
		t.Fatalf("the daemon does not hold its own environment's legacy lock %s: an old launcher can take it and start a legacy daemon beside the canonical one", ownLock)
	}
	// And the handed location is still held, as it was before.
	if again, err := acquireLegacyLock(spoof.Lock); err == nil {
		_ = again.File.Close()
		t.Fatal("the handed location is no longer held")
	}

	// Release the fence before the control below, which acquires the same
	// locations a second time: flock is per open file description, so a second
	// acquisition of a path this process already holds refuses — which is exactly
	// what makes the assertions above meaningful.
	closeHeldLegacyLocks(adopted)

	// POSITIVE CONTROL: with no handover, every location the daemon's own
	// environment names is taken.
	_ = os.Unsetenv(legacyLocksEnv)
	held, err := holdLegacyLocks()
	if err != nil {
		t.Fatalf("holdLegacyLocks without a handover: %v", err)
	}
	defer closeHeldLegacyLocks(held)
	if len(held) != len(own) {
		t.Fatalf("without a handover the daemon holds %d locks, want one per own location (%d)", len(held), len(own))
	}
	if ctrl, err := acquireLegacyLock(ownLock); err == nil {
		_ = ctrl.File.Close()
		t.Fatalf("positive control failed: %s was free even without a handover", ownLock)
	}
}
