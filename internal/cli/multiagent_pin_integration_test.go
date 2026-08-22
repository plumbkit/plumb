//go:build integration

package cli

// multiagent_pin_integration_test.go — PLAN-375's acceptance harness for the
// topology plumb is actually deployed into: a coordinator agent and N subagents
// multiplexed over ONE MCP connection (Claude Code spawns subagents that inherit
// the parent's single `plumb serve`), each declaring itself with
// session_start.session_id, none of them able to inject a per-call `_meta`
// identity.
//
// PLAN-286 built per-agent state (shards, inverted sticky guard) and PLAN-300
// closed the restore-path widening. What neither proved is the end-to-end claim
// this file exists to pin: that N concurrent subagents get ZERO pin-guard
// refusals for legitimate calls, that each one's calls resolve to the workspace
// it is entitled to, that a cross-workspace re-pin is still REFUSED with an
// actionable remedy (the #181/#182 fail-open class must stay closed), and that
// every agent's writes are visible to workspace_sessions' feed.
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
	if !m.s.isShared() {
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
	if err := m.sessionStart(t, map[string]any{"workspace": ws, "session_id": "subagent-1"}); err != nil {
		t.Fatalf("legitimate subagent: %v", err)
	}

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
	// One subagent asking for a project of its own is not an attack on the
	// connection: refusing it must not mark the shared session blocked, which
	// would raise a dashboard alert against the coordinator for a peer's call.
	if health, msg := sessionHealth(t, m.s.sessID); health == "blocked" {
		t.Errorf("a per-agent refusal flagged the whole connection blocked: %s", msg)
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
