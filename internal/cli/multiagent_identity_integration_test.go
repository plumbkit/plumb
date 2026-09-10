//go:build integration

package cli

// multiagent_identity_integration_test.go — the argument-carried identity
// channel end to end, on the topology Claude Code actually runs: a parent
// conversation and its subagents multiplexed over ONE `plumb serve`, every
// call stamped by the PreToolUse hook with `dev.plumbkit/logical-agent` inside
// `arguments` and NO `_meta` identity at all. Unlike the harness in
// multiagent_pin_integration_test.go, calls here go through a real mcp.Server,
// because the strip lives in its dispatch path and a harness that bypassed it
// would prove nothing about the seam that matters.
//
// Run with
//
//	go test -tags=integration ./internal/cli/ -run 'TestHookStampedIdentity' -v

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/tools"
)

// stampedConn is one connection whose tool surface is served by a real
// mcp.Server wired the way registerAllTools wires the identity seams.
type stampedConn struct {
	s   *connSession
	srv *mcp.Server
	id  int
}

func newStampedConn(t *testing.T) *stampedConn {
	t.Helper()
	store, ss := newOriginStore(t)
	s := newPersistSession(t, store, ss, "proxy-stamped")
	srv := mcp.New(mcp.ServerInfo{Name: "test", Version: "0"})
	srv.Register(tools.NewSessionStart(s.workspaceFor, nil, nil, nil, func() string { return "" }, nil).
		WithRepin(s.repinWorkspace).
		WithDeclaredAgent(s.declaredAgentCtx).
		WithExternalID(s.linkExternalID))
	srv.Register(tools.NewWriteFile(s.buildWriteDeps()))
	srv.Register(tools.NewReadFile(s.readTracker).WithReadsFor(s.readTrackerFor).WithWorkspace(s.workspaceFor))
	srv.OnToolRefusal = s.refuseSharedStateChange
	srv.OnBeforeTool = func(ctx context.Context, name string, args json.RawMessage, agent string) {
		s.recordLogicalAgentCall(agent)
		s.onBeforeTool(ctx, name, args)
	}
	return &stampedConn{s: s, srv: srv}
}

// call sends one tools/call frame carrying the reserved argument and no _meta,
// exactly as the hook-stamped client does, and returns the result text and
// whether the server flagged an error.
func (c *stampedConn) call(t *testing.T, agent, name string, args map[string]any) (string, bool) {
	t.Helper()
	if agent != "" {
		args[mcp.ArgLogicalAgentKey] = agent
	}
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal %s args: %v", name, err)
	}
	c.id++
	frame := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, c.id, name, raw)
	var out bytes.Buffer
	if err := c.srv.Serve(context.Background(), strings.NewReader(frame+"\n"), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	var resp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("decode response %s: %v", out.String(), err)
	}
	if resp.Error != nil {
		return resp.Error.Message, true
	}
	text := ""
	if len(resp.Result.Content) > 0 {
		text = resp.Result.Content[0].Text
	}
	return text, resp.Result.IsError
}

func TestHookStampedIdentity(t *testing.T) {
	t.Run("ParentAndSubagentWriteWithZeroRefusals", testHookStampedParentAndSubagentWrite)
	t.Run("SubagentCannotStealTheParentPin", testHookStampedSubagentCannotStealPin)
	t.Run("ParentStrictReadsSurviveTheFirstSubagent", testHookStampedParentReadsSurvive)
}

// testHookStampedParentAndSubagentWrite is the scenario the channel exists
// for: before it, the parent's first write after a subagent declared itself
// was refused as unattributable for the connection's whole life.
func testHookStampedParentAndSubagentWrite(t *testing.T) {
	c := newStampedConn(t)
	ws := freshTempDir(t)
	mustGitDir(t, ws)
	const conv, sub = "conv-1", "conv-1/agent-7"

	if text, isErr := c.call(t, conv, "session_start", map[string]any{"workspace": ws, "session_id": conv}); isErr {
		t.Fatalf("parent session_start refused: %s", text)
	}
	if text, isErr := c.call(t, sub, "session_start", map[string]any{"workspace": ws, "session_id": sub}); isErr {
		t.Fatalf("subagent session_start refused: %s", text)
	}
	for _, agent := range []string{conv, sub} {
		path := filepath.Join(ws, strings.ReplaceAll(agent, "/", "_")+".txt")
		if text, isErr := c.call(t, agent, "write_file", map[string]any{"file_path": path, "content": agent + "\n"}); isErr {
			t.Fatalf("%s write_file refused on a hook-stamped connection: %s", agent, text)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s write did not land: %v", agent, err)
		}
		if !c.s.writeTrackerFor(agentCtx(agent)).Wrote(path) {
			t.Errorf("%s's write is missing from its own tracker", agent)
		}
	}
	if c.s.writeTrackerFor(agentCtx(sub)).Wrote(filepath.Join(ws, "conv-1.txt")) {
		t.Error("the parent's write leaked into the subagent's tracker")
	}
	if got := c.s.workspaceFor(context.Background()); got != ws {
		t.Errorf("connection pin moved to %q", got)
	}
	if !c.s.logicalAgents.sharedWith("") {
		t.Error("two stamped identities must make the connection shared")
	}
	if got := c.s.externalID(); got != conv {
		t.Errorf("session linkage = %q, want the conversation %q", got, conv)
	}
	// The unstamped control: a write with no identity is still refused, so the
	// channel attributes rather than disables the ceiling.
	if text, isErr := c.call(t, "", "write_file", map[string]any{"file_path": filepath.Join(ws, "anon.txt"), "content": "x"}); !isErr || !strings.Contains(text, "no logical-agent identity") {
		t.Fatalf("an unstamped write must still be refused, got isError=%v %q", isErr, text)
	}
}

// testHookStampedSubagentCannotStealPin: the stamp attributes, it does not
// authorise. A subagent naming another project is refused on its own shard
// and the parent's pin is untouched.
func testHookStampedSubagentCannotStealPin(t *testing.T) {
	c := newStampedConn(t)
	ws, other := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, ws)
	mustGitDir(t, other)
	const conv, sub = "conv-2", "conv-2/agent-1"

	if text, isErr := c.call(t, conv, "session_start", map[string]any{"workspace": ws, "session_id": conv}); isErr {
		t.Fatalf("parent session_start refused: %s", text)
	}
	if text, isErr := c.call(t, sub, "session_start", map[string]any{"workspace": ws, "session_id": sub}); isErr {
		t.Fatalf("subagent session_start refused: %s", text)
	}
	text, isErr := c.call(t, sub, "session_start", map[string]any{"workspace": other, "session_id": sub})
	if !isErr || !strings.Contains(text, "force: true") {
		t.Fatalf("subagent re-pin to another project must be refused with the remedy, got isError=%v %q", isErr, text)
	}
	if got := c.s.workspaceFor(agentCtx(conv)); got != ws {
		t.Errorf("parent's pin moved to %q", got)
	}
	if got := c.s.workspaceFor(agentCtx(sub)); got != ws {
		t.Errorf("subagent's refused re-pin moved its shard to %q", got)
	}
}

// testHookStampedParentReadsSurvive: a read the parent made as the connection
// is still its own once a subagent turns the connection shared.
func testHookStampedParentReadsSurvive(t *testing.T) {
	c := newStampedConn(t)
	ws := freshTempDir(t)
	mustGitDir(t, ws)
	const conv, sub = "conv-3", "conv-3/agent-1"
	path := filepath.Join(ws, "main.go")
	if err := os.WriteFile(path, []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if text, isErr := c.call(t, conv, "session_start", map[string]any{"workspace": ws, "session_id": conv}); isErr {
		t.Fatalf("parent session_start refused: %s", text)
	}
	if text, isErr := c.call(t, conv, "read_file", map[string]any{"file_path": path}); isErr {
		t.Fatalf("parent read_file refused: %s", text)
	}
	if text, isErr := c.call(t, sub, "session_start", map[string]any{"workspace": ws, "session_id": sub}); isErr {
		t.Fatalf("subagent session_start refused: %s", text)
	}
	if c.s.readTrackerFor(agentCtx(conv)).Mtime(path).IsZero() {
		t.Fatal("the parent's read was lost when the connection turned shared")
	}
	if !c.s.readTrackerFor(agentCtx(sub)).Mtime(path).IsZero() {
		t.Fatal("the subagent inherited a read it never made")
	}
}
