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
	"runtime"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/sessionstate"
	"github.com/plumbkit/plumb/internal/tools"
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

// TestHomeUnder_FixtureShapes pins homeUnder's probe semantics on fixtures:
// containment is answered by identity, for every suffix of home's components,
// regardless of how the candidate spells the route.
func TestHomeUnder_FixtureShapes(t *testing.T) {
	base := freshTempDir(t)
	home := filepath.Join(base, "home")
	mustMkdir(t, home)
	mustMkdir(t, filepath.Join(base, "a", "b"))
	deep := filepath.Join(base, "a", "b", "home2")
	mustMkdir(t, deep)
	sibling := filepath.Join(base, "other")
	mustMkdir(t, sibling)

	for _, tc := range []struct {
		name string
		dir  string
		want bool
	}{
		{"direct container", base, true},
		{"deep home", base, true},
		{"identity", home, true},
		{"identity with trailing slash", home + string(filepath.Separator), true},
		{"sibling is not a container", sibling, false},
		{"a directory under home is not a container", filepath.Join(home, "proj"), false},
		{"unrelated directory", freshTempDir(t), false},
		{"empty candidate", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			homeFor := home
			if tc.name == "deep home" {
				homeFor = deep
			}
			if got := homeUnder(tc.dir, homeFor); got != tc.want {
				t.Errorf("homeUnder(%q, %q) = %v, want %v", tc.dir, homeFor, got, tc.want)
			}
		})
	}
	if homeUnder("", home) {
		t.Error("homeUnder(\"\", home) = true; an empty candidate is never a container")
	}
	if homeUnder(base, "") {
		t.Error("homeUnder(base, \"\") = true; an empty home disables the predicate")
	}
}

// TestHomeUnder_SymlinkedSpellingOfContainer: a candidate reached THROUGH a
// symlink is still a container — the kernel resolves the probe join, which is
// the whole point of probing instead of walking.
func TestHomeUnder_SymlinkedSpellingOfContainer(t *testing.T) {
	base := freshTempDir(t)
	home := filepath.Join(base, "home")
	mustMkdir(t, home)
	alias := filepath.Join(freshTempDir(t), "base-alias")
	if err := os.Symlink(base, alias); err != nil {
		t.Skipf("symlinks unsupported on this filesystem: %v", err)
	}
	if !homeUnder(alias, home) {
		t.Errorf("homeUnder(%q, %q) = false — a symlinked spelling of a container is still a container; the probe join is resolved by the kernel", alias, home)
	}
}

// TestHomeUnder_SymlinkInsideCandidate_DocumentedLimit pins the KNOWN limit of
// the shipped predicate: a symlink BELOW the candidate that reaches home under
// a name of its own is not caught, because the probe set only enumerates
// home's own component names (see homeUnder's doc comment and
// docs/threat-model.md). If a future guard closes this limit, update this
// test and the threat model together — silently changing either one is how
// the claim and the code drift apart.
func TestHomeUnder_SymlinkInsideCandidate_DocumentedLimit(t *testing.T) {
	base := freshTempDir(t)
	home := freshTempDir(t)
	link := filepath.Join(base, "sneaky")
	if err := os.Symlink(home, link); err != nil {
		t.Skipf("symlinks unsupported on this filesystem: %v", err)
	}
	if homeUnder(base, home) {
		t.Errorf("homeUnder caught a symlink-inside-the-candidate shape the shipped predicate documents as out of scope — update homeUnder's doc comment and docs/threat-model.md with the new coverage")
	}
}

// TestHomeUnder_MacOSSystemVolumeAliases runs the exact shapes issue #306 was
// defeated by, on a real macOS machine: /System/Volumes/Data CONTAINS $HOME
// while sharing an inode with neither /Users nor /, and
// /System/Volumes/Data/Users IS /Users (same inode) yet is not a lexical
// ancestor of $HOME's spelling. No fixture can reproduce firmlinks; both are
// probed live and skipped where the platform is absent.
func TestHomeUnder_MacOSSystemVolumeAliases(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS firmlink shapes only")
	}
	if _, err := os.Stat("/System/Volumes/Data/Users"); err != nil {
		t.Skipf("no system-volume alias to probe: %v", err)
	}
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		t.Skipf("no user-database home to contain: %v", err)
	}
	if _, err := os.Stat(u.HomeDir); err != nil {
		t.Skipf("home %q not stat-able: %v", u.HomeDir, err)
	}

	for _, dir := range []string{"/Users", "/", "/System/Volumes/Data", "/System/Volumes/Data/Users", filepath.Dir(u.HomeDir)} {
		if !homeUnder(dir, u.HomeDir) {
			t.Errorf("homeUnder(%q, %q) = false — a directory containing the home directory must be caught, whatever spelling names it (issue #306)", dir, u.HomeDir)
		}
	}
	if homeUnder(freshTempDir(t), u.HomeDir) {
		t.Error("homeUnder caught an ordinary temp directory — the predicate over-fires")
	}
}

// TestUndeclaredWideRootErr_OriginCarveOut pins the guard's decision table:
// refused for every weak origin, allowed for an explicit session_start (issue
// #182's contract), and never firing for an innocent directory.
func TestUndeclaredWideRootErr_OriginCarveOut(t *testing.T) {
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		t.Skipf("no user-database home: %v", err)
	}
	wide := filepath.Dir(u.HomeDir)
	if wide == "/" || wide == "." || wide == "" {
		t.Skipf("home %q sits at the filesystem root; no container to test", u.HomeDir)
	}

	for _, origin := range []sessionstate.PinSource{sessionstate.PinSourceRoots, sessionstate.PinSourceUnknown} {
		if err := undeclaredWideRootErr(wide, origin); err == nil {
			t.Errorf("undeclaredWideRootErr(%q, %s) = nil — a root containing the home directory %q must be refused without a declaration", wide, origin, u.HomeDir)
		}
	}
	if err := undeclaredWideRootErr(u.HomeDir, sessionstate.PinSourceRoots); err == nil {
		t.Errorf("undeclaredWideRootErr(%q, roots) = nil — the home directory itself must be refused without a declaration", u.HomeDir)
	}
	if err := undeclaredWideRootErr(wide, sessionstate.PinSourceSessionStart); err != nil {
		t.Errorf("undeclaredWideRootErr(%q, session_start) = %v — an explicit declaration must succeed even for a wide root (issue #182)", wide, err)
	}
	proj := freshTempDir(t)
	for _, origin := range []sessionstate.PinSource{sessionstate.PinSourceRoots, sessionstate.PinSourceUnknown, sessionstate.PinSourceSessionStart} {
		if err := undeclaredWideRootErr(proj, origin); err != nil {
			t.Errorf("undeclaredWideRootErr(%q, %s) = %v — an ordinary directory is never refused", proj, origin, err)
		}
	}
}

// TestAttachSynthetic_WideRootNeedsDeclaration exercises the guard through one
// of the three root-setting writers: an incidental (undeclared) seed at a
// directory containing the home directory must leave the session unattached.
func TestAttachSynthetic_WideRootNeedsDeclaration(t *testing.T) {
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		t.Skipf("no user-database home: %v", err)
	}
	wide := filepath.Dir(u.HomeDir)
	if wide == "/" || wide == "." || wide == "" {
		t.Skipf("home %q sits at the filesystem root; no container to test", u.HomeDir)
	}
	store, ss := newOriginStore(t)
	s := newPersistSession(t, store, ss, "proxyX")

	s.attachSynthetic(context.Background(), wide, sessionstate.PinSourceUnknown, pinTriggerLive)
	if got := s.workspace(); got != "" {
		t.Fatalf("attachSynthetic pinned %q without a declaration — a root containing the home directory must leave the session unattached (issue #306)", got)
	}
}

// TestRepin_WideRootFromRootsRefusedKeepsPin exercises the re-pin writer: a
// roots-driven re-pin to a wide root is refused and the existing pin survives.
func TestRepin_WideRootFromRootsRefusedKeepsPin(t *testing.T) {
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		t.Skipf("no user-database home: %v", err)
	}
	wide := filepath.Dir(u.HomeDir)
	if wide == "/" || wide == "." || wide == "" {
		t.Skipf("home %q sits at the filesystem root; no container to test", u.HomeDir)
	}
	store, ss := newOriginStore(t)
	rootA := freshTempDir(t)
	mustGitDir(t, rootA)
	s := newPersistSession(t, store, ss, "proxyX")
	s.attachWorkspace(context.Background(), "file://"+rootA)

	if _, err := s.repinWorkspaceFrom(context.Background(), wide, "", sessionstate.PinSourceRoots, pinTriggerLive, false); err == nil {
		t.Fatalf("a roots-driven re-pin to %q succeeded — a root containing the home directory must be refused without a declaration (issue #306)", wide)
	}
	if got := s.workspace(); got != rootA {
		t.Fatalf("the refused re-pin moved the pin anyway: got %q, want %q", got, rootA)
	}
}

// TestMaterialisePlumbDir_RefusesContainerOfHome is the containment half of
// the residue guard: an auto_attach_persist .plumb minted at a CONTAINER of
// the home directory would make Detect succeed there for every later session
// with no declaration — the same standing-capture shape the identity refusal
// blocks at $HOME itself.
func TestMaterialisePlumbDir_RefusesContainerOfHome(t *testing.T) {
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		t.Skipf("no user-database home: %v", err)
	}
	wide := filepath.Dir(u.HomeDir)
	if wide == "/" || wide == "." || wide == "" {
		t.Skipf("home %q sits at the filesystem root; no container to test", u.HomeDir)
	}
	if _, err := os.Stat(filepath.Join(wide, ".plumb")); err == nil {
		t.Skipf("%q already has a .plumb; refusing to test against it", wide)
	}

	if err := materialisePlumbDir(wide); err == nil {
		// A bug here mints a real marker at a real wide directory — undo it
		// before failing, or the test leaves the machine widened.
		_ = os.RemoveAll(filepath.Join(wide, ".plumb"))
		t.Fatalf("materialisePlumbDir(%q) succeeded — a container of the home directory must not become a workspace as a side effect (issue #306)", wide)
	} else if !strings.Contains(err.Error(), "must not become a plumb workspace") {
		// The refusal must be the GUARD's, not an incidental failure: on most
		// machines the user cannot write to a wide system directory, so a bare
		// EACCES would mask a missing guard while still returning an error —
		// exactly how this test passed green under mutation once.
		t.Fatalf("materialisePlumbDir(%q) failed, but not with the guard's refusal: %v", wide, err)
	}
	if _, err := os.Stat(filepath.Join(wide, ".plumb")); err == nil {
		t.Fatalf("materialisePlumbDir(%q) created the marker despite refusing", wide)
	}
}

// TestAttachWorkspace_HomeRootFromClientNeedsDeclaration pins the guard on the
// THIRD writer — the client roots/list attach path (attachWorkspacePinFrom) —
// which nothing else covered: deleting the guard there left the whole suite
// green (found by adversarial review). A client reporting $HOME as a root is
// not a declaration, even when $HOME carries a deliberate .plumb marker that
// lets Detect succeed there.
func TestAttachWorkspace_HomeRootFromClientNeedsDeclaration(t *testing.T) {
	home := freshTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	// A DELIBERATE marker at home — enough for Detect to succeed there, so the
	// attach reaches the root-setting writers at all.
	mustMkdir(t, filepath.Join(home, ".plumb"))
	mustWrite(t, filepath.Join(home, ".plumb", "context.md"), "# declared\n")

	store, ss := newOriginStore(t)
	s := newPersistSession(t, store, ss, "proxyX")
	s.attachWorkspace(context.Background(), "file://"+home)
	if got := s.workspace(); got != "" {
		t.Fatalf("attachWorkspace pinned %q from a client-reported root — a reported home directory is not a declaration (issue #306)", got)
	}
}

// TestContainment_KeysOnDBHomeNotEnvHOME pins the keying decision of
// userDBHome: containment refuses a root by the OS user-database home, NOT by
// the client-repointable $HOME. Flipping the key to os.UserHomeDir left every
// shipped test green on a machine where the two agree (found by adversarial
// review); this test makes them disagree. A hermetic sandbox's checkout
// CONTAINS its repointed $HOME and must stay pinnable by a weak origin — the
// exact shape that broke the guard's first placement in detect() — while the
// decoy home itself and the real home's container stay refused.
func TestContainment_KeysOnDBHomeNotEnvHOME(t *testing.T) {
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		t.Skipf("no user-database home: %v", err)
	}
	wide := filepath.Dir(u.HomeDir)
	if wide == "/" || wide == "." || wide == "" {
		t.Skipf("home %q sits at the filesystem root; no container to test", u.HomeDir)
	}

	candidate := freshTempDir(t) // the hermetic checkout
	decoyHome := filepath.Join(candidate, ".home")
	mustMkdir(t, decoyHome)
	t.Setenv("HOME", decoyHome)
	t.Setenv("USERPROFILE", decoyHome)

	if err := undeclaredWideRootErr(candidate, sessionstate.PinSourceUnknown); err != nil {
		t.Errorf("undeclaredWideRootErr(%q, unknown) = %v — a checkout containing only a REPOINTED $HOME must stay pinnable; containment keys on the user-database home (HOME=$PWD/.home sandboxes, Bazel execroots, nix-shell, CI images)", candidate, err)
	}
	if err := undeclaredWideRootErr(decoyHome, sessionstate.PinSourceUnknown); err == nil {
		t.Error("undeclaredWideRootErr(decoy $HOME, unknown) = nil — the repointed $HOME is still a home directory by identity and must be refused")
	}
	if err := undeclaredWideRootErr(wide, sessionstate.PinSourceUnknown); err == nil {
		t.Errorf("undeclaredWideRootErr(%q, unknown) = nil — repointing $HOME must not unguard the machine's real home container %q", wide, u.HomeDir)
	}
}

// TestPolicyRebuild_SwappedRootLosesBoundary is the TOCTOU regression found by
// adversarial review: the writers guard the directory at attach time, but the
// pinned STRING is re-canonicalised on every policy rebuild (the config poll
// alone runs every 30 seconds). Replacing the pinned directory with a symlink
// to a home-containing one — no race needed, only write access to its parent —
// must not widen the boundary on the next rebuild: the session loses its
// policy entirely (fail closed) unless the pin was explicitly declared.
func TestPolicyRebuild_SwappedRootLosesBoundary(t *testing.T) {
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		t.Skipf("no user-database home: %v", err)
	}
	wide := filepath.Dir(u.HomeDir)
	if wide == "/" || wide == "." || wide == "" {
		t.Skipf("home %q sits at the filesystem root; no container to test", u.HomeDir)
	}
	candidate := freshTempDir(t)

	store, ss := newOriginStore(t)
	s := newPersistSession(t, store, ss, "proxyX")
	s.attachSynthetic(context.Background(), candidate, sessionstate.PinSourceUnknown, pinTriggerLive)
	if got := s.workspace(); got != candidate {
		t.Fatalf("setup: workspace = %q, want %q", got, candidate)
	}
	if s.boundaryPolicy() == nil {
		t.Fatal("setup: the innocent candidate must have a policy before the swap")
	}

	// Swap the pinned directory for a symlink to a container of home.
	if err := os.Remove(candidate); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(wide, candidate); err != nil {
		t.Skipf("symlinks unsupported on this filesystem: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(candidate) })

	var rebuilt *tools.PathPolicy
	s.mutate(func(v *sessionView) {
		v.policy = s.buildPathPolicy(v)
		rebuilt = v.policy
	})
	if rebuilt != nil {
		t.Fatalf("a policy rebuild absorbed the swap: an undeclared session's boundary is now %q, which contains the home directory %q (issue #306)", wide, u.HomeDir)
	}

	// Control: the SAME root under a DECLARED pin keeps its boundary — the
	// issue #182 carve-out applies to rebuilds too.
	s.mutate(func(v *sessionView) {
		v.pinOrigin = sessionstate.PinSourceSessionStart
		v.policy = s.buildPathPolicy(v)
		rebuilt = v.policy
	})
	if rebuilt == nil {
		t.Fatal("a declared session_start pin must keep its boundary even when the root contains a home directory (issue #182)")
	}
}
