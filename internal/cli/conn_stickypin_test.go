package cli

// conn_stickypin_test.go — the sticky-pin guard for issue #182.
//
// A client that multiplexes several logical agent sessions over one plumb serve
// connection can have a peer's session_start silently re-pin the shared
// connection; the victim's relative-path calls then join onto the wrong
// project. Once a pin was set by an explicit session_start (live or restored),
// a conflicting live re-pin must be refused unless forced, and a roots-driven
// re-pin must not override it. Pins held via client roots or incidental
// auto-attach are NOT sticky — the first explicit pin must always land.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/sessionstate"
	"github.com/plumbkit/plumb/internal/tools"
)

func TestStickyPin_ConflictingSessionStartRepinRefused(t *testing.T) {
	store, ss := newOriginStore(t)
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)

	s := newPersistSession(t, store, ss, "proxyX")
	defer s.close()
	if _, err := s.repinWorkspace(context.Background(), rootA, "", false); err != nil {
		t.Fatalf("first explicit pin: %v", err)
	}

	_, err := s.repinWorkspace(context.Background(), rootB, "", false)
	if err == nil {
		t.Fatal("a conflicting live session_start re-pin must be refused while an explicit pin holds")
	}
	for _, want := range []string{rootA, rootB, "force: true", "issue #182"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal error should mention %q\n got: %v", want, err)
		}
	}
	if got := s.workspace(); got != rootA {
		t.Fatalf("the refused re-pin still moved the pin: got %q, want %q", got, rootA)
	}
}

func TestStickyPin_ForceOverrides(t *testing.T) {
	store, ss := newOriginStore(t)
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)

	s := newPersistSession(t, store, ss, "proxyX")
	defer s.close()
	if _, err := s.repinWorkspace(context.Background(), rootA, "", false); err != nil {
		t.Fatalf("first explicit pin: %v", err)
	}

	root, err := s.repinWorkspace(context.Background(), rootB, "", true)
	if err != nil {
		t.Fatalf("force: true must override the sticky-pin guard: %v", err)
	}
	if root != rootB || s.workspace() != rootB {
		t.Fatalf("forced re-pin: resolved %q, workspace %q; want %q", root, s.workspace(), rootB)
	}
}

func TestStickyPin_RestoredPinIsSticky(t *testing.T) {
	// A pin replayed after a daemon restart (pinTriggerRestore) is the same
	// deliberate choice — it must be just as sticky as a live one.
	store, ss := newOriginStore(t)
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)

	before := newPersistSession(t, store, ss, "proxyX")
	if _, err := before.repinWorkspace(context.Background(), rootA, "", false); err != nil {
		t.Fatalf("repinWorkspace: %v", err)
	}
	before.close()

	after := newPersistSession(t, store, ss, "proxyX")
	defer after.close()
	after.rehydratePin(context.Background())
	if got := after.workspace(); got != rootA {
		t.Fatalf("precondition: restored pin = %q, want %q", got, rootA)
	}

	if _, err := after.repinWorkspace(context.Background(), rootB, "", false); err == nil {
		t.Fatal("a re-pin against a restored explicit pin must be refused")
	}
	if got := after.workspace(); got != rootA {
		t.Fatalf("the refused re-pin still moved the pin: got %q, want %q", got, rootA)
	}
}

func TestStickyPin_RestoreReplayNeverBlocked(t *testing.T) {
	// The restore path itself passes force=false: replaying a persisted B pin
	// onto a connection holding an explicit A pin must succeed — that replay IS
	// the pin's owner re-attaching, not a peer stealing it.
	store, ss := newOriginStore(t)
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)

	s := newPersistSession(t, store, ss, "proxyX")
	defer s.close()
	if _, err := s.repinWorkspace(context.Background(), rootA, "", false); err != nil {
		t.Fatalf("first explicit pin: %v", err)
	}
	if _, err := s.repinWorkspaceFrom(context.Background(), rootB, "", sessionstate.PinSourceSessionStart, pinTriggerRestore, false); err != nil {
		t.Fatalf("restore-trigger replay must bypass the guard: %v", err)
	}
	if got := s.workspace(); got != rootB {
		t.Fatalf("restore did not attach: got %q, want %q", got, rootB)
	}
}

func TestStickyPin_RootsOriginPinNotSticky(t *testing.T) {
	// A pin attached from client roots is the client's claim, not the caller's:
	// the first explicit session_start must always be able to replace it.
	store, ss := newOriginStore(t)
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)

	s := newPersistSession(t, store, ss, "proxyX")
	defer s.close()
	s.attachWorkspace(context.Background(), "file://"+rootA) // origin roots

	if _, err := s.repinWorkspace(context.Background(), rootB, "", false); err != nil {
		t.Fatalf("a roots-held pin must not block the first explicit pin: %v", err)
	}
	if got := s.workspace(); got != rootB {
		t.Fatalf("explicit re-pin did not land: got %q, want %q", got, rootB)
	}
}

func TestStickyPin_IncidentalAutoAttachNotSticky(t *testing.T) {
	store, ss := newOriginStore(t)
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)

	s := newPersistSession(t, store, ss, "proxyX")
	defer s.close()
	s.attachWorkspacePin(context.Background(), "file://"+rootA, sessionstate.PinSourceUnknown)

	if _, err := s.repinWorkspace(context.Background(), rootB, "", false); err != nil {
		t.Fatalf("an incidental auto-attach must not block the first explicit pin: %v", err)
	}
}

func TestStickyPin_SameRootAndSubdirNotRefused(t *testing.T) {
	// The guard compares RESOLVED roots: re-pinning to the same root, or to a
	// subdirectory that Detect resolves up to it, is not a project switch and
	// must never be refused.
	store, ss := newOriginStore(t)
	root := freshTempDir(t)
	mustGitDir(t, root)
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	s := newPersistSession(t, store, ss, "proxyX")
	defer s.close()
	if _, err := s.repinWorkspace(context.Background(), root, "", false); err != nil {
		t.Fatalf("first explicit pin: %v", err)
	}
	if _, err := s.repinWorkspace(context.Background(), root, "", false); err != nil {
		t.Fatalf("same-root re-pin refused: %v", err)
	}
	resolved, err := s.repinWorkspace(context.Background(), sub, "", false)
	if err != nil {
		t.Fatalf("subdir re-pin refused: %v", err)
	}
	if resolved != root {
		t.Fatalf("subdir resolved to %q, want the enclosing root %q", resolved, root)
	}
}

func TestStickyPin_RootsChangeKeepsExplicitPin(t *testing.T) {
	// The roots vector of the same drift: a client that drops our root from its
	// reported set must not drag an explicit pin away. (Against a roots-origin
	// pin the same notification DOES re-pin — covered by
	// TestPersistPin_RootsChangedIsNotASessionStartPin.)
	store, ss := newOriginStore(t)
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)

	s := newPersistSession(t, store, ss, "proxyX")
	defer s.close()
	if _, err := s.repinWorkspace(context.Background(), rootA, "", false); err != nil {
		t.Fatalf("first explicit pin: %v", err)
	}

	s.onRootsChanged(context.Background(), []string{"file://" + rootB})
	if got := s.workspace(); got != rootA {
		t.Fatalf("a roots change moved an explicit pin: got %q, want %q", got, rootA)
	}

	// Control: a roots-origin pin still follows a genuine folder switch.
	s2 := newPersistSession(t, store, ss, "proxyY")
	defer s2.close()
	s2.attachWorkspace(context.Background(), "file://"+rootA)
	s2.onRootsChanged(context.Background(), []string{"file://" + rootB})
	if got := s2.workspace(); got != rootB {
		t.Fatalf("a roots-origin pin should follow the client's switch: got %q, want %q", got, rootB)
	}
}

func TestStickyPin_SameRootSessionStartMakesPinSticky(t *testing.T) {
	// The promotion edge: a session_start naming the CURRENT roots-attached
	// root upgrades the origin to session_start (persisted-pin rule). From that
	// point the pin is sticky, even though the root never moved.
	store, ss := newOriginStore(t)
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)

	s := newPersistSession(t, store, ss, "proxyX")
	defer s.close()
	s.attachWorkspace(context.Background(), "file://"+rootA) // origin roots
	if _, err := s.repinWorkspace(context.Background(), rootA, "", false); err != nil {
		t.Fatalf("same-root explicit pin: %v", err)
	}

	if _, err := s.repinWorkspace(context.Background(), rootB, "", false); err == nil {
		t.Fatal("a same-root session_start promotion should have made the pin sticky")
	}
}

// sessionHealth reads the persisted Health/HealthMessage for a session ID.
func sessionHealth(t *testing.T, id string) (string, string) {
	t.Helper()
	infos, err := session.List()
	if err != nil {
		t.Fatalf("session.List: %v", err)
	}
	for _, info := range infos {
		if info.ID == id {
			return info.Health, info.HealthMessage
		}
	}
	t.Fatalf("session %s not found", id)
	return "", ""
}

func TestStickyPin_RefusalMarksSessionHealth(t *testing.T) {
	// A refused steal attempt is surfaced to the operator the same way a
	// boundary violation is (Health: blocked in the TUI/dashboard), and a later
	// successful forced re-pin clears it — the session is healthy again.
	store, ss := newOriginStore(t)
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)

	s := newPersistSession(t, store, ss, "proxyX")
	defer s.close()
	if _, err := s.repinWorkspace(context.Background(), rootA, "", false); err != nil {
		t.Fatalf("first explicit pin: %v", err)
	}
	if health, _ := sessionHealth(t, s.sessID); health != "" {
		t.Fatalf("precondition: health = %q, want clear", health)
	}

	if _, err := s.repinWorkspace(context.Background(), rootB, "", false); err == nil {
		t.Fatal("precondition: the conflicting re-pin should have been refused")
	}
	health, msg := sessionHealth(t, s.sessID)
	if health != "blocked" {
		t.Errorf("health after refusal = %q, want %q", health, "blocked")
	}
	if !strings.Contains(msg, "sticky") || !strings.Contains(msg, rootB) {
		t.Errorf("health message should name the sticky pin and the requested root, got %q", msg)
	}

	// The victim's own same-root session_start — the natural next call in the
	// #182 field report — must heal the session, not only a forced switch.
	if _, err := s.repinWorkspace(context.Background(), rootA, "", false); err != nil {
		t.Fatalf("same-root re-pin: %v", err)
	}
	if health, msg := sessionHealth(t, s.sessID); health != "" {
		t.Errorf("health after the victim's same-root re-pin = %q (%q), want clear", health, msg)
	}

	if _, err := s.repinWorkspace(context.Background(), rootB, "", false); err == nil {
		t.Fatal("precondition: the second conflicting re-pin should have been refused")
	}
	if health, _ := sessionHealth(t, s.sessID); health != "blocked" {
		t.Fatalf("precondition: health = %q, want blocked again", health)
	}
	if _, err := s.repinWorkspace(context.Background(), rootB, "", true); err != nil {
		t.Fatalf("forced re-pin: %v", err)
	}
	if health, msg := sessionHealth(t, s.sessID); health != "" {
		t.Errorf("health after successful forced re-pin = %q (%q), want clear", health, msg)
	}
}

func TestStickyPin_ConcurrentExplicitPins_ExactlyOneLands(t *testing.T) {
	// The check-then-act hardening: the guard runs on the view under mutation,
	// so two explicit re-pins racing on an unpinned connection serialise —
	// whichever enters the lane first lands and makes the pin sticky, and the
	// other is refused. Under the old outside-the-lane snapshot check, both
	// could pass and the loser's pin was silently replaced.
	store, ss := newOriginStore(t)
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)

	s := newPersistSession(t, store, ss, "proxyX")
	defer s.close()

	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, target := range []string{rootA, rootB} {
		wg.Go(func() {
			_, errs[i] = s.repinWorkspace(context.Background(), target, "", false)
		})
	}
	wg.Wait()

	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("exactly one racing explicit pin must land: errA=%v errB=%v", errs[0], errs[1])
	}
	winner := rootA
	if errs[0] != nil {
		winner = rootB
	}
	if got := s.workspace(); got != winner {
		t.Fatalf("workspace = %q, want the winning pin %q", got, winner)
	}
}

func TestStickyPin_DirectRootsRepinKeptInLane(t *testing.T) {
	// The in-lane guard is authoritative for the roots vector too: even when
	// onRootsChanged's snapshot fast path is bypassed (a roots-origin re-pin
	// racing the explicit pin), a live roots-driven re-pin silently keeps an
	// explicit pin — no error, pin and origin unchanged.
	store, ss := newOriginStore(t)
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)

	s := newPersistSession(t, store, ss, "proxyX")
	defer s.close()
	if _, err := s.repinWorkspace(context.Background(), rootA, "", false); err != nil {
		t.Fatalf("first explicit pin: %v", err)
	}

	if _, err := s.repinWorkspaceFrom(context.Background(), rootB, "", sessionstate.PinSourceRoots, pinTriggerLive, false); err != nil {
		t.Fatalf("a roots-driven re-pin against an explicit pin must be a silent keep, not an error: %v", err)
	}
	if got := s.workspace(); got != rootA {
		t.Fatalf("a roots-driven re-pin moved an explicit pin: got %q, want %q", got, rootA)
	}
	if got := s.view().pinOrigin; got != sessionstate.PinSourceSessionStart {
		t.Fatalf("pin origin demoted to %q, want session_start", got)
	}
}

func TestStickyPin_MarkerlessExplicitPinIsSticky(t *testing.T) {
	// The contract must not depend on the folder having a .git or language
	// marker: a first explicit session_start naming a MARKERLESS folder routes
	// through onBeforeTool's auto-attach seeding into attachSynthetic, which
	// used to flatten the origin to unknown — leaving the pin stealable.
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ss, err := sessionstate.Open()
	if err != nil {
		t.Fatalf("sessionstate.Open: %v", err)
	}
	t.Cleanup(func() { ss.Close() })
	cfg := config.Defaults()
	cfg.Workspace.AutoAttach = true // enable the synthetic-root fallback
	store := config.NewStore(cfg)

	rootA, rootB := freshTempDir(t), freshTempDir(t) // deliberately NO markers
	s := newPersistSession(t, store, ss, "proxyX")
	defer s.close()

	// The daemon runs onBeforeTool before session_start's Execute; on an
	// unattached connection this is what attaches the explicit workspace arg.
	s.onBeforeTool(context.Background(), "session_start", json.RawMessage(`{"workspace":"`+rootA+`"}`))
	if got := s.workspace(); got != rootA {
		t.Fatalf("markerless explicit pin did not attach: workspace = %q, want %q", got, rootA)
	}
	if got := s.view().pinOrigin; got != sessionstate.PinSourceSessionStart {
		t.Fatalf("pin origin = %q, want session_start", got)
	}

	if _, err := s.repinWorkspace(context.Background(), rootB, "", false); err == nil {
		t.Fatal("a markerless explicit pin must be sticky: the peer re-pin should have been refused")
	}
	if got := s.workspace(); got != rootA {
		t.Fatalf("the refused re-pin still moved the pin: got %q, want %q", got, rootA)
	}
}

func TestStickyPin_LanguageOnlyRepinNotRefused(t *testing.T) {
	// forceLanguage re-pins the CURRENT root with a language override and no
	// force flag; the guard's different-root clause must let it through even
	// while the pin is sticky. (An inactive override is ignored by resolution —
	// the point here is only that the call is not refused.)
	store, ss := newOriginStore(t)
	root := freshTempDir(t)
	mustGitDir(t, root)

	s := newPersistSession(t, store, ss, "proxyX")
	defer s.close()
	if _, err := s.repinWorkspace(context.Background(), root, "", false); err != nil {
		t.Fatalf("first explicit pin: %v", err)
	}
	if _, err := s.repinWorkspace(context.Background(), root, "swift", false); err != nil {
		t.Fatalf("language-only re-pin of a sticky pin must not be refused: %v", err)
	}
	if got := s.workspace(); got != root {
		t.Fatalf("workspace = %q, want %q", got, root)
	}
}

func TestStickyPin_RestoredRootsOriginPinNotSticky(t *testing.T) {
	// The restore counterpart of the roots-origin rule: a roots-held pin
	// replayed after a daemon restart keeps its weaker origin, so the first
	// explicit session_start of the new process can still land without force.
	store, ss := newOriginStore(t)
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)

	before := newPersistSession(t, store, ss, "proxyX")
	before.attachWorkspace(context.Background(), "file://"+rootA) // origin roots
	before.close()

	after := newPersistSession(t, store, ss, "proxyX")
	defer after.close()
	after.rehydratePin(context.Background())
	if got := after.workspace(); got != rootA {
		t.Fatalf("precondition: restored pin = %q, want %q", got, rootA)
	}
	if got := after.view().pinOrigin; got != sessionstate.PinSourceRoots {
		t.Fatalf("precondition: restored origin = %q, want roots", got)
	}

	if _, err := after.repinWorkspace(context.Background(), rootB, "", false); err != nil {
		t.Fatalf("a restored roots-origin pin must not block the first explicit pin: %v", err)
	}
	if got := after.workspace(); got != rootB {
		t.Fatalf("explicit re-pin did not land: got %q, want %q", got, rootB)
	}
}

// newSessionStartTool wires a tools.SessionStart to a connSession the way
// registerAllTools does for the pieces under test here (workspace + re-pin),
// so these tests exercise the REAL tool surface, not just the daemon API —
// resolveSessionWorkspace's sameDir routing sits between the two.
func newSessionStartTool(s *connSession) *tools.SessionStart {
	return tools.NewSessionStart(s.workspaceFor, nil, nil, nil, func() string { return "" }, nil).
		WithRepin(s.repinWorkspace)
}

func TestStickyPin_VictimSameRootSessionStartHealsViaTool(t *testing.T) {
	// The heal must be reachable through the real MCP call: the #182 victim's
	// natural next move is session_start({workspace: A}) with A EXACTLY the
	// pinned root. resolveSessionWorkspace used to short-circuit on sameDir and
	// never reach the daemon, leaving the session flagged blocked forever.
	store, ss := newOriginStore(t)
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)

	s := newPersistSession(t, store, ss, "proxyX")
	defer s.close()
	if _, err := s.repinWorkspace(context.Background(), rootA, "", false); err != nil {
		t.Fatalf("first explicit pin: %v", err)
	}
	if _, err := s.repinWorkspace(context.Background(), rootB, "", false); err == nil {
		t.Fatal("precondition: the conflicting re-pin should have been refused")
	}
	if health, _ := sessionHealth(t, s.sessID); health != "blocked" {
		t.Fatalf("precondition: health = %q, want blocked", health)
	}

	out, err := newSessionStartTool(s).Execute(context.Background(), json.RawMessage(`{"workspace":"`+rootA+`"}`))
	if err != nil {
		t.Fatalf("victim's same-root session_start: %v", err)
	}
	if strings.Contains(out, "Re-pinned this connection") {
		t.Errorf("same-root session_start must not announce a re-pin\n%s", out)
	}
	if health, msg := sessionHealth(t, s.sessID); health != "" {
		t.Errorf("health after the victim's same-root session_start = %q (%q), want clear", health, msg)
	}
	if got := s.workspace(); got != rootA {
		t.Fatalf("workspace = %q, want %q", got, rootA)
	}
}

func TestStickyPin_SameRootPromotionThroughToolSurface(t *testing.T) {
	// The promotion twin of the heal test: session_start naming EXACTLY the
	// roots-attached root must make the pin sticky through the tool surface,
	// not only via the daemon API.
	store, ss := newOriginStore(t)
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)

	s := newPersistSession(t, store, ss, "proxyX")
	defer s.close()
	s.attachWorkspace(context.Background(), "file://"+rootA) // origin roots

	if _, err := newSessionStartTool(s).Execute(context.Background(), json.RawMessage(`{"workspace":"`+rootA+`"}`)); err != nil {
		t.Fatalf("same-root session_start: %v", err)
	}
	if got := s.view().pinOrigin; got != sessionstate.PinSourceSessionStart {
		t.Fatalf("pin origin = %q, want session_start (promotion through the tool surface)", got)
	}
	if _, err := s.repinWorkspace(context.Background(), rootB, "", false); err == nil {
		t.Fatal("the promoted pin should be sticky")
	}
}

func TestStickyPin_AliasOfOwnRootNotRefusedViaTool(t *testing.T) {
	// sameDir (os.SameFile) recognises alias spellings of the current root — a
	// symlink, a macOS firmlink (/var vs /private/var) — while the daemon's
	// sticky guard compares literal resolved roots. The tool layer must hand
	// the daemon the pinned spelling, so a caller naming their OWN workspace
	// through an alias is never refused as a peer steal, never torn down, and
	// never flagged blocked.
	store, ss := newOriginStore(t)
	root := freshTempDir(t)
	mustGitDir(t, root)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlink creation failed: %v", err)
	}

	s := newPersistSession(t, store, ss, "proxyX")
	defer s.close()
	if _, err := s.repinWorkspace(context.Background(), root, "", false); err != nil {
		t.Fatalf("first explicit pin: %v", err)
	}

	out, err := newSessionStartTool(s).Execute(context.Background(), json.RawMessage(`{"workspace":"`+alias+`"}`))
	if err != nil {
		t.Fatalf("an alias of the caller's own root must not be refused: %v", err)
	}
	if strings.Contains(out, "Re-pinned this connection") {
		t.Errorf("alias call must not announce a re-pin\n%s", out)
	}
	if got := s.workspace(); got != root {
		t.Fatalf("workspace = %q, want the pinned spelling %q", got, root)
	}
	if health, msg := sessionHealth(t, s.sessID); health != "" {
		t.Errorf("health = %q (%q), want clear — an alias call is not a steal attempt", health, msg)
	}
}

func TestStickyPin_RedundantSameRootCallKeepsTrackers(t *testing.T) {
	// Detect-vs-acquired drift (here: a go.mod root whose language server
	// cannot be acquired, so the acquired language stays none while Detect
	// says go) must not let a REDUNDANT same-root session_start take the
	// teardown path — write tracking (and with it undo history and strict-mode
	// read state) must survive. Only an explicit active language override
	// re-acquires on the same root.
	store, ss := newOriginStore(t)
	root := freshTempDir(t)
	mustGitDir(t, root)
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	s := newPersistSession(t, store, ss, "proxyX")
	defer s.close()
	if _, err := s.repinWorkspace(context.Background(), root, "", false); err != nil {
		t.Fatalf("first explicit pin: %v", err)
	}
	if got := s.view().acquiredLanguage; got != LanguageNone {
		t.Skipf("precondition: expected a failed acquire (language none), got %q — no drift to exercise", got)
	}

	written := filepath.Join(root, "touched.go")
	s.writeTracker.Record(written)
	if !s.writeTracker.Wrote(written) {
		t.Fatal("precondition: write tracker should hold the recorded path")
	}

	if _, err := s.repinWorkspace(context.Background(), root, "", false); err != nil {
		t.Fatalf("redundant same-root re-pin: %v", err)
	}
	if !s.writeTracker.Wrote(written) {
		t.Fatal("a redundant same-root session_start reset the write tracker (teardown path taken despite no explicit language override)")
	}
}

func TestStickyPin_MarkerlessPinRestoredAcrossRestart(t *testing.T) {
	// A persisted markerless (synthetic-root) pin must survive a daemon
	// restart through rehydratePin: Detect still finds no marker, so the
	// restore re-synthesises under the loaded origin instead of deferring —
	// and the restored pin is just as sticky as it was live.
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ss, err := sessionstate.Open()
	if err != nil {
		t.Fatalf("sessionstate.Open: %v", err)
	}
	t.Cleanup(func() { ss.Close() })
	cfg := config.Defaults()
	cfg.Workspace.AutoAttach = true
	store := config.NewStore(cfg)

	rootA, rootB := freshTempDir(t), freshTempDir(t) // deliberately NO markers
	before := newPersistSession(t, store, ss, "proxyX")
	before.onBeforeTool(context.Background(), "session_start", json.RawMessage(`{"workspace":"`+rootA+`"}`))
	if got := before.workspace(); got != rootA {
		t.Fatalf("precondition: markerless explicit pin = %q, want %q", got, rootA)
	}
	before.close()

	after := newPersistSession(t, store, ss, "proxyX")
	defer after.close()
	after.rehydratePin(context.Background())
	if got := after.workspace(); got != rootA {
		t.Fatalf("restored markerless pin = %q, want %q", got, rootA)
	}
	if got := after.view().pinOrigin; got != sessionstate.PinSourceSessionStart {
		t.Fatalf("restored origin = %q, want session_start", got)
	}
	if got := after.view().pinVia; got != "restore:session_start" {
		t.Fatalf("restored provenance label = %q, want restore:session_start", got)
	}
	if _, err := after.repinWorkspace(context.Background(), rootB, "", false); err == nil {
		t.Fatal("a restored markerless explicit pin must still be sticky")
	}
}

func TestStickyPin_ForceOnRootsHeldPinLands(t *testing.T) {
	// force against a non-sticky pin is a harmless no-op flag: the re-pin
	// lands exactly as the forceless one would.
	store, ss := newOriginStore(t)
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)

	s := newPersistSession(t, store, ss, "proxyX")
	defer s.close()
	s.attachWorkspace(context.Background(), "file://"+rootA) // origin roots

	if _, err := s.repinWorkspace(context.Background(), rootB, "", true); err != nil {
		t.Fatalf("forced re-pin over a roots-held pin: %v", err)
	}
	if got := s.workspace(); got != rootB {
		t.Fatalf("forced re-pin did not land: got %q, want %q", got, rootB)
	}
}

func TestStickyPin_MarkerlessExplicitPinPinsUnderDefaultAutoAttach(t *testing.T) {
	// The sticky contract is unconditional: an explicit session_start workspace
	// arg must synthesise a root for a markerless directory even under the
	// DEFAULT config (workspace.auto_attach = false), which used to gate the
	// synthesis and leave the connection unpinned while the workspace+language
	// path synthesised regardless (PLAN-266 item 2).
	store, ss := newOriginStore(t) // config.Defaults(): auto_attach = false

	rootA, rootB := freshTempDir(t), freshTempDir(t) // deliberately NO markers
	s := newPersistSession(t, store, ss, "proxyX")
	defer s.close()

	s.onBeforeTool(context.Background(), "session_start", json.RawMessage(`{"workspace":"`+rootA+`"}`))
	if got := s.workspace(); got != rootA {
		t.Fatalf("markerless explicit pin did not attach under default auto_attach: workspace = %q, want %q", got, rootA)
	}
	if got := s.view().pinOrigin; got != sessionstate.PinSourceSessionStart {
		t.Fatalf("pin origin = %q, want session_start", got)
	}

	if _, err := s.repinWorkspace(context.Background(), rootB, "", false); err == nil {
		t.Fatal("a markerless explicit pin under default auto_attach must be sticky: the peer re-pin should have been refused")
	}
	if got := s.workspace(); got != rootA {
		t.Fatalf("the refused re-pin still moved the pin: got %q, want %q", got, rootA)
	}
}

func TestStickyPin_LanguageOverrideOnStickyPinLogsBreadcrumb(t *testing.T) {
	// A peer can flip the shared connection's primary language without force:
	// the same-root language override takes the teardown path and resets the
	// read/write/undo trackers. The deliberate call stays honoured (accepted
	// design, PLAN-266 item 3), but a Warn breadcrumb must surface the reset to
	// the operator rather than let it pass silently.
	store, ss := newOriginStore(t)
	root := freshTempDir(t)
	mustGitDir(t, root)

	s := newPersistSession(t, store, ss, "proxyX")
	defer s.close()
	if _, err := s.repinWorkspace(context.Background(), root, "", false); err != nil {
		t.Fatalf("first explicit pin: %v", err)
	}

	buf := captureLog(s)
	if _, err := s.repinWorkspace(context.Background(), root, "go", false); err != nil {
		t.Fatalf("language override re-pin: %v", err)
	}
	for _, want := range []string{"primary language overridden", "sticky pin", "issue #182"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("language-override breadcrumb missing %q:\n%s", want, buf.String())
		}
	}
}
