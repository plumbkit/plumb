package cli

// conn_homepin_test.go — the daemon attach paths that could name the user's
// home directory as the workspace root. SynthesiseRoot's $HOME-seed exemption
// is keyed on the pin's ORIGIN, and these tests hold each caller to it: only a
// session_start declaration (live or replayed) may pin $HOME; an incidental
// tool path, a client-reported root, and a persisted pin from a weaker source
// must all be refused. The function-level guard is pool_synthesise_root_test.go;
// this file proves the origins are wired through to it.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/sessionstate"
)

// autoAttachSession builds an unattached session with [workspace] auto_attach
// enabled — the synthetic-root fallback under test.
func autoAttachSession(t *testing.T) *connSession {
	t.Helper()
	cfg := config.Defaults()
	cfg.Workspace.AutoAttach = true
	s := newConnSession(context.Background(), detectTestPool(), nil, config.NewStore(cfg), nil, nil, newSharedBudgets())
	t.Cleanup(s.close)
	return s
}

// dotfilesHome builds the dangerous fixture: a fake $HOME carrying a .git —
// the dotfiles-repo shape that makes every markerless path beneath it
// synthesise upward. Returns the canonical home path.
func dotfilesHome(t *testing.T) string {
	t.Helper()
	home := freshTempDir(t) // not t.TempDir: see the GOTMPDIR note in pool_synthesise_root_test.go
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	mustMkdir(t, filepath.Join(home, ".git"))
	return home
}

// TestOnBeforeTool_IncidentalHomeSeedDoesNotAttach is finding B1 on PR #288:
// the $HOME-as-seed exemption was keyed on the seed STRING being $HOME, but
// onBeforeTool seeds from seedPathFromArgs (uri / file_path / path), so with
// auto_attach enabled a single read_file of ~/.zshrc pinned the entire home
// directory. Reading a dotfile is not a declaration of intent.
func TestOnBeforeTool_IncidentalHomeSeedDoesNotAttach(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	home := dotfilesHome(t)
	mkTestFile(t, filepath.Join(home, ".zshrc"), "export EDITOR=vi\n")
	s := autoAttachSession(t)

	s.onBeforeTool(context.Background(), "read_file", json.RawMessage(`{"file_path":"`+filepath.Join(home, ".zshrc")+`"}`))

	if got := s.workspace(); got != "" {
		t.Fatalf("reading ~/.zshrc attached the session to %q; an incidental tool path must never pin the home directory", got)
	}
}

// TestOnBeforeTool_WorkspaceArgDoesNotLaunderAnIncidentalSeed is the round-3
// finding, and the same class one level up from the test above.
//
// The exemption was keyed on `workspaceArgPresent(args)` — "is a workspace key
// present anywhere in this call" — while the SEED comes from seedPathFromArgs,
// which prefers uri/file_path/path/root OVER workspace. Tools take both:
// relevant_memories and write_memory each accept a path AND a workspace. So a
// call naming one directory as its workspace and touching a file in $HOME
// seeded $HOME while counting as a deliberate declaration of it.
//
// The pin is stamped session_start and persisted, so it also survived every
// later reconnect through the rehydrate guard — in the DEFAULT configuration,
// escalating past auto_attach entirely.
func TestOnBeforeTool_WorkspaceArgDoesNotLaunderAnIncidentalSeed(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	home := dotfilesHome(t)
	mkTestFile(t, filepath.Join(home, ".zshrc"), "export EDITOR=vi\n")
	elsewhere := freshTempDir(t)
	s := autoAttachSession(t)

	// The shape of relevant_memories / write_memory: a path AND a workspace,
	// naming different directories. The workspace named is NOT the home
	// directory, so nothing here declares the home directory to be anything.
	s.onBeforeTool(context.Background(), "relevant_memories",
		json.RawMessage(`{"path":"`+filepath.Join(home, ".zshrc")+`","workspace":"`+elsewhere+`"}`))

	if got := s.workspace(); got == home {
		t.Fatalf("a workspace key naming %q laundered an incidental $HOME path into a pin of %q; "+
			"the declaration has to be about the directory being named", elsewhere, got)
	}
}

// TestOnBeforeTool_WideRootWithAMarkerIsNotPinned is round 5's finding, held
// today by the containment choke at the root writers (undeclaredWideRootErr).
//
// SynthesiseRoot is reached only on Detect's FAILURE branch. When the wide
// directory carries a marker Detect SUCCEEDS, and the success path never
// consulted any containment guard — so a find_files or search_in_files naming
// such a directory (both take a DIRECTORY as `path`) pinned it. That path is
// not behind auto_attach, so it was reachable in the default configuration; and
// plumb could mint the marker itself, because materialisePlumbDir guarded
// identity only. Round 5 refused this inside detect(); round 6 showed that
// shape breaks legitimate repos containing a sandbox home directory, so the
// refusal now lives where the pin is written, gated on the pin's origin —
// detection succeeds here, and the PIN is what gets refused.
func TestOnBeforeTool_WideRootWithAMarkerIsNotPinned(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	base := freshTempDir(t) // stands in for /Users
	home := filepath.Join(base, "home")
	mustMkdir(t, home)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	mustMkdir(t, filepath.Join(base, ".git")) // the marker that made Detect succeed

	s := autoAttachSession(t)
	s.onBeforeTool(context.Background(), "find_files", json.RawMessage(`{"path":"`+base+`"}`))

	if got := s.workspace(); got != "" {
		t.Fatalf("find_files({path: %q}) pinned %q, which contains the home directory %q; "+
			"a marker must not make a wide directory a workspace", base, got, home)
	}

	// Control: an ordinary marked project under the home directory still attaches,
	// which is where most projects live — so the refusal is about containment and
	// not about markers.
	proj := filepath.Join(home, "proj")
	mustMkdir(t, filepath.Join(proj, ".git"))
	s2 := autoAttachSession(t)
	s2.onBeforeTool(context.Background(), "find_files", json.RawMessage(`{"path":"`+proj+`"}`))
	if got := s2.workspace(); got == "" {
		t.Errorf("control failed — an ordinary project at %q did not attach", proj)
	}
}

// TestOnBeforeTool_LaunderedSeedIsNotStampedExplicit covers the OTHER consumer
// of `explicit`, which round 5 found unpinned: reverting it alone was green
// across the whole repository.
//
// `explicit` decides the pin ORIGIN as well as the SynthesiseRoot argument. When
// it was `workspaceArgPresent`, `{path: X, workspace: Y}` pinned X — a directory
// the caller never named — and stamped it PinSourceSessionStart, making it
// sticky and persisted. The caller's genuine session_start was then refused by
// the issue-#182 sticky guard until retried with force, and the previously
// chosen workspace was never restored.
//
// Asserted on the ORIGIN, not just on which directory was pinned: the other test
// exercises this boolean through the home directory only, so the origin half had
// no coverage at all.
func TestOnBeforeTool_LaunderedSeedIsNotStampedExplicit(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	victim := freshTempDir(t)
	mustMkdir(t, filepath.Join(victim, ".git"))
	mkTestFile(t, filepath.Join(victim, "a.go"), "package a\n")
	declared := freshTempDir(t)
	mustMkdir(t, filepath.Join(declared, ".git"))
	s := autoAttachSession(t)

	s.onBeforeTool(context.Background(), "relevant_memories",
		json.RawMessage(`{"path":"`+filepath.Join(victim, "a.go")+`","workspace":"`+declared+`"}`))

	var origin string
	s.mutate(func(v *sessionView) { origin = v.pinVia })
	if origin == string(sessionstate.PinSourceSessionStart) {
		t.Errorf("a pin seeded from an incidental path was stamped %q — sticky and persisted — "+
			"while the caller declared %q; only a seed that IS the workspace argument is explicit",
			origin, declared)
	}
}

// TestOnBeforeTool_ExplicitHomeWorkspaceStillAttaches is the issue #182
// control: session_start({workspace: "<home>"}) is a genuine declaration and
// must still succeed — only non-explicit seeds are refused.
func TestOnBeforeTool_ExplicitHomeWorkspaceStillAttaches(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	home := dotfilesHome(t)
	s := autoAttachSession(t)

	s.onBeforeTool(context.Background(), "session_start", json.RawMessage(`{"workspace":"`+home+`"}`))

	if got := s.workspace(); got != home {
		t.Fatalf("explicit session_start workspace=%q attached %q; an explicit pin must always succeed", home, got)
	}
}

// TestOnBeforeTool_ProjectUnderDotfilesHomeStillAttaches is the everyday
// control: refusing $HOME must not break auto-attach for the directories
// UNDER it, which is where most projects live. A markerless scratch dir below
// a dotfiles home synthesises to itself (the walk stops at $HOME).
func TestOnBeforeTool_ProjectUnderDotfilesHomeStillAttaches(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	home := dotfilesHome(t)
	scratch := filepath.Join(home, "scratch", "notes")
	mustMkdir(t, scratch)
	src := filepath.Join(scratch, "todo.txt")
	mkTestFile(t, src, "milk\n")
	s := autoAttachSession(t)

	s.onBeforeTool(context.Background(), "read_file", json.RawMessage(`{"file_path":"`+src+`"}`))

	if got := s.workspace(); got != scratch {
		t.Fatalf("markerless dir under a dotfiles home attached %q, want %q — the guard must stop the ascent, not the attach", got, scratch)
	}
}

// TestRehydratePin_HomePinNeedsSessionStartOrigin is finding B3 on PR #288: a
// persisted pin naming $HOME replayed through the guard, because on the replay
// the SEED is $HOME and the string-keyed exemption honoured it — default
// config, no auto_attach, no residue needed. The exemption is now keyed on the
// STORED origin: a row an earlier build persisted from a weaker source (client
// roots) is refused, while a row the caller genuinely created with
// session_start({workspace: "~"}) still restores (issue #182).
func TestRehydratePin_HomePinNeedsSessionStartOrigin(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	home := dotfilesHome(t)
	store := config.NewStore(config.Defaults())
	ss, err := sessionstate.Open()
	if err != nil {
		t.Fatalf("sessionstate.Open: %v", err)
	}
	defer ss.Close()

	// A $HOME pin persisted from client roots (an earlier build allowed this).
	if err := ss.UpsertPin("proxyRoots", home, LanguageNone, sessionstate.PinSourceRoots); err != nil {
		t.Fatalf("UpsertPin: %v", err)
	}
	weak := newPersistSession(t, store, ss, "proxyRoots")
	weak.rehydratePin(context.Background())
	if got := weak.workspace(); got != "" {
		t.Fatalf("a roots-origin $HOME pin was restored to %q; replay must not launder a weak origin into the home directory", got)
	}

	// The control: a genuine session_start pin at $HOME restores (issue #182).
	if err := ss.UpsertPin("proxyExplicit", home, LanguageNone, sessionstate.PinSourceSessionStart); err != nil {
		t.Fatalf("UpsertPin: %v", err)
	}
	explicit := newPersistSession(t, store, ss, "proxyExplicit")
	explicit.rehydratePin(context.Background())
	if got := explicit.workspace(); got != home {
		t.Fatalf("a session_start-origin $HOME pin restored %q, want %q — the explicit contract must survive the guard", got, home)
	}
}

// TestAttachWorkspace_ClientRootContainingHomeRefused holds the containment
// choke on the CLIENT-ROOTS attach route: attachWorkspace (roots/list at
// initialize, and onRootsChanged's first attach) records PinSourceRoots, which
// is a report of where the client happens to be, not a declaration — so a
// client reporting a marker-carrying directory that contains the home
// directory must not pin it. Detect SUCCEEDS on such a directory (the marker
// wins; detection is identity-guarded only), which is exactly why the refusal
// has to sit on the pin write.
func TestAttachWorkspace_ClientRootContainingHomeRefused(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	base := freshTempDir(t) // stands in for /Users
	home := filepath.Join(base, "home")
	mustMkdir(t, home)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	mustMkdir(t, filepath.Join(base, ".git")) // Detect succeeds here

	s := autoAttachSession(t)
	s.attachWorkspace(context.Background(), "file://"+base)
	if got := s.workspace(); got != "" {
		t.Fatalf("a client-reported root %q was pinned; it contains the home directory %q and nothing declared it", got, home)
	}

	// Control: an ordinary client-reported project still attaches through the
	// same route, so the choke is about containment, not about roots attaches.
	proj := filepath.Join(home, "proj")
	mustMkdir(t, filepath.Join(proj, ".git"))
	s2 := autoAttachSession(t)
	s2.attachWorkspace(context.Background(), "file://"+proj)
	if got := s2.workspace(); got != proj {
		t.Errorf("control failed — client-reported project attached %q, want %q", got, proj)
	}
}

// TestAttachSynthetic_WideRootRefusedAtTheWriter is the belt at the second
// v.acquiredRoot writer. In the current call graph SynthesiseRoot already
// returns "" for a non-explicit wide seed before attachSynthetic is reached,
// so this exercises the direct call — the invariant lives at the writers so a
// new or changed caller cannot reopen the hole by skipping a courtesy check
// upstream, and this test is what notices if the writer's own guard is
// removed on the strength of that upstream check.
func TestAttachSynthetic_WideRootRefusedAtTheWriter(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	base := freshTempDir(t)
	home := filepath.Join(base, "home")
	mustMkdir(t, home)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	s := autoAttachSession(t)
	s.attachSynthetic(context.Background(), base, sessionstate.PinSourceRoots, pinTriggerLive)
	if got := s.workspace(); got != "" {
		t.Fatalf("attachSynthetic pinned %q, which contains the home directory %q, from a non-explicit origin", got, home)
	}

	// The explicit origin still lands — issue #182's contract, at this writer.
	s.attachSynthetic(context.Background(), base, sessionstate.PinSourceSessionStart, pinTriggerLive)
	if got := s.workspace(); got != base {
		t.Errorf("an explicit synthetic attach of %q did not land (got %q)", base, got)
	}
}

// TestRehydratePin_WideRootPinNeedsSessionStartOrigin extends the $HOME replay
// rule one rung up, through Detect's SUCCESS branch: a persisted pin naming a
// marker-carrying directory that CONTAINS the home directory resolves through
// attachWorkspacePinFrom (not the synthetic fallback), so the choke must hold
// on the restore trigger too — a row an earlier build persisted from client
// roots cannot ride the replay back into a whole-/Users workspace, while a
// genuine session_start row still restores (issue #182).
func TestRehydratePin_WideRootPinNeedsSessionStartOrigin(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	base := freshTempDir(t)
	home := filepath.Join(base, "home")
	mustMkdir(t, home)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	mustMkdir(t, filepath.Join(base, ".git")) // Detect succeeds at the wide root
	store := config.NewStore(config.Defaults())
	ss, err := sessionstate.Open()
	if err != nil {
		t.Fatalf("sessionstate.Open: %v", err)
	}
	defer ss.Close()

	if err := ss.UpsertPin("proxyWideRoots", base, LanguageNone, sessionstate.PinSourceRoots); err != nil {
		t.Fatalf("UpsertPin: %v", err)
	}
	weak := newPersistSession(t, store, ss, "proxyWideRoots")
	weak.rehydratePin(context.Background())
	if got := weak.workspace(); got != "" {
		t.Fatalf("a roots-origin pin of %q was restored to %q; it contains the home directory %q", base, got, home)
	}

	if err := ss.UpsertPin("proxyWideExplicit", base, LanguageNone, sessionstate.PinSourceSessionStart); err != nil {
		t.Fatalf("UpsertPin: %v", err)
	}
	explicit := newPersistSession(t, store, ss, "proxyWideExplicit")
	explicit.rehydratePin(context.Background())
	if got := explicit.workspace(); got != base {
		t.Fatalf("a session_start-origin pin of %q restored %q; the explicit contract must survive the choke", base, got)
	}
}

// TestRepin_SandboxHomeInsideRepoKeepsDetectionAndLanguage is round 6's
// finding B3, the regression that forced containment out of detect(): a repo
// whose $HOME is one of its own subdirectories (hermetic build sandboxes with
// HOME=$PWD/.home, Bazel execroots, nix-shell, CI images repointing HOME
// inside the checkout) is an ordinary project, and an explicit pin of it must
// resolve through Detect — real root, real language — not fall through to a
// synthetic LanguageNone root with no LSP and nothing naming containment as
// the cause.
//
// The language server here is a warming stub (sleepCommand), so the assertion
// is on the language the session ACQUIRED, not on a display label. The
// non-explicit counterpart is refused by the choke — such a repo does contain
// a home directory, so auto-attaching it still demands a declaration; the
// error says so instead of silently degrading.
func TestRepin_SandboxHomeInsideRepoKeepsDetectionAndLanguage(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	repo := freshTempDir(t)
	mustWrite(t, filepath.Join(repo, "go.mod"), "module sandbox\n")
	mustMkdir(t, filepath.Join(repo, ".git"))
	sandboxHome := filepath.Join(repo, ".home")
	mustMkdir(t, sandboxHome)
	t.Setenv("HOME", sandboxHome)
	t.Setenv("USERPROFILE", sandboxHome)

	cmd, args := sleepCommand(t)
	pool := warmingPool(context.Background(), cmd, args)
	pool.startGrace = time.Millisecond
	defer pool.close()
	s := newConnSession(context.Background(), pool, nil, config.NewStore(config.Defaults()), nil, nil, newSharedBudgets())
	defer s.close()

	root, err := s.repinWorkspaceFrom(context.Background(), repo, "", sessionstate.PinSourceSessionStart, pinTriggerLive, false)
	if err != nil {
		t.Fatalf("explicit pin of a repo containing its sandbox $HOME was refused: %v", err)
	}
	if root != repo {
		t.Fatalf("explicit pin resolved %q, want the repo %q", root, repo)
	}
	var lang string
	s.mutate(func(v *sessionView) { lang = v.acquiredLanguage })
	if lang != "go" {
		t.Errorf("acquired language = %q, want %q — the pin must keep Detect's real language, not degrade to a synthetic root", lang, "go")
	}

	// The non-explicit origin is refused BY THE CHOKE, with containment named:
	// the repo really does contain a home directory, so only a declaration pins it.
	s2 := newConnSession(context.Background(), pool, nil, config.NewStore(config.Defaults()), nil, nil, newSharedBudgets())
	defer s2.close()
	if _, err := s2.repinWorkspaceFrom(context.Background(), repo, "", sessionstate.PinSourceRoots, pinTriggerLive, false); err == nil {
		t.Error("a non-explicit pin of a repo containing a home directory was accepted; want the containment refusal")
	}
}

// TestRepin_RootsDrivenHomeRepinRefused: onRootsChanged re-pins through
// repinWorkspaceFrom with PinSourceRoots — a client that starts reporting
// $HOME as its root is not a session_start declaration, so the synthetic
// fallback refuses it with an error instead of pinning the home directory.
func TestRepin_RootsDrivenHomeRepinRefused(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	home := dotfilesHome(t)
	s := autoAttachSession(t)

	if _, err := s.repinWorkspaceFrom(context.Background(), home, "", sessionstate.PinSourceRoots, pinTriggerLive, false); err == nil {
		t.Fatal("a roots-origin re-pin to $HOME succeeded; want a refusal")
	}
	if got := s.workspace(); got != "" {
		t.Fatalf("the refused re-pin still attached %q", got)
	}
}
