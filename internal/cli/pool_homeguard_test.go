package cli

// pool_homeguard_test.go — the identity machinery behind every $HOME workspace
// guard: how the home directory is determined (homeDirInfos) and how a
// directory is compared against it (sameDirAs). The guards themselves are
// exercised where they live (pool_test.go, pool_synthesise_root_test.go); this
// file pins the two properties nothing else held:
//
//   - sameDirAs fails OPEN on an empty home set — found as a surviving
//     mutation: flipping that branch to true was green across the entire repo.
//   - homeDirInfos does not depend on $HOME alone — the daemon inherits the
//     client's environment, so an emptied or repointed $HOME must not disarm
//     the guard protecting the real home directory.

import (
	"os"
	"os/user"
	"testing"
)

// TestSameDirAs_NoHomeFailsOpen pins the deliberate fail-open: when no home
// directory is determinable the guards go inert (return false), rather than
// treating EVERY directory as $HOME — which would refuse all detection on a
// machine with a broken environment. This branch had zero coverage: mutating
// it to fail-closed was green across the whole repo.
func TestSameDirAs_NoHomeFailsOpen(t *testing.T) {
	dir := freshTempDir(t)
	if sameDirAs(dir, nil) {
		t.Errorf("sameDirAs(%q, nil) = true; an empty home set must fail OPEN (guards inert), not claim every directory is $HOME", dir)
	}
	if sameDirAs(dir, []os.FileInfo{}) {
		t.Errorf("sameDirAs(%q, empty) = true; want false", dir)
	}
}

// TestSameDirAs_MatchesAnyOfSeveralHomes: the guard holds against EVERY home
// identity in the set, not just the first — that is what keeps a repointed
// $HOME from shadowing the real one.
func TestSameDirAs_MatchesAnyOfSeveralHomes(t *testing.T) {
	a, b := freshTempDir(t), freshTempDir(t)
	ai, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	bi, err := os.Stat(b)
	if err != nil {
		t.Fatal(err)
	}
	infos := []os.FileInfo{ai, bi}
	if !sameDirAs(a, infos) || !sameDirAs(b, infos) {
		t.Error("sameDirAs must match any identity in the set")
	}
	if sameDirAs(freshTempDir(t), infos) {
		t.Error("sameDirAs matched a directory in neither identity")
	}
}

// TestHomeDirInfos_SurvivesEmptyHOME: the guard's home identity does not hinge
// on the $HOME environment variable. The daemon inherits its environment from
// whichever client spawned it, so HOME="" (or unset) must fall back to the OS
// user database instead of silently disarming every $HOME guard.
func TestHomeDirInfos_SurvivesEmptyHOME(t *testing.T) {
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		t.Skipf("no user database entry to fall back to: %v", err)
	}
	if _, err := os.Stat(u.HomeDir); err != nil {
		t.Skipf("user database home %q not stat-able: %v", u.HomeDir, err)
	}
	t.Setenv("HOME", "")
	if got := homeDirInfos(); len(got) == 0 {
		t.Error("homeDirInfos() = empty with HOME unset; must fall back to the OS user database (os/user.Current) so a client-controlled environment cannot disarm the guard")
	}
}

// TestHomeDirInfos_RealHomeGuardedDespiteRepointedHOME: repointing $HOME at a
// decoy directory must not remove the REAL home from the guarded set. Both
// identities are guarded: the decoy (it is the operative $HOME for a dotfiles
// checkout) and the user-database home (the directory actually holding the
// credentials the guard exists to protect).
func TestHomeDirInfos_RealHomeGuardedDespiteRepointedHOME(t *testing.T) {
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		t.Skipf("no user database entry to cross-check: %v", err)
	}
	realHome, err := os.Stat(u.HomeDir)
	if err != nil {
		t.Skipf("user database home %q not stat-able: %v", u.HomeDir, err)
	}
	decoy := freshTempDir(t)
	t.Setenv("HOME", decoy)

	infos := homeDirInfos()
	foundReal := false
	for _, info := range infos {
		if os.SameFile(info, realHome) {
			foundReal = true
		}
	}
	if !foundReal {
		t.Errorf("homeDirInfos() with HOME=%q dropped the user-database home %q; a repointed $HOME must not unguard the real home directory", decoy, u.HomeDir)
	}
	if !sameDirAs(decoy, infos) {
		t.Errorf("homeDirInfos() with HOME=%q does not guard the decoy; the operative $HOME must stay guarded too", decoy)
	}
}
