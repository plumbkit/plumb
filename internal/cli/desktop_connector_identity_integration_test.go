//go:build integration

package cli

// desktop_connector_identity_integration_test.go — the 2026-09-30 incident end
// to end. Claude desktop's connector (clientInfo "local-agent-mode-plumb") runs
// ONE plumb serve for every conversation, applies the identity hook's
// updatedInput, and forwards only the argument keys a tool's schema declares.
// An agent re-pinned itself to a worktree at agent scope, then its relative
// edit_file landed in the main checkout: the reverse-DNS stamp had been
// dropped, so the call resolved through the connection pin.
//
// Run with
//
//	go test -tags=integration ./internal/cli/ -run 'TestDesktopConnector' -v

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

// desktopConn is one connection behind a host that forwards only declared
// argument keys, stamped by the hook under stampKey.
type desktopConn struct {
	s        *connSession
	srv      *mcp.Server
	id       int
	stampKey string
	declared map[string]map[string]bool
}

func newDesktopConn(t *testing.T, stampKey string) *desktopConn {
	t.Helper()
	store, ss := newOriginStore(t)
	s := newPersistSession(t, store, ss, "proxy-desktop")
	s.onClientInfo("local-agent-mode-plumb", "1.0.0")
	srv := mcp.New(mcp.ServerInfo{Name: "test", Version: "0"})
	srv.Register(tools.NewSessionStart(s.workspaceFor, nil, nil, nil, s.clientNameStr, nil).
		WithRepin(s.repinWorkspace).
		WithDeclaredAgent(s.declaredAgentCtx).
		WithStampChannel(s.stampChannelState).
		WithLinkage(s.linkExternalID))
	srv.Register(tools.NewWriteFile(s.buildWriteDeps()))
	srv.Register(tools.NewReadFile(s.readTracker).WithReadsFor(s.readTrackerFor).WithWorkspace(s.workspaceFor))
	srv.OnToolRefusal = s.refuseSharedStateChange
	// As registerHooks wires it: a successful session_start declares the
	// per-call identity it ran under (issue #513).
	srv.OnAfterTool = s.afterToolFromCtx
	srv.OnBeforeTool = func(ctx context.Context, name string, args json.RawMessage, agent string) {
		s.recordLogicalAgentCall(agent)
		s.onBeforeTool(ctx, name, args)
	}
	srv.DeclareIdentityArg = s.clientStripsUndeclaredArgs
	c := &desktopConn{s: s, srv: srv, stampKey: stampKey}
	c.declared = c.listDeclared(t)
	return c
}

// listDeclared reads tools/list the way the host does and keeps, per tool, the
// argument keys its schema declares.
func (c *desktopConn) listDeclared(t *testing.T) map[string]map[string]bool {
	t.Helper()
	out := c.serve(t, `{"jsonrpc":"2.0","id":0,"method":"tools/list"}`)
	var resp struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				InputSchema struct {
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("decode tools/list %s: %v", out, err)
	}
	declared := map[string]map[string]bool{}
	for _, tl := range resp.Result.Tools {
		keys := map[string]bool{}
		for k := range tl.InputSchema.Properties {
			keys[k] = true
		}
		declared[tl.Name] = keys
	}
	return declared
}

func (c *desktopConn) serve(t *testing.T, frame string) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := c.srv.Serve(context.Background(), strings.NewReader(frame+"\n"), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	return out.Bytes()
}

// call stamps args as the hook does, strips every undeclared key as the host
// does, and serves the call. It returns the result text and whether it failed.
func (c *desktopConn) call(t *testing.T, agent, name string, args map[string]any) (string, bool) {
	t.Helper()
	if agent != "" {
		args[c.stampKey] = agent
	}
	for k := range args {
		if !c.declared[name][k] {
			delete(args, k)
		}
	}
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal %s args: %v", name, err)
	}
	c.id++
	out := c.serve(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, c.id, name, raw))
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
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("decode response %s: %v", out, err)
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

// incident runs the 2026-09-30 sequence and returns the two checkouts and
// agent Y's write result.
func incident(t *testing.T, c *desktopConn) (mainCheckout, worktree, text string, isErr bool) {
	t.Helper()
	mainCheckout, worktree = freshTempDir(t), freshTempDir(t)
	mustGitDir(t, mainCheckout)
	mustGitDir(t, worktree)
	const x, y = "conv-x", "conv-y"
	for _, agent := range []string{x, y} {
		if text, isErr := c.call(t, agent, "session_start", map[string]any{"workspace": mainCheckout, "session_id": agent}); isErr {
			t.Fatalf("%s session_start refused: %s", agent, text)
		}
	}
	if text, isErr := c.call(t, y, "session_start", map[string]any{
		"workspace": worktree, "session_id": y, "force": true, "scope": "agent",
	}); isErr {
		t.Fatalf("agent-scope re-pin refused: %s", text)
	}
	text, isErr = c.call(t, y, "write_file", map[string]any{"file_path": "PROTOCOL.md", "content": "from y\n"})
	return mainCheckout, worktree, text, isErr
}

func TestDesktopConnector(t *testing.T) {
	t.Run("DeclaredStampRoutesTheWriteToTheAgentsWorktree", func(t *testing.T) {
		c := newDesktopConn(t, mcp.ArgLogicalAgentDeclaredKey)
		mainCheckout, worktree, text, isErr := incident(t, c)
		if isErr {
			t.Fatalf("stamped relative write refused: %s", text)
		}
		if got, err := os.ReadFile(filepath.Join(worktree, "PROTOCOL.md")); err != nil || string(got) != "from y\n" {
			t.Fatalf("the write did not land in the agent's worktree: %q, %v", got, err)
		}
		if _, err := os.Stat(filepath.Join(mainCheckout, "PROTOCOL.md")); err == nil {
			t.Fatal("the write ALSO landed in the main checkout")
		}
		// The peer still resolves against its own pin.
		if text, isErr := c.call(t, "conv-x", "write_file", map[string]any{"file_path": "X.md", "content": "x\n"}); isErr {
			t.Fatalf("peer write refused: %s", text)
		}
		if _, err := os.Stat(filepath.Join(mainCheckout, "X.md")); err != nil {
			t.Fatalf("the peer's relative write did not land in its checkout: %v", err)
		}
		if _, err := os.Stat(filepath.Join(worktree, "X.md")); err == nil {
			t.Fatal("the peer's write landed in the other agent's worktree")
		}
	})

	// The reverse-DNS stamp through the same host is dropped, as it was in the
	// incident. The anonymous write is now refused with a remedy, and lands in
	// neither checkout: not in the worktree (the identity is gone), and not in
	// the main checkout another agent pinned (plumb no longer guesses). This is
	// also the control showing the harness really drops undeclared keys.
	t.Run("UndeclaredStampIsRefusedNotMisrouted", func(t *testing.T) {
		c := newDesktopConn(t, mcp.ArgLogicalAgentKey)
		mainCheckout, worktree, text, isErr := incident(t, c)
		if !isErr {
			t.Fatalf("an unattributable write on a shared connection was admitted: %s", text)
		}
		for _, want := range []string{"cannot be attributed", "plumb hooks install claude-code"} {
			if !strings.Contains(text, want) {
				t.Errorf("refusal missing %q: %s", want, text)
			}
		}
		for _, dir := range []string{mainCheckout, worktree} {
			if _, err := os.Stat(filepath.Join(dir, "PROTOCOL.md")); err == nil {
				t.Errorf("the refused write landed in %s", dir)
			}
		}
	})

	// Issue #513. The stamp survives the host, but it names an identity no
	// session_start on this connection declared — a model typing plumb_agent
	// where the hook does not run. It used to be admitted onto a fresh shard
	// seeded from the connection's root and land in the main checkout. It is
	// refused, lands nowhere, and leaves no identity behind.
	t.Run("InventedIdentityIsRefusedNotMisrouted", func(t *testing.T) {
		c := newDesktopConn(t, mcp.ArgLogicalAgentDeclaredKey)
		mainCheckout, worktree, text, isErr := incident(t, c)
		if isErr {
			t.Fatalf("precondition: the declared agent's write was refused: %s", text)
		}
		observed := c.s.logicalAgents.count()

		text, isErr = c.call(t, "my-session", "write_file", map[string]any{"file_path": "INVENTED.md", "content": "who\n"})
		if !isErr {
			t.Fatalf("a write under an undeclared identity was admitted: %s", text)
		}
		if !strings.Contains(text, "session_start") {
			t.Errorf("the refusal does not name session_start as the remedy: %s", text)
		}
		for _, dir := range []string{mainCheckout, worktree} {
			if _, err := os.Stat(filepath.Join(dir, "INVENTED.md")); err == nil {
				t.Errorf("the refused write landed in %s", dir)
			}
		}
		if got := c.s.logicalAgents.count(); got != observed {
			t.Errorf("the refused call registered an identity: %d observed, want %d", got, observed)
		}

		// Controls. The declared agent still writes into its own worktree —
		// the same relative write through the same host, so the refusal above
		// is about the identity, not the call.
		if text, isErr := c.call(t, "conv-y", "write_file", map[string]any{"file_path": "Y2.md", "content": "y\n"}); isErr {
			t.Fatalf("declared agent's write refused: %s", text)
		}
		if _, err := os.Stat(filepath.Join(worktree, "Y2.md")); err != nil {
			t.Fatalf("the declared agent's write did not land in its worktree: %v", err)
		}
		// A hook-stamped subagent of a declared conversation is admitted
		// without ever calling session_start itself, and works where its
		// conversation chose to: conv-y re-pinned itself to the worktree, so
		// its subagent's relative write lands there, not in conv-x's checkout.
		if text, isErr := c.call(t, "conv-y/sub", "write_file", map[string]any{"file_path": "SUB.md", "content": "sub\n"}); isErr {
			t.Fatalf("a subagent of a declared conversation was refused: %s", text)
		}
		if _, err := os.Stat(filepath.Join(worktree, "SUB.md")); err != nil {
			t.Fatalf("the subagent's write did not land in its conversation's worktree: %v", err)
		}
		if _, err := os.Stat(filepath.Join(mainCheckout, "SUB.md")); err == nil {
			t.Fatal("the subagent's write landed in the other agent's checkout")
		}
		// The same root answers every implicit resolver, git's default
		// repository included (the git tool resolves through workspaceFor).
		if got := c.s.workspaceFor(mcp.WithLogicalAgent(context.Background(), "conv-y/sub")); got != worktree {
			t.Errorf("the subagent's workspace (git's default repository) is %q, want %q", got, worktree)
		}
		// The count above is not vacuous: an admitted new identity IS recorded.
		if got := c.s.logicalAgents.count(); got != observed+1 {
			t.Errorf("an admitted subagent was not recorded: %d observed, want %d", got, observed+1)
		}

		// The remedy is reachable: session_start under the identity declares it.
		if text, isErr := c.call(t, "my-session", "session_start", map[string]any{"session_id": "my-session"}); isErr {
			t.Fatalf("session_start, the named remedy, was refused: %s", text)
		}
		if text, isErr := c.call(t, "my-session", "write_file", map[string]any{"file_path": "INVENTED.md", "content": "who\n"}); isErr {
			t.Fatalf("a write after declaring through session_start was refused: %s", text)
		}
	})

	// A client that stamps every call and calls session_start WITHOUT a
	// session_id — the _meta-only channel sharedIdentityRemedy sanctions. The
	// declaration comes from the after-tool hook, not the session_id linker.
	t.Run("MetaOnlySessionStartDeclares", func(t *testing.T) {
		c := newDesktopConn(t, mcp.ArgLogicalAgentDeclaredKey)
		mainCheckout, _, text, isErr := incident(t, c)
		if isErr {
			t.Fatalf("precondition: the declared agent's write was refused: %s", text)
		}
		write := `{"file_path":"META.md","content":"meta\n"}`
		if text, isErr := c.callMeta(t, "meta-only", "write_file", write); !isErr {
			t.Fatalf("an undeclared _meta identity's write was admitted: %s", text)
		}
		if text, isErr := c.callMeta(t, "meta-only", "session_start", `{}`); isErr {
			t.Fatalf("session_start without a session_id failed: %s", text)
		}
		if text, isErr := c.callMeta(t, "meta-only", "write_file", write); isErr {
			t.Fatalf("a write after a _meta-only session_start was refused: %s", text)
		}
		if _, err := os.Stat(filepath.Join(mainCheckout, "META.md")); err != nil {
			t.Fatalf("the admitted write did not land in the root session_start reported: %v", err)
		}
	})
}

// callMeta serves a call whose identity rides _meta[dev.plumbkit/logical-agent]
// rather than an argument, with argsJSON passed through verbatim.
func (c *desktopConn) callMeta(t *testing.T, agent, name, argsJSON string) (string, bool) {
	t.Helper()
	c.id++
	out := c.serve(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s,"_meta":{%q:%q}}}`,
		c.id, name, argsJSON, mcp.MetaLogicalAgentKey, agent))
	var resp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("decode response %s: %v", out, err)
	}
	text := ""
	if len(resp.Result.Content) > 0 {
		text = resp.Result.Content[0].Text
	}
	return text, resp.Result.IsError
}
