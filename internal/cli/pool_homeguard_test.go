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
	"path/filepath"
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

// TestDetect_ResidualPlumbAtHomeIgnored is finding B2 on PR #288: detect
// consulted the .plumb marker BEFORE any home guard, so the bare ~/.plumb an
// earlier build's auto_attach_persist created kept resolving $HOME as the
// workspace — with auto_attach OFF, surviving every restart, making the
// SynthesiseRoot fix inert on any machine carrying the residue. At the home
// directory a .plumb now needs evidence of human intent (context.md or
// config.toml); a bare or purely machine-generated one is ignored.
func TestDetect_ResidualPlumbAtHomeIgnored(t *testing.T) {
	home := freshTempDir(t) // not t.TempDir: see the GOTMPDIR note in pool_synthesise_root_test.go
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	mustMkdir(t, filepath.Join(home, ".plumb"))
	// Daemon artefacts prove nothing about intent — they appear wherever a
	// session ran. The residue must stay residue with them present.
	mustWrite(t, filepath.Join(home, ".plumb", "topology.db"), "sqlite\n")
	sub := filepath.Join(home, "notes")
	mustMkdir(t, sub)

	pool := detectTestPool()
	if root, _, err := pool.Detect(sub); err == nil {
		t.Fatalf("Detect(%q) = %q; a bare ~/.plumb (auto_attach_persist residue) must not resolve the home directory as a workspace", sub, root)
	}
	if root, _, err := pool.Detect(home); err == nil {
		t.Fatalf("Detect(%q) = %q; want an error for the residue marker", home, root)
	}
}

// TestDetect_DeliberatePlumbAtHomeHonoured is the control for the residue
// guard: a user who ran `plumb init` in their home directory has declared the
// intent the guard exists to demand, and that marker must keep working.
//
// context.md ONLY. An earlier version of this test also accepted config.toml,
// and review showed why that defeats the guard: config.toml is MACHINE-written
// by config.SetProjectValue, whose callers include the agent_config tool, the
// web settings API, and — with no opt-in at all — any project-scoped save in
// the TUI. A user who pinned $HOME once and then changed a single setting would
// have minted their own permanent proof of intent. The config.toml case below
// now asserts the OPPOSITE of what it used to, deliberately.
func TestDetect_DeliberatePlumbAtHomeHonoured(t *testing.T) {
	for _, tc := range []struct {
		evidence   string
		honoured   bool
		whyRefused string
	}{
		{evidence: "context.md", honoured: true},
		{evidence: "config.toml", honoured: false, whyRefused: "machine-written by SetProjectValue from the TUI, the web API and agent_config, so it proves nothing about intent"},
	} {
		t.Run(tc.evidence, func(t *testing.T) {
			home := freshTempDir(t)
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			mustMkdir(t, filepath.Join(home, ".plumb"))
			mustWrite(t, filepath.Join(home, ".plumb", tc.evidence), "# marker\n")
			sub := filepath.Join(home, "notes")
			mustMkdir(t, sub)

			root, lang, err := detectTestPool().Detect(sub)
			if !tc.honoured {
				if err == nil {
					t.Errorf("Detect resolved %q as the workspace on the strength of a %s — %s",
						root, tc.evidence, tc.whyRefused)
				}
				return
			}
			if err != nil {
				t.Fatalf("Detect: %v — a deliberate ~/.plumb (%s) must be honoured", err, tc.evidence)
			}
			if root != home {
				t.Errorf("root = %q, want %q", root, home)
			}
			if lang != LanguageNone {
				t.Errorf("language = %q, want %q", lang, LanguageNone)
			}
		})
	}
}

// TestDetect_ReachingHomeStopsTheWalk is detect's half of finding S4: the old
// guard SKIPPED the home directory's markers but kept climbing, so a .git (or
// any marker) ABOVE the home directory resolved $HOME's parent as the
// workspace — wider than the $HOME capture the guard blocks. Reaching the
// home directory now terminates the walk with an error.
func TestDetect_ReachingHomeStopsTheWalk(t *testing.T) {
	base := freshTempDir(t)
	mustMkdir(t, filepath.Join(base, ".git")) // a repo ABOVE the home directory
	home := filepath.Join(base, "home")
	mustMkdir(t, home)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	sub := filepath.Join(home, "notes")
	mustMkdir(t, sub)

	if root, _, err := detectTestPool().Detect(sub); err == nil {
		t.Fatalf("Detect(%q) = %q; the walk must stop at the home directory, not resolve an ancestor above it", sub, root)
	}
}

// TestDetect_ProjectUnderResidualHomeStillResolves: the residue guard is about
// the home directory itself — a real project beneath it (where projects live)
// must be untouched by the ~/.plumb sitting above it.
func TestDetect_ProjectUnderResidualHomeStillResolves(t *testing.T) {
	home := freshTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	mustMkdir(t, filepath.Join(home, ".plumb")) // residue
	proj := filepath.Join(home, "Projects", "app")
	mustMkdir(t, filepath.Join(proj, ".git"))
	sub := filepath.Join(proj, "internal")
	mustMkdir(t, sub)

	root, _, err := detectTestPool().Detect(sub)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if root != proj {
		t.Errorf("root = %q, want the project %q", root, proj)
	}
}

// TestMaterialisePlumbDir_RefusesHome: auto_attach_persist must never mint the
// residue the detect guard exists to ignore. Even an explicit $HOME pin
// attaches for the session only — a machine-created ~/.plumb outlives the pin
// and would re-open the whole-home workspace for every later session.
func TestMaterialisePlumbDir_RefusesHome(t *testing.T) {
	home := freshTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	if err := materialisePlumbDir(home); err == nil {
		t.Fatal("materialisePlumbDir($HOME) succeeded; want a refusal")
	}
	if _, err := os.Stat(filepath.Join(home, ".plumb")); err == nil {
		t.Fatal("materialisePlumbDir($HOME) created ~/.plumb despite refusing")
	}

	// Control: an ordinary synthetic root still materialises.
	proj := filepath.Join(home, "scratch")
	mustMkdir(t, proj)
	if err := materialisePlumbDir(proj); err != nil {
		t.Fatalf("materialisePlumbDir(%q): %v", proj, err)
	}
	if _, err := os.Stat(filepath.Join(proj, ".plumb")); err != nil {
		t.Fatalf("materialisePlumbDir(%q) did not create the marker: %v", proj, err)
	}
}
