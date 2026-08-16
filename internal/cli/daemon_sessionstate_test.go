package cli

// daemon_sessionstate_test.go — the daemon-start session-state maintenance.
//
// These exercise sweepLegacyWidePins with its PRODUCTION predicate binding.
// internal/sessionstate's own tests inject a stub, so without this file the
// whole feature could be turned off (`SweepWidePinsOnce(func(string) bool
// { return false })`) with every test still green — a surviving mutant an
// adversarial review demonstrated.

import (
	"os/user"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/sessionstate"
)

// The daemon's sweep really does remove a wide pin, using the real containment
// predicate rather than a test stub — and really does spare an ordinary project.
func TestSweepLegacyWidePins_UsesTheRealContainmentPredicate(t *testing.T) {
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		t.Skipf("no user-database home: %v", err)
	}
	wide := filepath.Dir(u.HomeDir)
	if wide == "/" || wide == "." || wide == "" {
		t.Skipf("home %q sits at the filesystem root; no container to test", u.HomeDir)
	}
	proj := freshTempDir(t)

	_, ss := newOriginStore(t)
	if err := ss.UpsertPin("forged", wide, LanguageNone, sessionstate.PinSourceSessionStart); err != nil {
		t.Fatal(err)
	}
	if err := ss.UpsertPin("honest", proj, LanguageNone, sessionstate.PinSourceSessionStart); err != nil {
		t.Fatal(err)
	}

	sweepLegacyWidePins(ss)

	if _, _, _, ok, err := ss.LoadPin("forged"); err != nil || ok {
		t.Errorf("the wide pin survived the daemon sweep (ok=%v err=%v) — the production predicate is not wired", ok, err)
	}
	if _, _, _, ok, err := ss.LoadPin("honest"); err != nil || !ok {
		t.Errorf("an ordinary project pin was swept (ok=%v err=%v)", ok, err)
	}
}

// Nil-safe: a daemon whose session-state store failed to open passes nil, and
// the sweep must not panic on the startup path.
func TestSweepLegacyWidePins_NilStore(t *testing.T) {
	sweepLegacyWidePins(nil) // must not panic
}
