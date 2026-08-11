package cli

// conn_canonicalroot_test.go — the workspace root is stored canonicalised
// (issue #263).
//
// Roots used to be stored exactly as reported, so two sessions on ONE project
// reached by different path spellings — the everyday macOS /tmp → /private/tmp
// firmlink, a checkout under a symlinked parent, a $TMPDIR scratch project —
// disagreed about where they were. The visible damage was in the mailbox:
// leave_note compared the two roots textually, concluded the peer sitting in the
// same directory was in another project, routed the message to the daemon-level
// cross-project store, and delivery dropped it unread because cross_project is
// off by default. The sender was then told the recipient "is pinned to
// /private/tmp/myproj" — about a peer in the same folder.
//
// These tests work on a deliberately built alias rather than the platform's own,
// so they mean the same thing on Linux (where /tmp is a real directory) as on macOS.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
)

// aliasedWorkspace builds one project directory reachable by two spellings and
// returns them: the resolved path, and an equivalent path that traverses a symlink.
// The project carries a .git marker so Detect resolves it as a root with no
// language server.
func aliasedWorkspace(t *testing.T) (realRoot, alias string) {
	t.Helper()
	base := freshTempDir(t)
	realRoot = filepath.Join(base, "real", "proj")
	mustMkdir(t, realRoot)
	mustGitDir(t, realRoot)
	if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "alias")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	alias = filepath.Join(base, "alias", "proj")

	// The fixture is only meaningful if the two spellings really are the textual
	// mismatch the bug needs. A lexical clean — what sameWorkspace, NotifyKey and
	// the peer caches all use — must consider them different places.
	if filepath.Clean(realRoot) == filepath.Clean(alias) {
		t.Fatalf("fixture is not aliased: both spellings clean to %q", realRoot)
	}
	return realRoot, alias
}

// TestCanonicalRoot_AliasedAttachPinsTheRealRoot is the base case: whichever
// spelling the client reports, the pin holds the resolved one.
func TestCanonicalRoot_AliasedAttachPinsTheRealRoot(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	realRoot, alias := aliasedWorkspace(t)

	s := newConnSession(context.Background(), detectTestPool(), nil, config.NewStore(config.Defaults()), nil, nil, newSharedBudgets())
	defer s.close()
	s.attachWorkspace(context.Background(), "file://"+alias)

	if got := s.workspace(); got != realRoot {
		t.Errorf("pin = %q, want the resolved root %q", got, realRoot)
	}
	if got := sessionFolder(t, s.sessID); got != realRoot {
		t.Errorf("session registry Folder = %q, want %q — the registry and the pin are "+
			"compared against each other, so both must be canonicalised or the mismatch just moves", got, realRoot)
	}
}

// TestCanonicalRoot_TwoSessionsOneProjectAgree is issue #263 itself. Two
// connections attach to the same directory by different spellings; every
// same-project test downstream is a comparison of these two values, so they must
// be one string.
func TestCanonicalRoot_TwoSessionsOneProjectAgree(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	realRoot, alias := aliasedWorkspace(t)
	store := config.NewStore(config.Defaults())

	a := newConnSession(context.Background(), detectTestPool(), nil, store, nil, nil, newSharedBudgets())
	defer a.close()
	a.attachWorkspace(context.Background(), "file://"+realRoot)

	b := newConnSession(context.Background(), detectTestPool(), nil, store, nil, nil, newSharedBudgets())
	defer b.close()
	b.attachWorkspace(context.Background(), "file://"+alias)

	if a.workspace() != b.workspace() {
		t.Fatalf("two sessions in ONE project disagree about where they are: %q vs %q — "+
			"leave_note routes this as cross-project and delivery silently drops it",
			a.workspace(), b.workspace())
	}
	// What the mailbox actually compares: the sender's own root against the peer's
	// registered Folder.
	if got := sessionFolder(t, b.sessID); got != a.workspace() {
		t.Errorf("peer Folder %q != sender root %q", got, a.workspace())
	}
}

// TestCanonicalRoot_AliasedRepinIsANoOpNotAStickyRefusal is the second
// user-visible consequence. The sticky-pin guard (issue #182) refuses a live
// session_start that moves an explicit pin to a DIFFERENT root. With roots stored
// verbatim, naming the project you are already pinned to by its other spelling
// looked like a different root — so a legitimate, redundant session_start was
// refused as a peer trying to steal the pin, and the caller was told to retry
// with force: true to "switch projects" it was never leaving.
func TestCanonicalRoot_AliasedRepinIsANoOpNotAStickyRefusal(t *testing.T) {
	store, ss := newOriginStore(t)
	realRoot, alias := aliasedWorkspace(t)

	s := newPersistSession(t, store, ss, "proxyX")
	defer s.close()
	if _, err := s.repinWorkspace(context.Background(), realRoot, "", false); err != nil {
		t.Fatalf("first explicit pin: %v", err)
	}

	root, err := s.repinWorkspace(context.Background(), alias, "", false)
	if err != nil {
		t.Fatalf("re-pinning to the SAME project by its other spelling must be a no-op, "+
			"not a sticky-pin refusal: %v", err)
	}
	if root != realRoot {
		t.Errorf("resolved root = %q, want %q", root, realRoot)
	}
	if got := s.workspace(); got != realRoot {
		t.Errorf("pin = %q, want %q", got, realRoot)
	}
}

// TestCanonicalRoot_SyntheticRootIsCanonicalised covers the other root-producing
// path: a MARKERLESS folder, which Detect refuses and SynthesiseRoot turns into
// its own workspace. Issue #182's contract is explicit that an explicit pin must
// not behave differently for want of a .git or language marker, so this root
// needs the same treatment. Driven through repinWorkspace rather than
// attachSynthetic directly, so it exercises the path production actually takes.
func TestCanonicalRoot_SyntheticRootIsCanonicalised(t *testing.T) {
	store, ss := newOriginStore(t)
	base := freshTempDir(t)
	realRoot := filepath.Join(base, "real", "bare")
	mustMkdir(t, realRoot)
	if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "alias")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	alias := filepath.Join(base, "alias", "bare")

	s := newPersistSession(t, store, ss, "proxySynth")
	defer s.close()
	root, err := s.repinWorkspace(context.Background(), alias, "", false)
	if err != nil {
		t.Fatalf("explicit pin on a markerless folder: %v", err)
	}
	if root != realRoot {
		t.Errorf("resolved synthetic root = %q, want %q", root, realRoot)
	}
	if got := s.workspace(); got != realRoot {
		t.Errorf("synthetic pin = %q, want the resolved root %q", got, realRoot)
	}
}

// TestCanonicalRoot_DetectAgreesAcrossSpellings states the invariant directly at
// the layer that owns it: the pool answers "which project is this?" with one
// spelling, whichever way it was asked.
func TestCanonicalRoot_DetectAgreesAcrossSpellings(t *testing.T) {
	realRoot, alias := aliasedWorkspace(t)
	pool := detectTestPool()

	viaReal, _, err := pool.Detect(realRoot)
	if err != nil {
		t.Fatalf("Detect(real): %v", err)
	}
	viaAlias, _, err := pool.Detect(alias)
	if err != nil {
		t.Fatalf("Detect(alias): %v", err)
	}
	if viaReal != viaAlias {
		t.Fatalf("Detect disagrees across spellings of one project: %q vs %q", viaReal, viaAlias)
	}
	if viaAlias != realRoot {
		t.Errorf("Detect = %q, want %q", viaAlias, realRoot)
	}
}

// TestCanonicalRoot_AliasedURIRoutesToThePrimaryServer is the regression guard
// for the trap in this fix. routingProxy.route compares the root it detects for a
// file against the registered primaryRoot and, on a miss, acquires a SEPARATE
// language server for that root with pin=false — an entry the refcount path never
// reclaims, so it lives until the daemon exits. Canonicalising the session pin
// ALONE would therefore have traded a silently-dropped message for a permanently
// duplicated gopls, every time a client named a file in the project by the
// spelling it knows. Both sides have to come from the pool.
func TestCanonicalRoot_AliasedURIRoutesToThePrimaryServer(t *testing.T) {
	realRoot, alias := aliasedWorkspace(t)
	mustWrite(t, filepath.Join(realRoot, "go.mod"), "module test\n")

	pool := newTestPool()
	primary := &stubClient{id: "primary"}
	installEntry(pool, realRoot, primary)
	rp := newRoutingProxy(pool)
	rp.setPrimary(realRoot, "go", pool.entries[poolKey{realRoot, "go"}].proxy)

	// The client names a file in the pinned project by the ALIASED spelling.
	got, err := rp.route(context.Background(), "file://"+filepath.Join(alias, "main.go"), false)
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if got != primary {
		t.Fatal("an aliased URI inside the pinned project routed away from the primary server; " +
			"it would acquire a second, never-reclaimed language server for the same project")
	}
}

// TestCanonicalRoot_HintRelPathAcceptsAnAliasedPath is the regression guard for
// the trap in canonicalising the root, found by adversarial review after the
// first version of this change was already "done".
//
// Canonicalising the root fixes the consumers fed BY the pool. It does nothing
// for the consumers that compare the root against a path the AGENT supplies —
// and it makes those deterministically wrong, because one side is now always
// resolved while the other is whatever spelling the client knows. hintRelPath is
// the worst of them: filepath.Rel returns a "../…" escape, hintRelPath returns
// "", and memory hint injection stops firing for the whole project. No error, no
// log — the feature just goes quiet. Same shape in relevant_memories (which then
// contradicts the boundary guard that just admitted the path), episodicRelPath,
// peerArea and workspace_sessions.
func TestCanonicalRoot_HintRelPathAcceptsAnAliasedPath(t *testing.T) {
	realRoot, alias := aliasedWorkspace(t)
	mustMkdir(t, filepath.Join(realRoot, "internal", "auth"))

	args := []byte(`{"file_path":"` + filepath.Join(alias, "internal", "auth", "a.go") + `"}`)
	if got := hintRelPath(realRoot, args); got != "internal/auth/a.go" {
		t.Fatalf("hintRelPath = %q, want %q — a file named by the project's other "+
			"spelling must still match path-scoped memories", got, "internal/auth/a.go")
	}

	// The widening must not admit a path that is genuinely elsewhere.
	outside := []byte(`{"file_path":"` + filepath.Join(freshTempDir(t), "x.go") + `"}`)
	if got := hintRelPath(realRoot, outside); got != "" {
		t.Errorf("a path outside the workspace must not hint; got %q", got)
	}
}

// TestCanonicalRoot_NonexistentRootStillPins is the fail-soft half of the
// contract. Canonicalisation is best effort: a root that does not exist must
// still pin, and pin to the spelling the caller named, because a session that
// refuses to attach is strictly worse than one attached un-canonicalised. Driven
// through repinWorkspace so the pool — which is what canonicalises — is in the
// loop; calling attachSynthetic directly would assert nothing.
func TestCanonicalRoot_NonexistentRootStillPins(t *testing.T) {
	store, ss := newOriginStore(t)
	missing := filepath.Join(freshTempDir(t), "never", "created")

	s := newPersistSession(t, store, ss, "proxyMissing")
	defer s.close()
	root, err := s.repinWorkspace(context.Background(), missing, "", false)
	if err != nil {
		t.Fatalf("a nonexistent root must still pin: %v", err)
	}
	if root != missing {
		t.Errorf("resolved root = %q, want %q — an unresolvable tail must come back unchanged", root, missing)
	}
	if got := s.workspace(); got != missing {
		t.Errorf("pin = %q, want %q", got, missing)
	}
}
