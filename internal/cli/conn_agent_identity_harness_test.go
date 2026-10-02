package cli

// conn_agent_identity_harness_test.go — the end-to-end rig for issue #556's
// identity tests: real connSessions behind a real mcp.Server with every tool
// registered by registerAllTools, driven by tools/call frames stamped the way the
// Claude Code hook stamps them (the identity inside the arguments).
//
// It goes through the server rather than calling tool.Execute because the defects
// under test live in the wiring between the two — which accessor session_start was
// handed, which inbox a claim ran against — and a harness that built each tool by
// hand would test the hand-wiring instead of registerAllTools.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/sessionstate"
	"github.com/plumbkit/plumb/internal/tools"
)

// identityWorld is the state several connections share in one test: the collab
// pool (so two connections see one collab.db and one notifier) and the config.
type identityWorld struct {
	t    *testing.T
	cfg  config.Config
	pool *collabPool
	ss   *sessionstate.Store
	ids  atomic.Int64
	// registry is the daemon's, shared by every connection: it carries the /clear
	// markers a hook announces to the control socket.
	registry *connRegistry
}

// identityConn is one MCP connection: its connSession and the server in front of it.
type identityConn struct {
	w   *identityWorld
	s   *connSession
	srv *mcp.Server
}

func newIdentityWorld(t *testing.T) *identityWorld {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	cfg := config.Defaults()
	cfg.Git.CommitTrailer = true
	cfg.Collab.Mailbox = true
	w := &identityWorld{t: t, cfg: cfg, pool: newCollabPool(), registry: newConnRegistry()}
	t.Cleanup(w.pool.closeAll)
	return w
}

// withState gives the world a durable state store, for the proxy-credential path.
func (w *identityWorld) withState() *identityWorld {
	ss, err := sessionstate.Open()
	if err != nil {
		w.t.Fatalf("sessionstate.Open: %v", err)
	}
	w.t.Cleanup(ss.Close)
	w.ss = ss
	return w
}

// conn opens a connection. proxy is the serve proxy's credential, or "" for a
// client that has none (the restarted-`plumb serve` case: new secret, new id).
func (w *identityWorld) conn(proxy string) *identityConn {
	w.t.Helper()
	s := newConnSession(context.Background(), detectTestPool(), nil, config.NewStore(w.cfg), nil, w.ss, newSharedBudgets())
	w.t.Cleanup(s.close)
	s.collabPool = w.pool
	s.registry = w.registry
	if proxy != "" {
		s.onProxySession(proxy)
	}
	srv := mcp.New(mcp.ServerInfo{Name: "test", Version: "0"})
	s.registerAllTools(srv, time.Now())
	s.registerHooks(srv)
	return &identityConn{w: w, s: s, srv: srv}
}

// call sends one tools/call frame as agent: the hook's stamp inside the
// arguments, or none at all when agent is "" (the hook failed open).
func (c *identityConn) call(agent, tool string, args map[string]any) (text string, isErr bool) {
	c.w.t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	if agent != "" {
		args[mcp.ArgLogicalAgentKey] = agent
	}
	raw, err := json.Marshal(args)
	if err != nil {
		c.w.t.Fatalf("marshal args: %v", err)
	}
	id := c.w.ids.Add(1)
	frame := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, id, tool, raw)
	var out bytes.Buffer
	if err := c.srv.Serve(context.Background(), strings.NewReader(frame+"\n"), &out); err != nil {
		c.w.t.Fatalf("Serve: %v", err)
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
		c.w.t.Fatalf("decode %q: %v", out.String(), err)
	}
	if resp.Error != nil {
		return resp.Error.Message, true
	}
	if len(resp.Result.Content) > 0 {
		text = resp.Result.Content[0].Text
	}
	return text, resp.Result.IsError
}

// start is a stamped session_start that names the workspace and the id.
func (c *identityConn) start(agent, workspace, sessionID string, extra map[string]any) string {
	c.w.t.Helper()
	args := map[string]any{"session_id": sessionID}
	if workspace != "" {
		args["workspace"] = workspace
	}
	for k, v := range extra {
		args[k] = v
	}
	out, isErr := c.call(agent, "session_start", args)
	if isErr {
		c.w.t.Fatalf("session_start as %q: %s", agent, out)
	}
	return out
}

// stampedCtx is the ctx a call stamped as agent carries.
func stampedCtx(agent string) context.Context {
	return mcp.WithLogicalAgent(context.Background(), agent)
}

// sessionLine returns the "Session:" line of an orientation packet.
func sessionLine(text string) string {
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "Session:") {
			return strings.TrimSpace(l)
		}
	}
	return ""
}

// identityRepo is a throwaway git repository, canonicalised like a pin.
func identityRepo(t *testing.T) string {
	t.Helper()
	ws := freshTempDir(t)
	for _, argv := range [][]string{
		{"init", "-q", ws},
		{"-C", ws, "config", "user.email", "t@example.com"},
		{"-C", ws, "config", "user.name", "T"},
	} {
		if out, err := exec.Command("git", argv...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", argv, err, out)
		}
	}
	return ws
}

// writeIdentityFile writes a file the commit tests stage.
func writeIdentityFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// conversationIDOf extracts the conversation_id check_messages prints.
func conversationIDOf(t *testing.T, text string) string {
	t.Helper()
	const marker = `conversation_id: "`
	i := strings.Index(text, marker)
	if i < 0 {
		t.Fatalf("no conversation_id in %q", text)
	}
	rest := text[i+len(marker):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		t.Fatalf("unterminated conversation_id in %q", text)
	}
	return rest[:j]
}

// inbox is the inbox of a connection with no agent to tell apart from it: the
// context-free call the single-agent mailbox tests make. Production never asks
// that question — every claim is made on behalf of a caller (inboxFor).
func (s *connSession) inbox() tools.Inbox {
	return s.inboxFor(context.Background())
}
