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
