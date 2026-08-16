package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/session"
)

// newIntentTestSession builds a connSession wired to a real per-temp-workspace
// collab pool, with the given [collab] snapshot, so intentHint's gating and
// formatting can be exercised hermetically.
func newIntentTestSession(t *testing.T, ws string, cc config.CollabConfig) *connSession {
	t.Helper()
	s := &connSession{
		store:      config.NewStore(config.Defaults()),
		collabPool: newCollabPool(),
		sessID:     "self",
		ctx:        context.Background(),
	}
	s.mutate(func(v *sessionView) {
		v.acquiredRoot = ws
		v.collab = cc
	})
	t.Cleanup(func() { s.collabPool.closeAll() })
	return s
}

// seedPeerIntent stores an intent authored by a peer session (author_id "peer").
func seedPeerIntent(t *testing.T, s *connSession, ws string, globs []string) {
	t.Helper()
	store := s.collabPool.acquire(ws)
	if store == nil {
		t.Fatal("acquire collab store")
	}
	err := store.PutIntent(context.Background(), collab.IntentInput{
		AuthorSession: "swift-falcon",
		AuthorID:      "peer",
		Body:          "refactoring the rate limiter",
		PathGlobs:     globs,
		TTL:           time.Hour,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
}

func TestIntentHint_MatchingPathIsLabelledClaim(t *testing.T) {
	ws := t.TempDir()
	s := newIntentTestSession(t, ws, config.CollabConfig{Intents: true, HintBudgetBytes: 512})
	seedPeerIntent(t, s, ws, []string{"ratelimit*"})
	args := []byte(`{"file_path":"` + filepath.Join(ws, "ratelimit.go") + `"}`)
	got := s.intentHint(args, ws)
	if !strings.Contains(got, "claim, unverified") {
		t.Errorf("intent hint must be labelled as an unverified claim; got %q", got)
	}
	if !strings.Contains(got, "swift-falcon") || !strings.Contains(got, "advisory") {
		t.Errorf("intent hint should name the peer and flag it advisory; got %q", got)
	}
}

func TestIntentHint_DisabledCleanly(t *testing.T) {
	ws := t.TempDir()
	s := newIntentTestSession(t, ws, config.CollabConfig{Intents: false, HintBudgetBytes: 512})
	seedPeerIntent(t, s, ws, []string{"ratelimit*"})
	args := []byte(`{"file_path":"` + filepath.Join(ws, "ratelimit.go") + `"}`)
	if got := s.intentHint(args, ws); got != "" {
		t.Errorf("intents=false must suppress the hint, got %q", got)
	}
}

func TestIntentHint_OwnIntentNotHinted(t *testing.T) {
	ws := t.TempDir()
	s := newIntentTestSession(t, ws, config.CollabConfig{Intents: true, HintBudgetBytes: 512})
	// An intent authored by THIS session (author_id "self") must not hint itself.
	store := s.collabPool.acquire(ws)
	_ = store.PutIntent(context.Background(), collab.IntentInput{
		AuthorSession: "me", AuthorID: "self", Body: "x", PathGlobs: []string{"ratelimit*"}, TTL: time.Hour,
	}, time.Now())
	args := []byte(`{"file_path":"` + filepath.Join(ws, "ratelimit.go") + `"}`)
	if got := s.intentHint(args, ws); got != "" {
		t.Errorf("a session must not be hinted about its own intent, got %q", got)
	}
}

func TestIntentHint_NonMatchingPath(t *testing.T) {
	ws := t.TempDir()
	s := newIntentTestSession(t, ws, config.CollabConfig{Intents: true, HintBudgetBytes: 512})
	seedPeerIntent(t, s, ws, []string{"internal/auth/*.go"})
	args := []byte(`{"file_path":"` + filepath.Join(ws, "ratelimit.go") + `"}`)
	if got := s.intentHint(args, ws); got != "" {
		t.Errorf("a non-matching path must not hint, got %q", got)
	}
}

func TestIntentHint_NoStoreNoCreation(t *testing.T) {
	ws := t.TempDir()
	s := newIntentTestSession(t, ws, config.CollabConfig{Intents: true, HintBudgetBytes: 512})
	// No intent has ever been written, so collab.db does not exist; the read path
	// must NOT create it and must yield no hint.
	args := []byte(`{"file_path":"` + filepath.Join(ws, "ratelimit.go") + `"}`)
	if got := s.intentHint(args, ws); got != "" {
		t.Errorf("no store should yield no hint, got %q", got)
	}
	if collab.Exists(ws) {
		t.Error("the read path must never create collab.db")
	}
}

func TestIntentHint_BudgetCap(t *testing.T) {
	ws := t.TempDir()
	s := newIntentTestSession(t, ws, config.CollabConfig{Intents: true, HintBudgetBytes: 40})
	seedPeerIntent(t, s, ws, []string{"ratelimit*"})
	args := []byte(`{"file_path":"` + filepath.Join(ws, "ratelimit.go") + `"}`)
	got := s.intentHint(args, ws)
	if got == "" {
		t.Fatal("expected a (clamped) hint")
	}
	if len([]byte(got)) > 40 {
		t.Errorf("intent hint %q exceeds the 40-byte budget (%d bytes)", got, len(got))
	}
}

// TestIntentHint_AdvisoryOnlyIsAdditive asserts the hint is a pure suffix
// appended to a tool's output — it never replaces or blocks the response. The
// enrich path (enrichToolOutput) concatenates it, so a write's result is
// unchanged apart from the trailing advisory block.
func TestIntentHint_AdvisoryOnlyIsAdditive(t *testing.T) {
	ws := t.TempDir()
	s := newIntentTestSession(t, ws, config.CollabConfig{Intents: true, HintBudgetBytes: 512})
	seedPeerIntent(t, s, ws, []string{"ratelimit*"})
	args := []byte(`{"file_path":"` + filepath.Join(ws, "ratelimit.go") + `"}`)
	base := "wrote ratelimit.go (42 bytes)"
	combined := base + s.intentHint(args, ws)
	if !strings.HasPrefix(combined, base) {
		t.Error("the intent hint must be appended, never replace the tool output")
	}
	if len(combined) <= len(base) {
		t.Error("expected an advisory block to be appended")
	}
}

// TestTargetAllowsCrossProject_ResolvesArbitraryWorkspace pins the difference
// from crossProjectOn (workspace_sessions_collab.go): that reads THIS
// connection's own cached [collab] snapshot, while targetAllowsCrossProject
// resolves an ARBITRARY other workspace's config fresh, on demand — the shape
// a daemon-wide display needs when checking consent for every participant in
// a conversation, none of which need be the workspace this session is pinned
// to.
func TestTargetAllowsCrossProject_ResolvesArbitraryWorkspace(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	other := t.TempDir()
	writeProjectConfig(t, other, "[collab]\ncross_project = true\n")
	grantExecTrust(t, other)

	// This session is pinned to an unrelated workspace; the question is about
	// `other`, not about where this session lives.
	s := newIntentTestSession(t, t.TempDir(), config.CollabConfig{})

	if !s.targetAllowsCrossProject(other) {
		t.Error("a trusted, opted-in OTHER workspace must report consent")
	}
	if s.targetAllowsCrossProject(t.TempDir()) {
		t.Error("a workspace with no opt-in must not report consent")
	}
	if s.targetAllowsCrossProject("") {
		t.Error("an empty workspace must never report consent")
	}
}

// TestDaemonWideConversations_RequiresEveryParticipantToOptIn is the
// end-to-end wiring proof: real project configs, real trust grants, a real
// global collab store, filtered by the real connSession method.
func TestDaemonWideConversations_RequiresEveryParticipantToOptIn(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	optedIn := t.TempDir()
	writeProjectConfig(t, optedIn, "[collab]\ncross_project = true\n")
	grantExecTrust(t, optedIn)

	peerOptedIn := t.TempDir()
	writeProjectConfig(t, peerOptedIn, "[collab]\ncross_project = true\n")
	grantExecTrust(t, peerOptedIn)

	silent := t.TempDir() // never opts in

	s := newIntentTestSession(t, optedIn, config.CollabConfig{})
	global := s.collabPool.acquireGlobal()
	if global == nil {
		t.Fatal("acquire global collab store")
	}
	ctx, now := context.Background(), time.Now()

	// Both participants consent: must be included.
	unanimous, err := global.PutNote(ctx, collab.NoteInput{
		AuthorSession: "a", AuthorID: "a", Body: "hi", Addressee: "b",
		TTL: time.Hour, OriginWorkspace: optedIn, TargetWorkspace: peerOptedIn,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	// One participant never consented: must be excluded even though the other did.
	partial, err := global.PutNote(ctx, collab.NoteInput{
		AuthorSession: "a", AuthorID: "a", Body: "hi", Addressee: "c",
		TTL: time.Hour, OriginWorkspace: optedIn, TargetWorkspace: silent,
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.daemonWideConversations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(got))
	for _, c := range got {
		ids = append(ids, c.ID)
	}
	found := false
	for _, id := range ids {
		if id == unanimous {
			found = true
		}
		if id == partial {
			t.Errorf("a conversation with a non-consenting participant leaked into the daemon-wide view: %v", ids)
		}
	}
	if !found {
		t.Errorf("the unanimously-consented conversation is missing from the daemon-wide view: %v", ids)
	}
}

// TestDaemonWideConversations_NoGlobalStoreYieldsNothingWithoutCreatingOne: the
// common case (no cross-project traffic has ever happened on this daemon) must
// answer cleanly, and the read path must never be the thing that materialises
// collab-xproject.db.
func TestDaemonWideConversations_NoGlobalStoreYieldsNothingWithoutCreatingOne(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	s := newIntentTestSession(t, t.TempDir(), config.CollabConfig{})

	got, err := s.daemonWideConversations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("expected nil with no global store, got %+v", got)
	}
	if collab.GlobalExists() {
		t.Error("the read path must never create the global collab store")
	}
}

// writeLiveSessionFile plants a session file directly in the session directory.
// It exists because session.Register REFUSES a name a live session already
// holds, so the duplicate-name state resolvePeer must survive cannot be reached
// through the API — only by a stale or hand-written file, which is exactly the
// case the resolver must not guess its way through.
func writeLiveSessionFile(t *testing.T, info session.Info) {
	t.Helper()
	dir, err := session.Dir()
	if err != nil {
		t.Fatalf("session.Dir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir session dir: %v", err)
	}
	info.PID = os.Getpid() // a live pid, or listLocked marks it ended
	info.StartedAt = time.Now()
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, info.ID+".json"), data, 0o600); err != nil {
		t.Fatalf("write session file: %v", err)
	}
}

// TestTargetAllowsCrossProject_NoProjectConfigRefuses is PLAN-334's real-wiring
// half: a target workspace with no .plumb/config.toml at all must resolve to
// the compiled default (cross_project = false), not error out ambiguously.
func TestTargetAllowsCrossProject_NoProjectConfigRefuses(t *testing.T) {
	s := newIntentTestSession(t, t.TempDir(), config.CollabConfig{})
	target := t.TempDir() // never written to — no .plumb/ at all
	if s.targetAllowsCrossProject(target) {
		t.Error("a target with no project config must not be treated as opted in")
	}
}

// TestTargetAllowsCrossProject_UntrustedOptInIsIgnored is the trust-gating half
// finding 8 flagged as unverified: cross_project is a trust-gated key, so a
// project that merely WRITES cross_project = true into its own
// .plumb/config.toml — without that project ever being trusted — must not
// count as consent. Real XDG paths are sandboxed to a temp dir, so "never
// trusted" is simply the untouched default state, exactly as a fresh clone of
// an unknown repository would be.
func TestTargetAllowsCrossProject_UntrustedOptInIsIgnored(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	s := newIntentTestSession(t, t.TempDir(), config.CollabConfig{})
	target := t.TempDir()
	writeProjectConfig(t, target, "[collab]\ncross_project = true\n")
	if s.targetAllowsCrossProject(target) {
		t.Error("an untrusted project's own cross_project = true must be ignored, not honoured")
	}
}

// TestResolvePeer_BindsOneLiveSessionAndRefusesToGuess. Resolving a name is what
// decides both where a message is stored and which session it is bound to, so it
// must report the peer's ID when exactly one live session answers — and report
// nothing when none or several do, leaving the message addressed by name alone
// rather than bound to a coin toss that would deliver it to the wrong agent
// while telling the sender it reached the right one.
func TestResolvePeer_BindsOneLiveSessionAndRefusesToGuess(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	s := newIntentTestSession(t, t.TempDir(), config.CollabConfig{Mailbox: true})

	writeLiveSessionFile(t, session.Info{ID: "id-alice", Name: "alice", Folder: "/proj/a"})
	writeLiveSessionFile(t, session.Info{ID: "id-bob", Name: "bob", Folder: "/proj/b"})

	peer, ok := s.resolvePeer("alice")
	if !ok || peer.ID != "id-alice" || peer.Workspace != "/proj/a" {
		t.Fatalf("resolvePeer(alice) = %+v ok=%v, want id-alice at /proj/a", peer, ok)
	}
	if _, ok := s.resolvePeer("nobody"); ok {
		t.Error("a name no live session holds must not resolve")
	}

	// A second live file under the same name: ambiguous, so unresolvable.
	writeLiveSessionFile(t, session.Info{ID: "id-impostor", Name: "alice", Folder: "/proj/evil"})
	if peer, ok := s.resolvePeer("alice"); ok {
		t.Errorf("an ambiguous name resolved to %+v — binding must never be a guess", peer)
	}
}
