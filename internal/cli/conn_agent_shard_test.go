package cli

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/tools"
)

// TestAgentRepinIsolation is the headline acceptance for PLAN-286: two logical
// agents multiplexed over one connection re-pin independently, so B's
// session_start does not reset A's pin, and the per-agent sticky guard refuses a
// SAME-agent non-forced re-pin while a different agent's re-pin lands on its own
// shard (the actual issue #182 fix).
func TestAgentRepinIsolation(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	s := newConnSession(context.Background(), detectTestPool(), nil, store, nil, nil, newSharedBudgets())
	t.Cleanup(s.close)

	// Two distinct logical agents share one connection.
	s.recordLogicalAgentAttach("agent-a")
	s.recordLogicalAgentCall("agent-b")

	rootA := freshTempDir(t)
	mustGitDir(t, rootA)
	rootB := freshTempDir(t)
	mustGitDir(t, rootB)
	rootC := freshTempDir(t)
	mustGitDir(t, rootC)

	ctxA := mcp.WithLogicalAgent(context.Background(), "agent-a")
	ctxB := mcp.WithLogicalAgent(context.Background(), "agent-b")

	if _, err := s.repinWorkspace(ctxA, "file://"+rootA, "", false); err != nil {
		t.Fatalf("agent A re-pin: %v", err)
	}
	if _, err := s.repinWorkspace(ctxB, "file://"+rootB, "", false); err != nil {
		t.Fatalf("agent B re-pin: %v", err)
	}
	if got := s.workspaceFor(ctxA); got != rootA {
		t.Fatalf("agent A workspace = %q, want %q — B clobbered A's pin", got, rootA)
	}
	if got := s.workspaceFor(ctxB); got != rootB {
		t.Fatalf("agent B workspace = %q, want %q", got, rootB)
	}

	// A same-agent non-forced re-pin is refused (sticky, inverted guard).
	if _, err := s.repinWorkspace(ctxA, "file://"+rootC, "", false); err == nil {
		t.Fatal("same-agent non-forced re-pin was accepted; the per-agent sticky guard must refuse it")
	}
	if got := s.workspaceFor(ctxA); got != rootA {
		t.Fatalf("refused re-pin moved agent A's pin to %q, want %q", got, rootA)
	}
	// A forced same-agent re-pin lands.
	if _, err := s.repinWorkspace(ctxA, "file://"+rootC, "", true); err != nil {
		t.Fatalf("forced same-agent re-pin: %v", err)
	}
	if got := s.workspaceFor(ctxA); got != rootC {
		t.Fatalf("forced re-pin left agent A at %q, want %q", got, rootC)
	}
	// Agent B is untouched by A's forced move.
	if got := s.workspaceFor(ctxB); got != rootB {
		t.Fatalf("agent B workspace = %q after A moved, want %q", got, rootB)
	}
}

// TestAgentTrackerIsolation pins that the per-agent read trackers are separate:
// one agent's re-pin resets its own trackers, never a peer's.
func TestAgentTrackerIsolation(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	s := newConnSession(context.Background(), detectTestPool(), nil, store, nil, nil, newSharedBudgets())
	t.Cleanup(s.close)

	s.recordLogicalAgentAttach("agent-a")
	s.recordLogicalAgentCall("agent-b")

	ctxA := mcp.WithLogicalAgent(context.Background(), "agent-a")
	ctxB := mcp.WithLogicalAgent(context.Background(), "agent-b")

	rootA := freshTempDir(t)
	mustGitDir(t, rootA)
	rootB := freshTempDir(t)
	mustGitDir(t, rootB)
	if _, err := s.repinWorkspace(ctxA, "file://"+rootA, "", false); err != nil {
		t.Fatalf("agent A re-pin: %v", err)
	}
	if _, err := s.repinWorkspace(ctxB, "file://"+rootB, "", false); err != nil {
		t.Fatalf("agent B re-pin: %v", err)
	}

	// Agent A records a read.
	path := rootA + "/a.go"
	mtime := time.Unix(1_700_000_000, 0)
	s.readTrackerFor(ctxA).Record(path, mtime, "sha-a")

	// Force B to a new root to exercise B's own reset path.
	rootB2 := freshTempDir(t)
	mustGitDir(t, rootB2)
	if _, err := s.repinWorkspace(ctxB, "file://"+rootB2, "", true); err != nil {
		t.Fatalf("agent B forced re-pin: %v", err)
	}
	if got := s.readTrackerFor(ctxA).Mtime(path); !got.Equal(mtime) {
		t.Fatalf("agent B's re-pin reset agent A's read tracker (got %v)", got)
	}
	if got := s.workspaceFor(ctxA); got != rootA {
		t.Fatalf("agent A workspace = %q after B moved, want %q", got, rootA)
	}
	if got := s.workspaceFor(ctxB); got != rootB2 {
		t.Fatalf("agent B workspace = %q, want %q", got, rootB2)
	}
}

// TestWriteDepsRoutePerAgent proves the live tool path resolves per-agent state
// (PLAN-286): buildWriteDeps wires the ctx-aware resolvers, so a read recorded
// through the resolver the tools use lands in that agent's shard tracker, not a
// peer's, and distinct agents get distinct trackers and rate limiters.
func TestWriteDepsRoutePerAgent(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	s := newConnSession(context.Background(), detectTestPool(), nil, store, nil, nil, newSharedBudgets())
	t.Cleanup(s.close)

	s.recordLogicalAgentAttach("agent-a")
	s.recordLogicalAgentCall("agent-b")

	wd := s.buildWriteDeps()
	if wd.ReadsFor == nil || wd.WritesFor == nil || wd.UndoFor == nil || wd.LimiterFor == nil {
		t.Fatal("buildWriteDeps must wire the per-agent resolvers")
	}

	ctxA := mcp.WithLogicalAgent(context.Background(), "agent-a")
	ctxB := mcp.WithLogicalAgent(context.Background(), "agent-b")

	ra, rb := wd.ReadsFor(ctxA), wd.ReadsFor(ctxB)
	if ra == rb {
		t.Fatal("agents must have separate read trackers")
	}
	if wd.LimiterFor(ctxA) == wd.LimiterFor(ctxB) {
		t.Fatal("agents must have separate rate limiters")
	}

	// A read recorded through the resolver the tools use lands only in that
	// agent's tracker, never a peer's.
	ra.Record("/ws/a.go", time.Unix(1_700_000_000, 0), "sha-a")
	if s.readTrackerFor(ctxA).Mtime("/ws/a.go").IsZero() {
		t.Fatal("read recorded via ReadsFor must land in agent A's tracker")
	}
	if !s.readTrackerFor(ctxB).Mtime("/ws/a.go").IsZero() {
		t.Fatal("agent A's read leaked into agent B's tracker")
	}
}

// TestAgentRepinRefusalLogsATrace pins the ONE trace a refused per-agent re-pin
// leaves: a Warn in the daemon log, carrying the agent id, both roots and the
// remedy. The connection-level guard has always recorded a refused steal, and a
// refused cross-workspace drift on a shared connection is exactly what an
// operator needs to find on this past-vulnerability surface — going silent when
// the refusal moved per-agent would have been a regression.
//
// The log is deliberately the whole trace; repinAgent's doc comment says why a
// session health note is not usable here.
func TestAgentRepinRefusalLogsATrace(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	s := newConnSession(context.Background(), detectTestPool(), nil, store, nil, nil, newSharedBudgets())
	t.Cleanup(s.close)
	var logs bytes.Buffer
	s.logger = slog.New(slog.NewTextHandler(&logs, nil))

	s.recordLogicalAgentCall("agent-a")
	s.recordLogicalAgentCall("agent-b")
	ctxA := mcp.WithLogicalAgent(context.Background(), "agent-a")

	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)
	if _, err := s.repinWorkspace(ctxA, "file://"+rootA, "", false); err != nil {
		t.Fatalf("agent A first pin: %v", err)
	}
	logs.Reset() // only the refusal below is under test

	if _, err := s.repinWorkspace(ctxA, "file://"+rootB, "", false); err == nil {
		t.Fatal("precondition: the same-agent non-forced re-pin should have been refused")
	}
	got := logs.String()
	if !strings.Contains(got, "per-agent session_start re-pin refused") {
		t.Fatalf("a refused per-agent re-pin left no trace in the daemon log:\n%s", got)
	}
	if !strings.Contains(got, "level=WARN") {
		t.Errorf("the refusal trace is not logged at Warn, so it will not surface in an ordinary log scan:\n%s", got)
	}
	for _, want := range []string{"agent-a", rootA, rootB, "force: true"} {
		if !strings.Contains(got, want) {
			t.Errorf("the refusal trace does not name %q, so an operator cannot act on it:\n%s", want, got)
		}
	}

	// A re-pin that LANDS is not an incident and must not log the refusal line.
	logs.Reset()
	if _, err := s.repinWorkspace(ctxA, "file://"+rootB, "", true); err != nil {
		t.Fatalf("forced re-pin: %v", err)
	}
	if strings.Contains(logs.String(), "per-agent session_start re-pin refused") {
		t.Errorf("a successful re-pin logged the refusal trace:\n%s", logs.String())
	}
}

// TestRefusedRepinCommitsNoIdentity guards the other half of "a refused call
// commits nothing". Routing a call to its own agent must not write that agent
// into the observed identity set, because `seen` only grows: a single typo'd
// workspace would otherwise flip the connection into per-agent keying forever,
// and every PEER's next call would land on a fresh shard with an empty read
// tracker — reads recorded under the connection key before the typo, so strict
// mode starts refusing their edits with "has not been read".
func TestRefusedRepinCommitsNoIdentity(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	s := newConnSession(context.Background(), detectTestPool(), nil, store, nil, nil, newSharedBudgets())
	t.Cleanup(s.close)

	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)

	// One agent has attached and pinned; a peer recorded a read against the
	// connection's tracker, as every call on an unshared connection does.
	s.recordLogicalAgentAttach("coordinator")
	if _, err := s.repinWorkspace(mcp.WithLogicalAgent(context.Background(), "coordinator"), "file://"+rootA, "", false); err != nil {
		t.Fatalf("coordinator pin: %v", err)
	}
	read := filepath.Join(rootA, "peer.go")
	s.readTrackerFor(context.Background()).Record(read, time.Unix(1_700_000_000, 0), "sha-peer")

	// A second agent declares itself and asks for a project outside the pin. The
	// ctx is derived through declaredAgentCtx — the real channel session_start
	// uses — because what is under test is precisely whether THAT step writes the
	// declaration down. Building the ctx with mcp.WithLogicalAgent directly would
	// bypass the mechanism and pass no matter what it does.
	ctxB := s.declaredAgentCtx(context.Background(), "drifter")
	if _, err := s.repinWorkspace(ctxB, "file://"+rootB, "", false); err == nil {
		t.Fatal("precondition: the cross-workspace re-pin should have been refused")
	}

	if committedShared(s) {
		t.Error("a REFUSED session_start permanently flipped the connection into per-agent keying: " +
			"every peer's next call now lands on a fresh shard")
	}
	if s.readTrackerFor(context.Background()).Mtime(read).IsZero() {
		t.Error("a peer's recorded read was lost after an unrelated agent's re-pin was refused — " +
			"strict mode will now refuse that peer's edits with \"has not been read\"")
	}
	if got := s.workspaceFor(context.Background()); got != rootA {
		t.Errorf("unattributed calls resolve to %q after a refused re-pin, want %q", got, rootA)
	}
}

// committedShared reports the connection's COMMITTED sharing state — two or more
// identities actually recorded — as distinct from sharedWith's question, which
// counts the caller of the request in hand. Tests assert the committed state,
// because "did this call write an identity down" is exactly what a refused call
// must answer no to. Spelled out here rather than as a method on connSession: a
// production gate that reads the committed state instead of sharedWith would
// route a lone declaring agent to the connection and a refused one to a shard,
// which is the bug this file exists to keep closed.
func committedShared(s *connSession) bool { return s.logicalAgents.sharedWith("") }

// TestAnonymousCallOnSharedConnectionFailsClosed pins PLAN-394: once two
// identities have committed, a call carrying no per-call identity resolves
// against the CONNECTION's state — its pin, its boundary policy. It must never
// inherit the most recently attached agent's shard: after that agent force-pins
// itself to another project, the inherited root IS the peer's project, the
// inherited boundary admits the peer's paths, and the inherited trackers record
// the call as the peer's work. That was the measured fail-open.
func TestAnonymousCallOnSharedConnectionFailsClosed(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	s := newConnSession(context.Background(), detectTestPool(), nil, store, nil, nil, newSharedBudgets())
	t.Cleanup(s.close)

	rootConn := freshTempDir(t)
	mustGitDir(t, rootConn)
	rootPeer := freshTempDir(t)
	mustGitDir(t, rootPeer)

	// The coordinator attaches and pins the connection's project; the peer
	// attaches LAST and force-pins its own shard elsewhere — the incident's
	// exact shape.
	s.recordLogicalAgentAttach("coordinator")
	if _, err := s.repinWorkspace(mcp.WithLogicalAgent(context.Background(), "coordinator"), "file://"+rootConn, "", false); err != nil {
		t.Fatalf("coordinator pin: %v", err)
	}
	s.recordLogicalAgentAttach("peer")
	if _, err := s.repinWorkspace(mcp.WithLogicalAgent(context.Background(), "peer"), "file://"+rootPeer, "", true); err != nil {
		t.Fatalf("peer forced re-pin: %v", err)
	}
	if !committedShared(s) {
		t.Fatal("precondition: two attached agents must make the connection shared")
	}

	anonymous := context.Background()
	if got := s.workspaceFor(anonymous); got != rootConn {
		t.Errorf("an anonymous call resolves to %q, want the connection pin %q — it inherited the peer's shard", got, rootConn)
	}
	if _, err := s.policyFor(anonymous).Check(filepath.Join(rootPeer, "x.go"), tools.AccessReadWrite); err == nil {
		t.Error("the boundary admits a path in the peer's project for an anonymous call — the inherited shard's policy is the fail-open")
	}
	if err := s.refuseSharedStateChange(anonymous, "write_file", ""); err == nil {
		t.Error("an anonymous state-changing call on a shared connection must be refused")
	}

	// The identified paths are untouched: the peer's shard is still its own.
	if got := s.workspaceFor(mcp.WithLogicalAgent(context.Background(), "peer")); got != rootPeer {
		t.Errorf("peer workspace = %q after its forced re-pin, want %q", got, rootPeer)
	}
	if err := s.refuseSharedStateChange(anonymous, "write_file", "peer"); err != nil {
		t.Errorf("an identified state-changing call must not refuse: %v", err)
	}
}
