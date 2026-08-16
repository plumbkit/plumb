package cli

// conn_attachoninit_test.go — the OnInit attach ladder, driven end to end.
//
// These exercise attachOnInit itself rather than hand-replaying its rungs, so
// the ordering the ladder encodes is actually covered. The regression they guard
// is a silent cross-repository write: a connection re-pinned to project B by
// session_start came back attached to project A (the client's launch root) after
// a daemon restart, and a relative-path write then resolved against A.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/sessionstate"
	"github.com/plumbkit/plumb/internal/tools"
)

// rootsReplying returns a client-request fake that answers roots/list with root,
// counting how many times it was asked.
func rootsReplying(root string, calls *int) mcp.RequestFn {
	return func(_ context.Context, method string, _ any) (json.RawMessage, error) {
		if method != "roots/list" {
			return nil, nil
		}
		*calls++
		return json.RawMessage(`{"roots":[{"uri":"file://` + root + `"}]}`), nil
	}
}

// reconnect simulates a daemon restart: a fresh connSession under the same proxy
// session ID and the same store, whose client still reports rootsRoot.
func reconnect(t *testing.T, store *config.Store, ss *sessionstate.Store, rootsRoot string, calls *int) *connSession {
	t.Helper()
	after := newPersistSession(t, store, ss, "proxyX")
	after.setClientRequest(rootsReplying(rootsRoot, calls))
	after.attachOnInit(context.Background(), rootsReplying(rootsRoot, calls))
	return after
}

// TestExplicitRepinSurvivesDaemonRestart is the headline regression: the exact
// sequence that wrote a file into the wrong repository.
func TestExplicitRepinSurvivesDaemonRestart(t *testing.T) {
	store, ss := newOriginStore(t)
	rootA, rootB := freshTempDir(t), freshTempDir(t) // A = client's launch root
	mustGitDir(t, rootA)                             // B = deliberately chosen
	mustGitDir(t, rootB)

	calls := 0
	before := newPersistSession(t, store, ss, "proxyX")
	before.attachOnInit(context.Background(), rootsReplying(rootA, &calls))
	if got := before.workspace(); got != rootA {
		t.Fatalf("first attach = %q, want the client root %q", got, rootA)
	}
	if _, err := before.repinWorkspace(context.Background(), rootB, "", false); err != nil {
		t.Fatalf("repinWorkspace: %v", err)
	}
	before.close()

	after := reconnect(t, store, ss, rootA, &calls)
	if got := after.workspace(); got != rootB {
		t.Fatalf("reconnect landed on %q; a deliberate session_start pin (%q) must outrank client roots (%q)",
			got, rootB, rootA)
	}
}

func TestOnInit_RootsAttachDoesNotClobberSessionStartPin(t *testing.T) {
	// The pin was not merely ignored on reconnect — the roots attach persisted
	// over it, so even a later rehydrate could not recover the chosen workspace.
	store, ss := newOriginStore(t)
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)

	calls := 0
	before := newPersistSession(t, store, ss, "proxyX")
	before.attachOnInit(context.Background(), rootsReplying(rootA, &calls))
	if _, err := before.repinWorkspace(context.Background(), rootB, "", false); err != nil {
		t.Fatalf("repinWorkspace: %v", err)
	}
	before.close()

	reconnect(t, store, ss, rootA, &calls)

	ws, _, src, ok, err := ss.LoadPin("proxyX")
	if err != nil || !ok {
		t.Fatalf("LoadPin: ok=%v err=%v", ok, err)
	}
	if ws != rootB || src != sessionstate.PinSourceSessionStart {
		t.Fatalf("reconnect clobbered the pin: got (%q, %q), want (%q, %q)",
			ws, src, rootB, sessionstate.PinSourceSessionStart)
	}
}

func TestOnInit_SkipsRootsRPCWhenPinned(t *testing.T) {
	// roots/list is a synchronous round-trip to the client. A deliberate pin
	// suppresses the roots rung entirely, so it must not be asked at all.
	store, ss := newOriginStore(t)
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)

	calls := 0
	before := newPersistSession(t, store, ss, "proxyX")
	if _, err := before.repinWorkspace(context.Background(), rootB, "", false); err != nil {
		t.Fatalf("repinWorkspace: %v", err)
	}
	before.close()

	calls = 0
	reconnect(t, store, ss, rootA, &calls)
	if calls != 0 {
		t.Fatalf("roots/list called %d times on a connection already pinned by session_start", calls)
	}
}

func TestOnInit_RootsPinDoesNotBeatFreshRoots(t *testing.T) {
	// A roots-origin pin is only a cached copy of what the client said. If the
	// client now reports a different folder, the client is the newer authority.
	store, ss := newOriginStore(t)
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)

	calls := 0
	before := newPersistSession(t, store, ss, "proxyX")
	before.attachOnInit(context.Background(), rootsReplying(rootA, &calls)) // pins A, source=roots
	before.close()

	after := reconnect(t, store, ss, rootB, &calls) // client moved to B
	if got := after.workspace(); got != rootB {
		t.Fatalf("reconnect = %q, want the client's fresh root %q", got, rootB)
	}
}

func TestOnInit_LegacyPinDoesNotBeatRoots(t *testing.T) {
	// A row written before the source column existed must behave exactly as it
	// did before this change: roots wins. The upgrade is behaviour-neutral.
	store, ss := newOriginStore(t)
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)
	if err := ss.UpsertPin("proxyX", rootB, LanguageNone, sessionstate.PinSourceUnknown); err != nil {
		t.Fatalf("seed legacy pin: %v", err)
	}

	calls := 0
	after := reconnect(t, store, ss, rootA, &calls)
	if got := after.workspace(); got != rootA {
		t.Fatalf("legacy pin outranked client roots: got %q, want %q", got, rootA)
	}
}

func TestOnInit_ReplayedMetaPinBeatsRoots(t *testing.T) {
	// Rung 1 with no database at all: the serve proxy replayed the caller's
	// session_start workspace in _meta (onPinnedWorkspace), and it must outrank the
	// client's roots even when the persisted pin is absent (persist_state off, or
	// the row pruned). This is the channel that does not depend on the DB.
	store, ss := newOriginStore(t)
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)

	calls := 0
	s := newPersistSession(t, store, ss, "proxyX")
	s.onPinnedWorkspace(rootB) // as the replayed initialize _meta would
	s.setClientRequest(rootsReplying(rootA, &calls))
	s.attachOnInit(context.Background(), rootsReplying(rootA, &calls))

	if got := s.workspace(); got != rootB {
		t.Fatalf("replayed _meta pin lost to roots: got %q, want %q", got, rootB)
	}
	if calls != 0 {
		t.Fatalf("roots/list called %d times despite a replayed pin", calls)
	}
}

// wideRootOrSkip returns a directory that CONTAINS the machine's home directory
// (home's parent: /Users on macOS, /home on Linux CI), which is what #306's
// containment guard refuses without a declaration.
func wideRootOrSkip(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		t.Skipf("no user-database home: %v", err)
	}
	wide := filepath.Dir(u.HomeDir)
	if wide == "/" || wide == "." || wide == "" {
		t.Skipf("home %q sits at the filesystem root; no container to test", u.HomeDir)
	}
	return wide
}

// The initialize `_meta` pinned-workspace key is not authenticated: the daemon
// cannot distinguish a plumb serve proxy replaying an accepted session_start
// from any other MCP client that simply set the key. So it must not carry
// #306's home-containment exemption, which is the one authority unique to a
// declaration — a client shipping `_meta[pinned-workspace] = "/Users"` would
// otherwise put every home directory on the machine, and every credential under
// them, inside the session's read-write boundary (issue #318).
func TestOnInit_ReplayedMetaPinCannotClaimHomeContainment(t *testing.T) {
	wide := wideRootOrSkip(t)
	store, ss := newOriginStore(t)
	rootA := freshTempDir(t) // the client's honest launch root
	mustGitDir(t, rootA)

	calls := 0
	s := newPersistSession(t, store, ss, "proxyX")
	s.onPinnedWorkspace(wide) // as any client setting the _meta key would
	s.setClientRequest(rootsReplying(rootA, &calls))
	s.attachOnInit(context.Background(), rootsReplying(rootA, &calls))

	if got := s.workspace(); got == wide {
		t.Fatalf("replayed _meta pin attached %q — a root containing the home directory must not be reachable over an unauthenticated channel (issue #318)", got)
	}
	// Fail-safe, not fail-open: the ladder carries on and the honest client root
	// still attaches. A refusal that left the session unattached would be a
	// denial-of-service any client could inflict on itself.
	if got := s.workspace(); got != rootA {
		t.Fatalf("after refusing the wide replayed pin the ladder landed on %q, want the client root %q", got, rootA)
	}
	// And nothing wide may be left behind in the database for a later rehydrate.
	ws, _, _, _, err := ss.LoadPin("proxyX")
	if err != nil {
		t.Fatalf("LoadPin: %v", err) // an error here must not silently pass the check below
	}
	if ws == wide {
		t.Fatalf("a refused wide replayed pin was persisted as %q", ws)
	}
}

// The refusal's real cost, pinned so the docs cannot understate it. When the
// database has no row to fall back on — [session] persist_state off, a first
// connect, or (the case that bites in the DEFAULT configuration) a row older
// than [session] persist_state_ttl_minutes that the startup prune swept — a
// caller's DECLARED wide root does not come back at all. Every lower rung
// refuses it too, because roots and the cwd hint are weaker origins than the
// declaration that is now missing, so the connection returns UNATTACHED rather
// than pinned somewhere narrower. (No cwd hint is set here; with one, the last
// rung could attach an unrelated project instead — never wider, but not
// nothing.)
//
// It asserts the OUTCOME, not each rung's reasoning: with no marker at the wide
// root, detection alone would also decline it. The roots rung's own guard is
// covered by TestAttachWorkspace_HomeRootFromClientNeedsDeclaration.
func TestOnInit_UndeclaredFallbackLeavesWideRootUnattached(t *testing.T) {
	wide := wideRootOrSkip(t)
	store, ss := newOriginStore(t) // deliberately no pin row: the pruned/persist-off case

	calls := 0
	s := newPersistSession(t, store, ss, "proxyX")
	s.onPinnedWorkspace(wide)
	// The client reports the same wide directory, so the roots rung is the one
	// that would otherwise catch the fall.
	s.setClientRequest(rootsReplying(wide, &calls))
	s.attachOnInit(context.Background(), rootsReplying(wide, &calls))

	if got := s.workspace(); got != "" {
		t.Fatalf("a wide root was attached by a lower rung as %q — no origin but an explicit session_start may pin a home container", got)
	}
}

// A pin accepted over the replayed _meta channel must ALSO lose #306's
// exemption in the LIVE policy re-check, not only at attach (issue #318).
//
// The accepted pin deliberately records PinSourceSessionStart — that is what
// keeps rank, stickiness and persistence unchanged — but policyRootRefused keys
// on the same origin, so without a separate mark the channel would be refused a
// home container at attach and handed one on the next policy rebuild. That is
// the worse half to miss: the holder of this channel NAMES the root, so it can
// pass a clean directory through the attach check and then replace it with a
// symlink to a home container, which the 30-second config poll then absorbs.
// Found by independent adversarial review, which demonstrated the swap putting
// ~/.ssh read-write inside the boundary.
func TestOnInit_ReplayedMetaPinLosesExemptionOnPolicyRebuild(t *testing.T) {
	wide := wideRootOrSkip(t)
	store, ss := newOriginStore(t)

	// A perfectly innocent directory: it passes the attach-time containment check.
	candidate := freshTempDir(t)
	mustGitDir(t, candidate)

	calls := 0
	s := newPersistSession(t, store, ss, "proxyX")
	s.onPinnedWorkspace(candidate) // as the replayed initialize _meta would
	s.setClientRequest(rootsReplying(freshTempDir(t), &calls))
	s.attachOnInit(context.Background(), rootsReplying(freshTempDir(t), &calls))

	if got := s.workspace(); got != candidate {
		t.Fatalf("setup: workspace = %q, want the replayed root %q", got, candidate)
	}
	if s.boundaryPolicy() == nil {
		t.Fatal("setup: an innocent replayed root must have a policy before the swap")
	}

	// Swap the named directory for a symlink to a container of home, exactly as
	// the client that named it could.
	if err := os.RemoveAll(candidate); err != nil {
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
		t.Fatalf("a policy rebuild absorbed the swap: a pin claimed over the unauthenticated _meta channel now has a boundary of %q, which contains the home directory — every credential under it is inside the session (issues #306, #318)", wide)
	}
}

// The mark is about the CHANNEL, not the root: a live session_start re-pin
// clears it, so a caller who deliberately declares a workspace gets the full
// exemption back on the very next pin. Without this, one replayed pin would
// poison the connection's authority for the rest of its life.
func TestOnInit_LiveRepinClearsTheUnverifiedReplayMark(t *testing.T) {
	store, ss := newOriginStore(t)
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)

	calls := 0
	s := newPersistSession(t, store, ss, "proxyX")
	s.onPinnedWorkspace(rootA)
	s.setClientRequest(rootsReplying(rootA, &calls))
	s.attachOnInit(context.Background(), rootsReplying(rootA, &calls))
	if !s.view().pinUnverifiedReplay {
		t.Fatal("setup: an accepted replayed pin must be marked unverified")
	}

	if _, err := s.repinWorkspace(context.Background(), rootB, "", true); err != nil {
		t.Fatalf("repinWorkspace: %v", err)
	}
	if s.view().pinUnverifiedReplay {
		t.Fatal("a live session_start re-pin must clear the unverified-replay mark")
	}
}

// An unrestored wide claim must reach the OPERATOR, not just the log at Info.
//
// The refusal stays quiet because it fires on every restart of a legitimately
// declared wide workspace. Once the ladder has finished, a claim that nothing
// restored is worth surfacing whatever caused it — the session is not attached
// where its client asked. It does NOT follow that the claim was forged: the
// daemon cannot tell, and this release's own sweep produces the same shape for
// a caller who really did declare that root (issue #318).
func TestOnInit_UnbackedWideClaimMarksTheSessionBlocked(t *testing.T) {
	wide := wideRootOrSkip(t)
	store, ss := newOriginStore(t) // no row: nothing vouches for the claim
	rootA := freshTempDir(t)
	mustGitDir(t, rootA)

	calls := 0
	s := newPersistSession(t, store, ss, "proxyX")
	s.onPinnedWorkspace(wide)
	s.setClientRequest(rootsReplying(rootA, &calls))
	s.attachOnInit(context.Background(), rootsReplying(rootA, &calls))

	health, msg := sessionHealth(t, s.sessID)
	if health != "blocked" {
		t.Fatalf("an unrestored wide claim left health = %q, want \"blocked\" — it must be visible to an operator", health)
	}
	assertClaimMessage(t, msg, wide)
}

// assertClaimMessage binds what the operator-facing text must and must NOT say.
//
// This is the THIRD attempt at guarding it, and the first two both fell to the
// same trap they were written to close — worth stating so the next edit does not
// make it four:
//
//  1. Only `Contains(msg, root)`. The rejected accusatory wording names the
//     root too, so restoring it stayed green.
//  2. Added `Contains(msg, "session_start")` as a "carries the remedy" check.
//     The accusation contains that token as well — "with no session_start
//     behind it" — so it still stayed green. A banned-substring list beside it
//     was no better: one of its entries only ever appeared in the LOG line, not
//     in this message, so it bound nothing at all, and any freshly-worded
//     accusation walked straight past the rest.
//
// A banned-word list cannot bind a claim; it only bans spellings. So the load-
// bearing assertion here is POSITIVE and specific: the message must contain the
// actionable call form, `session_start({workspace: "<root>"`. An accusation does
// not incidentally contain that — it is the remedy itself, quoted, with the very
// root at issue.
//
// Be honest about the remaining hole rather than claim it is closed: a message
// that carries the remedy AND accuses anyway still passes, and review confirmed
// one can be written. String matching cannot decide what a sentence asserts.
// What this guard does buy is that the message can never SILENTLY lose its
// remedy — which is the half that changes what a caller can do about it — and
// that the specific wordings a review has already rejected cannot come back.
func assertClaimMessage(t *testing.T, msg, root string) {
	t.Helper()
	if !strings.Contains(msg, root) {
		t.Errorf("message does not name the claimed root %q: %s", root, msg)
	}
	// The primary bind: an actionable remedy naming this root. This is what the
	// rejected wording cannot satisfy, whatever else it says.
	if remedy := fmt.Sprintf("session_start({workspace: %q", root); !strings.Contains(msg, remedy) {
		t.Errorf("message does not carry the actionable remedy %q — marking a legitimate caller's session blocked is only defensible if it tells them what to do: %s", remedy, msg)
	}
	// Second line of defence. The daemon cannot tell a forged claim from a
	// declaration whose row the sweep or the TTL prune removed, so it must not
	// assert either intent OR the absence of a declaration it cannot observe.
	for _, banned := range []string{
		"forged", "forgery", "fabricat", "rogue",
		"no session_start", "never ran", "nothing corroborates", "never used",
	} {
		if strings.Contains(strings.ToLower(msg), banned) {
			t.Errorf("message accuses the client or asserts an absence it cannot know (%q): %s", banned, msg)
		}
	}
}

// ...and a wide root the caller really did declare must NOT raise it. The row
// restores the same directory, so the claim is corroborated and the session is
// healthy. Without this the alarm would fire on every restart of a legitimate
// dotfiles session, which is exactly the crying-wolf the Info-level refusal was
// demoted to avoid.
func TestOnInit_DeclaredWideRootDoesNotRaiseTheAlarm(t *testing.T) {
	wide := wideRootOrSkip(t)
	store, ss := newOriginStore(t)
	if err := ss.UpsertPin("proxyX", wide, LanguageNone, sessionstate.PinSourceSessionStart); err != nil {
		t.Fatalf("seed declared wide pin: %v", err)
	}
	rootA := freshTempDir(t)
	mustGitDir(t, rootA)

	calls := 0
	s := newPersistSession(t, store, ss, "proxyX")
	s.onPinnedWorkspace(wide)
	s.setClientRequest(rootsReplying(rootA, &calls))
	s.attachOnInit(context.Background(), rootsReplying(rootA, &calls))

	if got := s.workspace(); got != wide {
		t.Fatalf("setup: the declared wide root did not restore: got %q, want %q", got, wide)
	}
	if health, msg := sessionHealth(t, s.sessID); health != "" {
		t.Fatalf("a restored declaration raised the unrestored-claim alarm: health=%q msg=%s", health, msg)
	}
}

// The alarm must stay SILENT for an ordinary session — the half of the design
// that justified keeping the refusal at Info in the first place. Without this,
// dropping the `refused == ""` early return marks EVERY session blocked and the
// whole suite stays green, which an adversarial review demonstrated.
func TestOnInit_OrdinarySessionIsNotMarkedBlocked(t *testing.T) {
	store, ss := newOriginStore(t)
	root := freshTempDir(t)
	mustGitDir(t, root)

	calls := 0
	s := newPersistSession(t, store, ss, "proxyX")
	s.setClientRequest(rootsReplying(root, &calls))
	s.attachOnInit(context.Background(), rootsReplying(root, &calls))

	if got := s.workspace(); got != root {
		t.Fatalf("setup: workspace = %q, want %q", got, root)
	}
	if health, msg := sessionHealth(t, s.sessID); health != "" {
		t.Fatalf("an ordinary session with no replayed pin was marked %q: %s", health, msg)
	}
}

// The corroboration check compares CANONICALLY, and that is load-bearing. The
// proxy replays the session_start ARGUMENT it observed, while the attach
// resolves symlinks — so a caller who declared a workspace through a symlinked
// parent has a refused claim and an attached root that are the same directory
// under two spellings. A textual comparison would call that uncorroborated and
// raise the unrestored-claim alarm on a legitimate session, which is exactly the
// crying wolf this design set out to avoid.
//
// Exercised on reportUnbackedReplay directly: two spellings of a WIDE root are
// not portable to construct, but the comparison itself is what needs pinning.
func TestReportUnbackedReplay_ComparesCanonically(t *testing.T) {
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}
	mustGitDir(t, realDir)

	store, ss := newOriginStore(t)
	s := newPersistSession(t, store, ss, "proxyX")
	s.attachWorkspacePin(context.Background(), "file://"+realDir, sessionstate.PinSourceSessionStart)
	if got := s.workspace(); got == "" {
		t.Fatal("setup: the session did not attach")
	}

	// The claim names the SAME directory through the symlink.
	s.reportUnbackedReplay(link)

	if health, msg := sessionHealth(t, s.sessID); health != "" {
		t.Fatalf("a claim naming the attached directory under a second spelling raised the alarm: health=%q msg=%s", health, msg)
	}

	// Control: a genuinely different directory still raises it, so the test above
	// is not passing because the alarm never fires.
	s.reportUnbackedReplay(freshTempDir(t))
	if health, _ := sessionHealth(t, s.sessID); health != "blocked" {
		t.Fatalf("control: an unrelated claimed root left health = %q, want \"blocked\"", health)
	}
}

// The exemption is withheld from the _meta CHANNEL, not from the declaration
// itself. A caller who really did run session_start on a wide root had that
// origin recorded by this daemon, so rung 1b restores it from the database on
// reconnect — the restart path #182/#181 exist to protect is untouched.
func TestOnInit_DeclaredWideRootStillRestoresFromDatabase(t *testing.T) {
	wide := wideRootOrSkip(t)
	store, ss := newOriginStore(t)
	rootA := freshTempDir(t)
	mustGitDir(t, rootA)
	if err := ss.UpsertPin("proxyX", wide, LanguageNone, sessionstate.PinSourceSessionStart); err != nil {
		t.Fatalf("seed declared wide pin: %v", err)
	}

	calls := 0
	s := newPersistSession(t, store, ss, "proxyX")
	s.onPinnedWorkspace(wide) // the proxy replays the same fact it observed
	s.setClientRequest(rootsReplying(rootA, &calls))
	s.attachOnInit(context.Background(), rootsReplying(rootA, &calls))

	if got := s.workspace(); got != wide {
		t.Fatalf("a wide root declared by session_start and recorded in the database did not restore: got %q, want %q (issue #182's contract)", got, wide)
	}
}

// The narrowing must not cost the rung its rank: an ordinary project replayed
// over _meta still beats the client's roots, which is the restart regression
// TestOnInit_ReplayedMetaPinBeatsRoots guards. Asserted here too because the
// containment check runs before the attach and an over-broad predicate would
// silently demote every replayed pin to the roots rung.
func TestOnInit_ReplayedMetaPinKeepsRankForOrdinaryRoots(t *testing.T) {
	store, ss := newOriginStore(t)
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)

	calls := 0
	s := newPersistSession(t, store, ss, "proxyX")
	s.onPinnedWorkspace(rootB)
	s.setClientRequest(rootsReplying(rootA, &calls))
	s.attachOnInit(context.Background(), rootsReplying(rootA, &calls))

	if got := s.workspace(); got != rootB {
		t.Fatalf("ordinary replayed pin lost its rank: got %q, want %q", got, rootB)
	}
	if got := s.view().pinOrigin; got != sessionstate.PinSourceSessionStart {
		t.Fatalf("accepted replayed pin recorded origin %q, want %q — the channel check must not demote the pin it accepts",
			got, sessionstate.PinSourceSessionStart)
	}
}

func TestOnInit_RootsLessClientFallsBackToPin(t *testing.T) {
	// The pre-existing behaviour for Claude Desktop and friends: no roots, so the
	// persisted pin — of any origin — restores the connection.
	store, ss := newOriginStore(t)
	root := freshTempDir(t)
	mustGitDir(t, root)
	if err := ss.UpsertPin("proxyX", root, LanguageNone, sessionstate.PinSourceRoots); err != nil {
		t.Fatalf("seed pin: %v", err)
	}

	noRoots := func(_ context.Context, _ string, _ any) (json.RawMessage, error) {
		return json.RawMessage(`{"roots":[]}`), nil
	}
	s := newPersistSession(t, store, ss, "proxyX")
	s.setClientRequest(noRoots)
	s.attachOnInit(context.Background(), noRoots)

	if got := s.workspace(); got != root {
		t.Fatalf("roots-less client did not rehydrate: got %q, want %q", got, root)
	}
}
