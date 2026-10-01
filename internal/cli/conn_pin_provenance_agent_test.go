package cli

// conn_pin_provenance_agent_test.go — the provenance of the pin that RESOLVED a
// call (issue #529, item 2).
//
// daemon_info and the workspace-boundary refusal both quote how, when and from
// where "the" pin was set. On a shared connection there are several: an agent
// that re-pinned its own shard resolves against that, but both surfaces read the
// connection's, so the agent was told its pin was set long ago by session_start
// — the connection's — while the one it had set minutes earlier went unmentioned.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/sessionstate"
	"github.com/plumbkit/plumb/internal/tools"
)

// provenanceFixture is a shared connection whose pin came from the client's
// roots, with one agent that re-pinned its own shard by session_start and one
// that never did.
type provenanceFixture struct {
	s                     *connSession
	store                 *config.Store
	ss                    *sessionstate.Store
	rootA, rootB, outside string
	ctxOwn, ctxFollower   context.Context
	connectionProvenance  tools.PinProvenance
}

func newProvenanceFixture(t *testing.T) *provenanceFixture {
	t.Helper()
	store, ss := newOriginStore(t)
	f := &provenanceFixture{store: store, ss: ss, rootA: freshTempDir(t), rootB: freshTempDir(t), outside: freshTempDir(t)}
	mustGitDir(t, f.rootA)
	mustGitDir(t, f.rootB)
	f.ctxOwn = mcp.WithLogicalAgent(context.Background(), "own")
	f.ctxFollower = mcp.WithLogicalAgent(context.Background(), "follower")

	f.s = newPersistSession(t, store, ss, "proxy-provenance")
	f.s.attachWorkspace(context.Background(), "file://"+f.rootA) // origin: roots
	f.s.recordLogicalAgentAttach("own")
	f.s.recordLogicalAgentAttach("follower")
	_ = f.s.workspaceFor(f.ctxFollower) // seed the follower's shard from the connection

	if _, err := f.s.repinWorkspace(f.ctxOwn, f.rootB, "", false, false); err != nil {
		t.Fatalf("the agent's own session_start: %v", err)
	}
	f.connectionProvenance = f.s.pinProvenance()
	return f
}

// TestPinProvenanceFor_ReportsThePinThatResolvedTheCall: the agent that re-pinned
// itself is told about THAT pin, and everyone else about the connection's.
func TestPinProvenanceFor_ReportsThePinThatResolvedTheCall(t *testing.T) {
	f := newProvenanceFixture(t)

	if got := f.connectionProvenance.Source; got != "roots" {
		t.Fatalf("precondition: the connection's pin came via %q, want roots", got)
	}
	own := f.s.pinProvenanceFor(f.ctxOwn)
	if own.Source != "session_start" {
		t.Errorf("the agent's provenance source = %q, want session_start: it re-pinned its own shard, but was told the connection's (%q)",
			own.Source, f.connectionProvenance.Source)
	}
	if own.Previous != f.rootA {
		t.Errorf("the agent's provenance previous = %q, want %q (what its own move left)", own.Previous, f.rootA)
	}
	if own.At.IsZero() {
		t.Error("the agent's provenance carries no time: its own move must be stamped")
	}

	follower := f.s.pinProvenanceFor(f.ctxFollower)
	if follower.Source != "roots" {
		t.Errorf("a shard that follows the connection reports %q, want the connection's roots", follower.Source)
	}
	if got := f.s.pinProvenanceFor(context.Background()); got.Source != "roots" {
		t.Errorf("an unattributed call reports %q, want the connection's roots", got.Source)
	}
}

// TestDaemonInfo_QuotesTheCallersOwnPinProvenance goes through the real tool.
func TestDaemonInfo_QuotesTheCallersOwnPinProvenance(t *testing.T) {
	f := newProvenanceFixture(t)
	info := tools.NewDaemonInfoFunc(func() string { return "id" }, func() string { return "name" }, "v0", time.Now()).
		WithPinProvenanceFor(f.s.pinProvenanceFor)

	own, err := info.Execute(f.ctxOwn, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(own, "via session_start") || !strings.Contains(own, "previously "+f.rootA) {
		t.Errorf("the re-pinned agent's daemon_info does not describe its own pin:\n%s", own)
	}
	if strings.Contains(own, "via roots") {
		t.Errorf("the re-pinned agent's daemon_info quotes the connection's pin:\n%s", own)
	}

	follower, err := info.Execute(f.ctxFollower, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(follower, "via roots") || strings.Contains(follower, "via session_start") {
		t.Errorf("a following agent's daemon_info must describe the connection's pin:\n%s", follower)
	}
}

// TestBoundaryRefusal_QuotesTheCallersOwnPinProvenance: the same two surfaces'
// other half, the refusal an agent meets when a path leaves its workspace.
func TestBoundaryRefusal_QuotesTheCallersOwnPinProvenance(t *testing.T) {
	f := newProvenanceFixture(t)
	path := filepath.Join(f.outside, "x.go")

	err := f.s.checkBoundaryFor(f.ctxOwn, path, tools.AccessRead)
	if err == nil {
		t.Fatal("a path outside the agent's workspace was admitted")
	}
	for _, want := range []string{"via session_start", "previously " + f.rootA} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the agent's boundary refusal does not say %q:\n%s", want, err.Error())
		}
	}
	if strings.Contains(err.Error(), "via roots") {
		t.Errorf("the agent's boundary refusal quotes the connection's pin:\n%s", err.Error())
	}

	err = f.s.checkBoundaryFor(f.ctxFollower, path, tools.AccessRead)
	if err == nil || !strings.Contains(err.Error(), "via roots") || strings.Contains(err.Error(), "via session_start") {
		t.Errorf("a following agent's refusal must quote the connection's pin: %v", err)
	}
}

// TestPinProvenanceFor_FollowersTrackTheConnectionAndRestoredShardsSaySo covers
// the other ways a shard comes by its root: dragged with the connection (it takes
// the connection's provenance with it) and restored from its own row (it says it
// was restored, as the connection's own restored pin does).
func TestPinProvenanceFor_FollowersTrackTheConnectionAndRestoredShardsSaySo(t *testing.T) {
	f := newProvenanceFixture(t)
	rootC := freshTempDir(t)
	mustGitDir(t, rootC)

	if _, err := f.s.repinConnection(f.ctxOwn, rootC, "", true); err != nil {
		t.Fatalf("the connection move: %v", err)
	}
	conn := f.s.pinProvenance()
	got := f.s.pinProvenanceFor(f.ctxFollower)
	if got.Source != conn.Source || got.Previous != conn.Previous || got.Forced != conn.Forced || !got.At.Equal(conn.At) {
		t.Errorf("a shard dragged with the connection reports %+v, want the connection's %+v", got, conn)
	}
	if own := f.s.pinProvenanceFor(f.ctxOwn); own.Previous != f.rootA {
		t.Errorf("the agent's own pin lost its provenance in the connection's move: %+v", own)
	}

	f.s.close()
	second := newPersistSession(t, f.store, f.ss, "proxy-provenance")
	second.attachWorkspace(context.Background(), "file://"+f.rootA)
	second.recordLogicalAgentAttach("own")
	second.recordLogicalAgentAttach("follower")
	restored := second.pinProvenanceFor(f.ctxOwn)
	if restored.Source != "restore:session_start" {
		t.Errorf("a restored shard reports source %q, want restore:session_start", restored.Source)
	}
	if restored.Forced || restored.Previous != "" {
		t.Errorf("a restored shard looks like a displacement: %+v", restored)
	}
}

// TestPinProvenanceFor_ConfirmingTheSeededRootUpgradesTheLabelOnly: an agent
// that names the root its shard already holds chose it, but moved nothing, so the
// label upgrades from the connection's roots to session_start while when it was
// set and what it replaced stand — the per-agent counterpart of the connection's
// same-root promotion (TestPinProvenance_SameRootPromotionUpgradesViaOnly).
func TestPinProvenanceFor_ConfirmingTheSeededRootUpgradesTheLabelOnly(t *testing.T) {
	f := newProvenanceFixture(t)

	before := f.s.pinProvenanceFor(f.ctxFollower)
	if before.Source != "roots" {
		t.Fatalf("precondition: the follower's pin came via %q, want roots", before.Source)
	}
	if _, err := f.s.repinWorkspace(f.ctxFollower, f.rootA, "", false, false); err != nil {
		t.Fatalf("the follower naming its seeded root: %v", err)
	}

	after := f.s.pinProvenanceFor(f.ctxFollower)
	if after.Source != "session_start" {
		t.Errorf("a confirmed pin reports %q, want session_start", after.Source)
	}
	if !after.At.Equal(before.At) || after.Previous != before.Previous {
		t.Errorf("confirming moved nothing, yet provenance changed: before %+v, after %+v", before, after)
	}
	err := f.s.checkBoundaryFor(f.ctxFollower, filepath.Join(f.outside, "x.go"), tools.AccessRead)
	if err == nil || !strings.Contains(err.Error(), "via session_start") {
		t.Errorf("the refusal after a confirm still quotes the old label: %v", err)
	}
}

// TestPinProvenanceFor_AnAgentsForcedMoveDoesNotClaimADisplacement: force: true on
// an agent's own re-pin overrides ITS OWN earlier pin, and the connection's
// "Forced" mark means someone else's was taken. Carrying it over made the agent's
// refusal for a path in the root it had just left say the connection was
// force-re-pinned away by another agent — a displacement it did not suffer.
func TestPinProvenanceFor_AnAgentsForcedMoveDoesNotClaimADisplacement(t *testing.T) {
	f := newProvenanceFixture(t)
	rootC := freshTempDir(t)
	mustGitDir(t, rootC)

	if _, err := f.s.repinWorkspace(f.ctxOwn, rootC, "", true, false); err != nil {
		t.Fatalf("the agent's forced move off its own pin: %v", err)
	}

	got := f.s.pinProvenanceFor(f.ctxOwn)
	if got.Previous != f.rootB {
		t.Fatalf("precondition: provenance previous = %q, want the root it left, %q", got.Previous, f.rootB)
	}
	if got.Forced {
		t.Errorf("an agent's forced move off its OWN pin is reported as displacing someone's: %+v", got)
	}
	err := f.s.checkBoundaryFor(f.ctxOwn, filepath.Join(f.rootB, "x.go"), tools.AccessRead)
	if err == nil {
		t.Fatal("a path in the root the agent left was admitted")
	}
	if strings.Contains(err.Error(), "force-re-pinned") {
		t.Errorf("the refusal tells the agent another agent displaced it, when it moved itself:\n%s", err.Error())
	}
}

// TestDaemonInfo_RegisteredToolQuotesTheCallersOwnPinProvenance goes through the
// REAL registration path (registerAllTools), because the accessor is wired there
// and a test that builds its own daemon_info proves only the tool: wiring the
// connection's accessor in place of the per-call one would pass every test above.
func TestDaemonInfo_RegisteredToolQuotesTheCallersOwnPinProvenance(t *testing.T) {
	s, srv := buildTestConnSession(t)
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)
	ctxOwn := mcp.WithLogicalAgent(context.Background(), "own")
	s.attachWorkspace(context.Background(), "file://"+rootA) // origin: roots
	s.recordLogicalAgentAttach("own")
	s.recordLogicalAgentAttach("follower")
	if _, err := s.repinWorkspace(ctxOwn, rootB, "", false, false); err != nil {
		t.Fatalf("the agent's own session_start: %v", err)
	}

	tool, ok := srv.Lookup("daemon_info")
	if !ok {
		t.Fatal("daemon_info is not registered")
	}
	out, err := tool.Execute(ctxOwn, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "via session_start") || strings.Contains(out, "via roots") {
		t.Errorf("the registered daemon_info quotes the connection's pin to an agent that re-pinned itself:\n%s", out)
	}
}
