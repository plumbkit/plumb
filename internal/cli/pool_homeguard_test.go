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
	"context"
	"os"
	"os/user"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/paths"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/sessionstate"
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

// TestSynthesiseRoot_RefusesAWideSeedForEveryCaller is round 4's finding, and
// the reason the refusal is now a property of the RETURN rather than a check at
// one caller.
//
// Round 3 put the containment refusal in repinWorkspaceFrom. Review then showed
// the other two consumers of SynthesiseRoot still pinned the wide root: the
// first-tool-call seeding path — find_files and search_in_files take a
// DIRECTORY as `path`, so a call naming /Users seeds it directly — and
// rehydratePin replaying a roots-origin row, which runs unconditionally in the
// default configuration. Stopping the WALK could not fix either, because the
// walk's fallback returns the seed, and here the seed IS the offending
// directory.
//
// Asserting on SynthesiseRoot directly is deliberate: it is the one place all
// three routes pass through, so this cannot be satisfied by fixing a fourth
// caller and leaving a fifth.
func TestSynthesiseRoot_RefusesAWideSeedForEveryCaller(t *testing.T) {
	base := freshTempDir(t) // stands in for /Users — it CONTAINS the home directory
	home := filepath.Join(base, "home")
	mustMkdir(t, home)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	mustMkdir(t, filepath.Join(base, ".git")) // a marker that would otherwise win

	pool := &workspacePool{}
	for _, seed := range []string{base, home, "/"} {
		if got := pool.SynthesiseRoot(seed, false); got != "" {
			t.Errorf("SynthesiseRoot(%q, explicit=false) = %q, want \"\" — a seed at or "+
				"above the home directory must be refused, not resolved", seed, got)
		}
	}

	// Control 1: issue #182 — an explicitly named wide root still resolves.
	if got := pool.SynthesiseRoot(base, true); got == "" {
		t.Errorf("an EXPLICIT seed of %q was refused; #182 says an explicit pin always succeeds", base)
	}
	// Control 2: the ordinary case is untouched — a markerless dir under the home
	// directory still synthesises to itself, which is where most projects live.
	seed := filepath.Join(home, "scratch", "markerless")
	mustMkdir(t, seed)
	if got, want := pool.SynthesiseRoot(seed, false), paths.Canonical(seed); got != want {
		t.Errorf("SynthesiseRoot(%q) = %q, want %q — the refusal must not break ordinary seeds", seed, got, want)
	}
}

// TestRepin_NonExplicitRootContainingHomeIsRefused pins the refusal at the pin
// itself, which reports the problem at the call that caused it rather than at
// the next path-bearing tool.
func TestRepin_NonExplicitRootContainingHomeIsRefused(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	base := freshTempDir(t) // stands in for /Users — it CONTAINS the home directory
	home := filepath.Join(base, "home")
	mustMkdir(t, home)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	mustMkdir(t, filepath.Join(base, ".git")) // so Detect would otherwise resolve it

	store := config.NewStore(config.Defaults())
	s := newConnSession(context.Background(), detectTestPool(), nil, store, nil, nil, newSharedBudgets())
	defer s.close()

	// PinSourceRoots: a client REPORTING this folder, not a caller declaring it.
	if _, err := s.repinWorkspaceFrom(context.Background(), base, "", sessionstate.PinSourceRoots, pinTriggerLive, false); err == nil {
		t.Errorf("a non-explicit re-pin to %q was accepted; it contains the home directory %q, "+
			"so every credential under it would be inside the boundary", base, home)
	}

	// Control 1: the same directory named EXPLICITLY still pins — issue #182 says
	// an explicit pin always succeeds, and this refusal must not weaken that.
	if _, err := s.repinWorkspaceFrom(context.Background(), base, "", sessionstate.PinSourceSessionStart, pinTriggerLive, true); err != nil {
		t.Errorf("control failed — an EXPLICIT pin of %q was refused: %v", base, err)
	}

	// Control 2: an ordinary project, which contains no home directory, still
	// pins from a non-explicit origin — so the refusal is about containment and
	// not a re-pin that has stopped working.
	proj := filepath.Join(home, "proj")
	mustMkdir(t, filepath.Join(proj, ".git"))
	if _, err := s.repinWorkspaceFrom(context.Background(), proj, "", sessionstate.PinSourceRoots, pinTriggerLive, true); err != nil {
		t.Errorf("control failed — an ordinary project re-pin was refused: %v", err)
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
	base := freshTempDir(t) // stands in for /Users — CONTAINS the home directory
	home := filepath.Join(base, "home")
	mustMkdir(t, home)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	if err := materialisePlumbDir(home); err == nil {
		t.Fatal("materialisePlumbDir($HOME) succeeded; want a refusal")
	}
	if _, err := os.Stat(filepath.Join(home, ".plumb")); err == nil {
		t.Fatal("materialisePlumbDir($HOME) created ~/.plumb despite refusing")
	}

	// A directory CONTAINING the home directory, not just the home directory
	// itself. Found as a surviving mutation: minting a marker at /Users makes
	// detect() succeed there for every later session with no declaration at all,
	// which turns one explicit pin into a standing workspace holding every home
	// directory on the machine.
	if err := materialisePlumbDir(base); err == nil {
		t.Errorf("materialisePlumbDir(%q) succeeded; it contains the home directory %q", base, home)
	}
	if _, err := os.Stat(filepath.Join(base, ".plumb")); err == nil {
		t.Errorf("materialisePlumbDir(%q) created the marker despite refusing", base)
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
