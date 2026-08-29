//go:build integration

package cli

// multiagent_pin_integration_test.go — PLAN-375's acceptance harness for the
// topology plumb is actually deployed into: a coordinator agent and N subagents
// multiplexed over ONE MCP connection (Claude Code spawns subagents that inherit
// the parent's single `plumb serve`), each declaring itself with
// session_start.session_id.
//
// READ THIS BEFORE TRUSTING A RESULT HERE — what the harness models, precisely:
//
//   - session_start calls carry NO per-call `_meta` identity. That is the honest
//     model of the target client, and it is the channel the fix is about.
//   - LATER calls (the write_file cases) DO carry one. That is NOT the target
//     client: Claude Code's per-call `_meta` has a tool-use id and a progress
//     token, nothing agent-scoped. Those subtests therefore prove per-agent
//     routing for a client that CAN identify each call — a real supported
//     topology, but not the one the subagents in the story are running.
//
// The gap between the two WAS deliberate and pinned, not overlooked:
// testAnonymousCallsInheritTheLastAttachedAgent recorded what happened when a
// later call carried no identity at all — attribution to whichever agent
// attached LAST — as a KNOWN, TRACKED defect (the shardFor/attachIdentity
// fallback, pre-existing to PLAN-286). PLAN-394 closed it: an anonymous call on
// a shared connection now fails closed to the connection (reads resolve there,
// state-changing calls are refused), and the subtests below pin the closed
// behaviour.
//
// PLAN-286 built per-agent state (shards, inverted sticky guard) and PLAN-300
// closed the restore-path widening. What neither proved is the end-to-end claim
// this file exists to pin: that N concurrent subagents get ZERO pin-guard
// refusals for legitimate calls, that each one's session_start resolves to the
// workspace it is entitled to, that a cross-workspace re-pin is still REFUSED
// with an actionable remedy and without flagging the whole connection blocked
// (the #181/#182 fail-open class must stay closed), and that the connection's
// own pin never moves under any of it. The refusal's daemon-log trace is
// asserted by TestAgentRepinRefusalLogsATrace instead, where the logger can be
// captured.
//
// The harness drives the REAL tool surface — tools.SessionStart and
// tools.WriteFile wired exactly as registerAllTools wires them — because the
// defect this card exists to fix lived in the ORDER the tool did its work, not
// in the daemon API the unit tests call directly.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/tools"
)

// multiAgentConn is one MCP connection with the identity/pin/write surface wired
// the way registerAllTools wires it. One instance == one `plumb serve` shared by
// every logical agent below.
type multiAgentConn struct {
	s     *connSession
	start *tools.SessionStart
	write *tools.WriteFile
}

func newMultiAgentConn(t *testing.T) *multiAgentConn {
	t.Helper()
	store, ss := newOriginStore(t)
	s := newPersistSession(t, store, ss, "proxy-multiagent")
	start := tools.NewSessionStart(s.workspaceFor, nil, nil, nil, func() string { return "" }, nil).
		WithRepin(s.repinWorkspace).
		WithDeclaredAgent(s.declaredAgentCtx).
		WithExternalID(func(id string) string {
			session.SetExternalID(s.sessionID(), id)
			s.recordLogicalAgentAttach(id)
			return ""
		})
	return &multiAgentConn{s: s, start: start, write: tools.NewWriteFile(s.buildWriteDeps())}
}

// call dispatches one tool the way internal/mcp's tools/call handler does:
// derive the per-call ctx from the (absent) _meta identity, run the OnBeforeTool
// hook, then Execute. Going through the hook matters — it is what attaches an
// unpinned connection from the call's own `workspace` argument, so skipping it
// would test a path no client ever takes.
func (m *multiAgentConn) call(t *testing.T, metaAgent, name string, args map[string]any, exec func(context.Context, json.RawMessage) (string, error)) error {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal %s args: %v", name, err)
	}
	ctx := mcp.WithLogicalAgent(context.Background(), metaAgent)
	if err := m.s.refuseSharedStateChange(ctx, name, metaAgent); err != nil {
		return err
	}
	m.s.recordLogicalAgentCall(metaAgent)
	m.s.onBeforeTool(ctx, name, raw)
	_, err = exec(ctx, raw)
	return err
}

// sessionStart calls the real session_start tool as a Claude Code subagent does:
// a session_id argument and NO per-call _meta identity, so the ONLY channel the
// daemon can learn this agent's identity from is the very call that also names
// its workspace.
func (m *multiAgentConn) sessionStart(t *testing.T, args map[string]any) error {
	t.Helper()
	return m.call(t, "", "session_start", args, m.start.Execute)
}

// writeFile calls the real write_file tool for an agent that has already
// declared itself, i.e. with the per-call identity a later call carries.
func (m *multiAgentConn) writeFile(t *testing.T, agentID, path, content string) error {
	t.Helper()
	return m.call(t, agentID, "write_file", map[string]any{"file_path": path, "content": content}, m.write.Execute)
}

// agentCtx is the ctx a LATER (non-session_start) tool call from agentID runs
// under once that agent has declared itself. A client that can inject per-call
// _meta produces exactly this; the session_start path derives it from session_id.
func agentCtx(agentID string) context.Context {
	return mcp.WithLogicalAgent(context.Background(), agentID)
}

// TestMultiAgentPin is the card's literal acceptance harness. Run with
//
//	go test -tags=integration ./internal/... -run 'TestMultiAgentPin' -v
func TestMultiAgentPin(t *testing.T) {
	t.Run("SubagentIdentifiedInTheSameCallIsNotRefused", testSubagentSameCallIdentity)
	t.Run("ConcurrentSubagentsZeroRefusals", testConcurrentSubagentsZeroRefusals)
	t.Run("CrossWorkspaceSubagentRefusedWithRemedy", testCrossWorkspaceRefusedWithRemedy)
	t.Run("AnonymousStateChangeStillRefused", testAnonymousStateChangeStillRefused)
	t.Run("EveryAgentWriteIsAttributedAndVisible", testEveryAgentWriteVisible)
	t.Run("AnonymousCallOnSharedConnectionFailsClosed", testAnonymousCallOnSharedConnectionFailsClosed)
	t.Run("AnonymousWriteIntoPeerProjectRefused", testAnonymousWriteIntoPeerProjectRefused)
	t.Run("SingleAgentConnectionStillPinsTheConnection", testSingleAgentConnectionStillPinsTheConnection)
	t.Run("UndeclaredAgentsForcePingPongIsContested", testUndeclaredAgentsForcePingPongIsContested)
}

// testUndeclaredAgentsForcePingPongIsContested replays the topology this whole
// file models with ONE detail removed: the agents declare no session_id at all.
//
// That is not a hypothetical client. It is the one the incident happened on —
// its session record carries no external_id — and it is the case every other
// subtest here is blind to, because each of them passes a session_id and so gets
// PLAN-286's per-agent shards. With no identity on either channel,
// logicalAgentState.seen stays EMPTY: sharedWith is false, no shard is created,
// no shared-connection warning fires, and the anonymous-write ceiling never
// engages (it needs two observed identities). Every #182 defence is inert, and
// the two agents fight over the connection-level pin.
//
// What plumb can still see is the SHAPE, and this pins that it does: a pin
// force-taken between two projects twice makes the connection contested, and
// from then on the displaced agent is TOLD its workspace was taken instead of
// being handed the force advice that caused the taking.
func testUndeclaredAgentsForcePingPongIsContested(t *testing.T) {
	m := newMultiAgentConn(t)
	wsA, wsB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, wsA)
	mustGitDir(t, wsB)

	// Agent A takes the connection. No session_id — that is the whole point.
	if err := m.sessionStart(t, map[string]any{"workspace": wsA}); err != nil {
		t.Fatalf("agent A session_start: %v", err)
	}
	if committedShared(m.s) {
		t.Fatal("an undeclared agent must not mark the connection shared; if it does, this subtest no longer models the incident")
	}

	// Agent B is refused — correctly — and the refusal tells it to force.
	err := m.sessionStart(t, map[string]any{"workspace": wsB})
	if err == nil {
		t.Fatal("a second undeclared agent re-pinning across projects must be refused (sticky, issue #182)")
	}
	if !strings.Contains(err.Error(), "force: true") {
		t.Fatalf("the first refusal no longer offers force; the incident's premise has changed: %v", err)
	}

	// B does as it was told. One displacement is still an ordinary switch.
	if err := m.sessionStart(t, map[string]any{"workspace": wsB, "force": true}); err != nil {
		t.Fatalf("agent B forced session_start: %v", err)
	}
	if m.s.pinContested() {
		t.Fatal("a single forced re-pin made the connection contested; that is a project switch")
	}

	// A takes it back — the alternation. THIS is the signal.
	if err := m.sessionStart(t, map[string]any{"workspace": wsA, "force": true}); err != nil {
		t.Fatalf("agent A forced session_start: %v", err)
	}
	if !m.s.pinContested() {
		t.Fatal("two forced alternations between two projects did not make the connection contested")
	}

	// B, now displaced, reaches for a file in its own project. It must learn
	// that the workspace was taken — not merely that the connection points
	// somewhere else, which reads as B's own mistake.
	bErr := m.s.checkBoundary(filepath.Join(wsB, "main.go"), tools.AccessRead)
	if bErr == nil {
		t.Fatal("a file in the displaced project must still be refused; the pin really did move")
	}
	if !strings.Contains(bErr.Error(), "force-re-pinned away from "+wsB) {
		t.Errorf("the displaced agent is not told its workspace was taken: %v", bErr)
	}
	if strings.Contains(bErr.Error(), "retry with force: true") {
		t.Errorf("the displaced agent is still being told to force, which is what produced the ping-pong: %v", bErr)
	}

	// And the next refusal names the real remedy rather than the escalation.
	err = m.sessionStart(t, map[string]any{"workspace": wsB})
	if err == nil {
		t.Fatal("the sticky guard stopped refusing once contested; this must change advice, never permission")
	}
	if !strings.Contains(err.Error(), "Identify each agent instead") {
		t.Errorf("the contested refusal does not name the real remedy: %v", err)
	}

	// Forcing still WORKS. plumb cannot know which undeclared agent is entitled
	// to the workspace, and refusing outright would strand real work.
	if err := m.sessionStart(t, map[string]any{"workspace": wsB, "force": true}); err != nil {
		t.Fatalf("force stopped working on a contested connection; that is a behaviour change, not a message change: %v", err)
	}
}

// testSubagentSameCallIdentity is the headline regression. A subagent's FIRST
// contact is session_start({workspace, session_id}) — identity and workspace in
// one call. Resolving the workspace before recording the identity made that call
// unattributable at the moment the sticky-pin guard ran, so the guard refused it
// as a peer stealing the coordinator's pin. That refusal is the observed
// abandonment: a coordinator that tells six subagents not to touch plumb.
func testSubagentSameCallIdentity(t *testing.T) {
	m := newMultiAgentConn(t)
	ws := freshTempDir(t)
	mustGitDir(t, ws)

	// The coordinator pins the shared workspace first — the ordinary case, and
	// the pin every subagent below must inherit rather than fight.
	if err := m.sessionStart(t, map[string]any{"workspace": ws, "session_id": "coordinator"}); err != nil {
		t.Fatalf("coordinator session_start: %v", err)
	}
	if got := m.s.workspace(); got != ws {
		t.Fatalf("connection workspace = %q, want %q", got, ws)
	}

	// The subagent names the SAME workspace it was told to work in.
	if err := m.sessionStart(t, map[string]any{"workspace": ws, "session_id": "subagent-1"}); err != nil {
		t.Fatalf("subagent session_start refused — this is the #182 pin fight PLAN-375 exists to close: %v", err)
	}
	if got := m.s.workspaceFor(agentCtx("subagent-1")); got != ws {
		t.Fatalf("subagent workspace = %q, want %q", got, ws)
	}
	if got := m.s.workspaceFor(agentCtx("coordinator")); got != ws {
		t.Fatalf("coordinator workspace = %q after the subagent attached, want %q", got, ws)
	}
	if !committedShared(m.s) {
		t.Fatal("two declared session_ids must mark the connection shared, so per-agent keying is in effect")
	}
	if health, msg := sessionHealth(t, m.s.sessID); health == "blocked" {
		t.Fatalf("a legitimate subagent attach flagged the session blocked: %s", msg)
	}
}

// testConcurrentSubagentsZeroRefusals runs N=5 subagents CONCURRENTLY over the
// one connection, mixing the argument shapes a real fan-out produces: the
// workspace named exactly, a subdirectory of it, no workspace at all (pure
// re-orientation), and a repeat call. Every one is legitimate, so every one must
// land — concurrently, with no serialisation-dependent refusal.
func testConcurrentSubagentsZeroRefusals(t *testing.T) {
	m := newMultiAgentConn(t)
	ws := freshTempDir(t)
	mustGitDir(t, ws)
	sub := filepath.Join(ws, "internal", "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}
	if err := m.sessionStart(t, map[string]any{"workspace": ws, "session_id": "coordinator"}); err != nil {
		t.Fatalf("coordinator session_start: %v", err)
	}

	cases := []struct {
		id   string
		args map[string]any
	}{
		{"subagent-1", map[string]any{"workspace": ws, "session_id": "subagent-1"}},
		{"subagent-2", map[string]any{"session_id": "subagent-2"}},
		{"subagent-3", map[string]any{"workspace": sub, "session_id": "subagent-3"}},
		{"subagent-4", map[string]any{"workspace": ws, "session_id": "subagent-4", "detail": "brief"}},
		{"subagent-5", map[string]any{"workspace": ws, "session_id": "subagent-5"}},
	}

	errs := make([]error, len(cases))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, c := range cases {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // maximise the overlap on the re-pin lane
			errs[i] = m.sessionStart(t, c.args)
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("%s: refused a legitimate call: %v", cases[i].id, err)
		}
	}
	// Every agent — including the two that never named a workspace or named only
	// a subdirectory — resolves to the shared root.
	for _, c := range cases {
		if got := m.s.workspaceFor(agentCtx(c.id)); got != ws {
			t.Errorf("%s workspace = %q, want %q", c.id, got, ws)
		}
	}
	if got := m.s.workspaceFor(agentCtx("coordinator")); got != ws {
		t.Errorf("coordinator workspace = %q after 5 concurrent subagents, want %q", got, ws)
	}
	if got := m.s.workspace(); got != ws {
		t.Errorf("connection-level pin = %q, want %q — no subagent may move it", got, ws)
	}
}

// testCrossWorkspaceRefusedWithRemedy is the malicious-drift half: per-agent
// isolation must not become a licence to silently reach into a project the
// coordinator never chose. A subagent that names a DIFFERENT workspace is
// refused, the refusal names the remedy, and no agent's write surface widens —
// this is the #181/#182 fail-open class staying closed.
func testCrossWorkspaceRefusedWithRemedy(t *testing.T) {
	m := newMultiAgentConn(t)
	ws := freshTempDir(t)
	mustGitDir(t, ws)
	other := freshTempDir(t)
	mustGitDir(t, other)

	if err := m.sessionStart(t, map[string]any{"workspace": ws, "session_id": "coordinator"}); err != nil {
		t.Fatalf("coordinator session_start: %v", err)
	}

	// The drifter is deliberately the SECOND agent to declare itself, so this is
	// the call that makes the connection shared. Identity has to be settled
	// BEFORE the re-pin for that to be true at the moment the re-pin runs; if it
	// is settled after, this call is still seen as a single-agent connection
	// re-pinning itself and moves the coordinator's pin instead of its own shard.
	err := m.sessionStart(t, map[string]any{"workspace": other, "session_id": "drifter"})
	if err == nil {
		t.Fatal("a subagent re-pinning to a workspace outside the coordinator's project was accepted — the #182 guard is fail-open")
	}
	msg := err.Error()
	for _, want := range []string{"force", "session_start"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal does not name the remedy (missing %q): %s", want, msg)
		}
	}
	// The refusal left every surface where it was.
	if got := m.s.workspaceFor(agentCtx("drifter")); got != ws {
		t.Errorf("refused agent workspace = %q, want the inherited %q — a refused re-pin must not move anything", got, ws)
	}
	if got := m.s.workspaceFor(agentCtx("coordinator")); got != ws {
		t.Errorf("coordinator workspace = %q, want %q", got, ws)
	}
	if _, err := m.s.policyFor(agentCtx("drifter")).Check(filepath.Join(other, "x.go"), tools.AccessReadWrite); err == nil {
		t.Error("the refused agent's boundary admits a path in the workspace it was refused — fail-open")
	}
	// The refusal's durable trace is a Warn in the daemon log, asserted by
	// TestAgentRepinRefusalLogsATrace where the logger can be captured. What this
	// harness owns is the other half: it must NOT mark the shared session
	// "blocked". One subagent asking for a project of its own is a scoping
	// question about that agent, not the connection being unusable, and "blocked"
	// raises a dashboard alert against the coordinator for a peer's call.
	if health, msg := sessionHealth(t, m.s.sessID); health == "blocked" {
		t.Errorf("a per-agent refusal flagged the whole connection blocked: %s", msg)
	}

	// A refused agent never ATTACHED, so its identity was never committed to the
	// observed set — the set sharedWith and refuse route on. Declaring an
	// identity inside a call is a per-call claim; only a call that succeeded
	// commits it. (Before PLAN-394 this was pinned against the attach-time
	// fallback identity; the fallback is gone, and the commitment rule is the
	// part that has to keep holding.)
	if _, committed := m.s.logicalAgents.seen["drifter"]; committed {
		t.Error("a REFUSED re-pin committed the drifter's identity — the connection would route and refuse on a peer that never attached")
	}
	if got := m.s.workspaceFor(context.Background()); got != ws {
		t.Errorf("an unattributed call resolves to %q after a refused cross-workspace re-pin, want %q", got, ws)
	}

	// The remedy actually works, and only for the agent that used it.
	if err := m.sessionStart(t, map[string]any{"workspace": other, "session_id": "drifter", "force": true}); err != nil {
		t.Fatalf("force: true is the named remedy and must land: %v", err)
	}
	if got := m.s.workspaceFor(agentCtx("drifter")); got != other {
		t.Errorf("forced re-pin left the agent at %q, want %q", got, other)
	}
	if got := m.s.workspaceFor(agentCtx("coordinator")); got != ws {
		t.Errorf("a peer's forced re-pin moved the coordinator to %q, want %q", got, ws)
	}
	if got := m.s.workspace(); got != ws {
		t.Errorf("a peer's forced re-pin moved the CONNECTION pin to %q, want %q", got, ws)
	}
}

// testAnonymousStateChangeStillRefused pins the fail-closed ceiling PLAN-286
// shipped: a shared connection whose agents identified themselves ONLY through
// per-call _meta cannot attribute an anonymous mutating call, and refuses it
// naming the topology and the remedy. Per-agent isolation must not have turned
// that refusal off.
func testAnonymousStateChangeStillRefused(t *testing.T) {
	m := newMultiAgentConn(t)
	m.s.recordLogicalAgentCall("agent-a")
	m.s.recordLogicalAgentCall("agent-b")

	err := m.s.refuseSharedStateChange(context.Background(), "write_file", "")
	if err == nil {
		t.Fatal("an anonymous state-changing call on a shared connection must be refused")
	}
	for _, want := range []string{"one plumb serve per logical agent", mcp.MetaLogicalAgentKey, "session_start.session_id"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q: %s", want, err)
		}
	}
	// An identified call on the same connection is not refused.
	if err := m.s.refuseSharedStateChange(context.Background(), "write_file", "agent-a"); err != nil {
		t.Errorf("an identified state-changing call must not be refused: %v", err)
	}
	// Reads are never refused: sharing read-only state is safe.
	if err := m.s.refuseSharedStateChange(context.Background(), "read_file", ""); err != nil {
		t.Errorf("a read must not be refused: %v", err)
	}
}

// testEveryAgentWriteVisible closes the loop the card asks for: with N agents
// multiplexed, every agent's write must actually land through the real write
// tool, be recorded against THAT agent's own trackers, and stay inside the
// workspace it is pinned to.
func testEveryAgentWriteVisible(t *testing.T) {
	m := newMultiAgentConn(t)
	ws := freshTempDir(t)
	mustGitDir(t, ws)
	if err := m.sessionStart(t, map[string]any{"workspace": ws, "session_id": "coordinator"}); err != nil {
		t.Fatalf("coordinator session_start: %v", err)
	}

	agents := []string{"coordinator", "subagent-1", "subagent-2", "subagent-3", "subagent-4"}
	for _, id := range agents[1:] {
		if err := m.sessionStart(t, map[string]any{"workspace": ws, "session_id": id}); err != nil {
			t.Fatalf("%s session_start: %v", id, err)
		}
	}

	for _, id := range agents {
		path := filepath.Join(ws, id+".txt")
		if err := m.writeFile(t, id, path, id+"\n"); err != nil {
			t.Fatalf("%s write_file: %v", id, err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s write did not land: %v", id, err)
		}
	}

	// Each write is attributed to its own agent: the writer sees it, and no peer
	// does. A shared tracker would make every agent see all five.
	for _, id := range agents {
		path := filepath.Join(ws, id+".txt")
		if !m.s.writeTrackerFor(agentCtx(id)).Wrote(path) {
			t.Errorf("%s's own write is missing from its write tracker", id)
		}
		for _, peer := range agents {
			if peer == id {
				continue
			}
			if m.s.writeTrackerFor(agentCtx(peer)).Wrote(path) {
				t.Errorf("%s's write leaked into %s's tracker", id, peer)
			}
		}
	}
}

// testAnonymousCallsInheritTheLastAttachedAgent PINS A KNOWN DEFECT, on purpose.
// It asserts what plumb does today, not what it should do, so the gap cannot
// widen or regress unnoticed — and so that closing it turns exactly this subtest
// red, which is the signal to delete it.
//
// The defect: shardFor falls back to logicalAgents.attachIdentity() — the MOST
// RECENTLY attached session_id — for a call carrying no per-call `_meta`. Every
// agent on a shared connection that cannot inject `_meta` therefore has its
// later calls attributed to whichever peer attached last: reads and writes are
// recorded against that peer's trackers, and its workspace and boundary policy
// are the ones resolved. It is pre-existing (PLAN-286 shipped it, this card did
// not introduce it), it is filed as its own card, and it is deliberately NOT
// fixed here.
//
// Why it matters to this file: every OTHER write subtest hands write_file a
// per-call identity, which the target client cannot do. This is what the same
// scenario actually does over the channel that client really has.
// testAnonymousCallOnSharedConnectionFailsClosed is what the retired
// testAnonymousCallsInheritTheLastAttachedAgent used to pin in reverse: on a
// shared connection an anonymous call — the only channel a client that cannot
// inject _meta has — resolves to the CONNECTION, never to the most recently
// attached agent's shard, and its state-changing half is refused outright. The
// old subtest asserted the inheritance as a known gap; PLAN-394 closed the gap.
func testAnonymousCallOnSharedConnectionFailsClosed(t *testing.T) {
	m := newMultiAgentConn(t)
	ws := freshTempDir(t)
	mustGitDir(t, ws)

	if err := m.sessionStart(t, map[string]any{"workspace": ws, "session_id": "coordinator"}); err != nil {
		t.Fatalf("coordinator session_start: %v", err)
	}
	if err := m.sessionStart(t, map[string]any{"workspace": ws, "session_id": "subagent-last"}); err != nil {
		t.Fatalf("subagent session_start: %v", err)
	}

	// The anonymous call resolves to the connection's pin — no peer's shard.
	if got := m.s.workspaceFor(context.Background()); got != ws {
		t.Errorf("anonymous call resolved to workspace %q, want %q", got, ws)
	}

	// Its state-changing half is refused, naming the supported topology and the
	// identity channels that admit it.
	path := filepath.Join(ws, "coordinator-anonymous.txt")
	err := m.writeFile(t, "", path, "coordinator\n")
	if err == nil {
		t.Fatal("an anonymous write on a shared connection must be refused (PLAN-394 closed the inherit-last-attached gap)")
	}
	for _, want := range []string{"one plumb serve per logical agent", mcp.MetaLogicalAgentKey} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q: %s", want, err)
		}
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("the refused anonymous write landed anyway")
	}
	for _, id := range []string{"coordinator", "subagent-last"} {
		if m.s.writeTrackerFor(agentCtx(id)).Wrote(path) {
			t.Errorf("the refused anonymous write leaked into %s's tracker", id)
		}
	}

	// Reads stay open, and an identified call lands on its own shard.
	if err := m.s.refuseSharedStateChange(context.Background(), "read_file", ""); err != nil {
		t.Errorf("a read must never be refused: %v", err)
	}
	if err := m.writeFile(t, "coordinator", filepath.Join(ws, "coordinator-identified.txt"), "coordinator\n"); err != nil {
		t.Errorf("an identified write must land: %v", err)
	}
}

// testAnonymousWriteIntoPeerProjectRefused is the incident the old gap produced
// while it was open: the peer attaches last and force-pins its own shard to a
// DIFFERENT project, and the anonymous call inherits that shard — so the
// unattributable call resolves to the peer's project and the peer's boundary
// admits the peer's paths. Failing closed to the connection is what keeps the
// write out.
func testAnonymousWriteIntoPeerProjectRefused(t *testing.T) {
	m := newMultiAgentConn(t)
	ws := freshTempDir(t)
	mustGitDir(t, ws)
	peer := freshTempDir(t)
	mustGitDir(t, peer)

	if err := m.sessionStart(t, map[string]any{"workspace": ws, "session_id": "coordinator"}); err != nil {
		t.Fatalf("coordinator session_start: %v", err)
	}
	if err := m.sessionStart(t, map[string]any{"workspace": peer, "session_id": "subagent-last", "force": true}); err != nil {
		t.Fatalf("peer forced session_start: %v", err)
	}

	if got := m.s.workspaceFor(context.Background()); got != ws {
		t.Errorf("an anonymous call resolves to %q, want the connection pin %q — it inherited the peer's shard", got, ws)
	}
	if err := m.s.refuseSharedStateChange(context.Background(), "write_file", ""); err == nil {
		t.Fatal("an anonymous write into the peer's project must be refused")
	}
	victim := filepath.Join(peer, "victim.txt")
	if _, err := m.s.policyFor(context.Background()).Check(victim, tools.AccessReadWrite); err == nil {
		t.Error("the anonymous call's boundary admits a path in the peer's project — fail-open")
	}
	if m.s.writeTrackerFor(agentCtx("subagent-last")).Wrote(victim) {
		t.Error("the anonymous write was recorded as the peer's work")
	}
}

// testSingleAgentConnectionStillPinsTheConnection guards the hot path this whole
// change promises not to disturb: ONE agent over one connection must keep
// pinning the CONNECTION, never a shard. Per-agent keying is a shared-connection
// affair — it is what makes a peer's re-pin harmless — and switching it on for a
// lone agent would leave the connection pin frozen at whatever it first
// resolved, so every unattributed call (background goroutines, the roots ladder,
// any client that sends no identity) would keep resolving to the stale root
// while the agent believed it had moved.
func testSingleAgentConnectionStillPinsTheConnection(t *testing.T) {
	m := newMultiAgentConn(t)
	first, second := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, first)
	mustGitDir(t, second)

	if err := m.sessionStart(t, map[string]any{"workspace": first, "session_id": "solo"}); err != nil {
		t.Fatalf("first session_start: %v", err)
	}
	if got := m.s.workspace(); got != first {
		t.Fatalf("connection pin = %q, want %q", got, first)
	}
	if committedShared(m.s) {
		t.Fatal("one agent must not mark the connection shared")
	}

	// A re-orientation naming the same root changes nothing.
	if err := m.sessionStart(t, map[string]any{"workspace": first, "session_id": "solo"}); err != nil {
		t.Fatalf("re-orientation: %v", err)
	}
	if got := m.s.workspace(); got != first {
		t.Errorf("connection pin = %q after a same-root call, want %q", got, first)
	}

	// A deliberate switch moves the CONNECTION, which is what a lone agent's
	// session_start has always done.
	if err := m.sessionStart(t, map[string]any{"workspace": second, "session_id": "solo", "force": true}); err != nil {
		t.Fatalf("forced switch: %v", err)
	}
	if got := m.s.workspace(); got != second {
		t.Errorf("connection pin = %q after a lone agent's deliberate switch, want %q — "+
			"the switch landed on a shard, so every unattributed call still resolves to the old root",
			got, second)
	}
	if got := m.s.workspaceFor(context.Background()); got != second {
		t.Errorf("unattributed calls resolve to %q, want %q", got, second)
	}
}
