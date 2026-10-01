package cli

// conn_pin_scope_meta_test.go — which pin a session_start moved, as the serve
// proxy hears it (issue #527, second half).
//
// The proxy records a successful session_start's workspace as its REPLAY pin and
// replays it on reconnect as a connection-level, session_start-origin pin, which
// is sticky and outranks the client's roots. toolResultMeta reported the
// CONNECTION's workspace after every session_start, whichever pin the call moved,
// so an agent-scope call — which moved only that agent's shard — made the proxy
// replay the connection's roots-derived root as though someone had chosen it.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
)

func sessionStartArgs(t *testing.T, workspace string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"workspace": workspace})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestToolResultMeta_AgentScopeIsMarkedAndKeepsTheConnectionsRoot: an agent-scope
// session_start says so, and still names the CONNECTION's root (never the agent's
// worktree) as the resolved workspace. The root is there for a proxy that
// predates the scope key: it cannot read the scope, so it commits whatever
// resolved workspace the result names, and with none it falls back to the raw
// argument, the agent's own worktree, which it replays as the connection's pin
// for every agent on the connection.
func TestToolResultMeta_AgentScopeIsMarkedAndKeepsTheConnectionsRoot(t *testing.T) {
	store, ss := newOriginStore(t)
	parent, worktree := worktreeUnderParent(t)
	s := newPersistSession(t, store, ss, "proxy-scope-agent")
	s.attachWorkspace(context.Background(), "file://"+parent) // the client's roots
	s.recordLogicalAgentAttach("coordinator")
	s.recordLogicalAgentAttach("agent-A")

	ctx := mcp.WithResultNotes(mcp.WithLogicalAgent(context.Background(), "agent-A"))
	rep, err := s.repinWorkspace(ctx, worktree, "", false, false)
	if err != nil {
		t.Fatalf("the agent's own session_start: %v", err)
	}
	if rep.Scope != "agent" {
		t.Fatalf("precondition: the re-pin moved scope %q, want agent", rep.Scope)
	}

	meta := s.toolResultMeta(ctx, "session_start", sessionStartArgs(t, worktree))
	if got := meta[mcp.MetaPinScopeKey]; got != mcp.PinScopeAgent {
		t.Errorf("_meta[%s] = %v, want %q", mcp.MetaPinScopeKey, got, mcp.PinScopeAgent)
	}
	if got := s.workspace(); got != parent {
		t.Fatalf("precondition: the agent's own pin left the connection on %q, want %q", got, parent)
	}
	got, ok := meta[mcp.MetaResolvedWorkspaceKey]
	if !ok {
		t.Fatalf("_meta carries no %s on an agent-scope call: a proxy built before %s falls back to the raw argument, %s, and replays the agent's worktree as the connection's pin",
			mcp.MetaResolvedWorkspaceKey, mcp.MetaPinScopeKey, worktree)
	}
	if got != parent {
		t.Errorf("_meta[%s] = %v, want the CONNECTION's root %q, not the agent's worktree %q",
			mcp.MetaResolvedWorkspaceKey, got, parent, worktree)
	}
	// The identity channel is independent of the pin and must not lose its gate.
	if _, ok := meta[mcp.MetaSessionIDKey]; !ok {
		t.Errorf("_meta lost %s: an agent-scope session_start still tells the proxy who the session is", mcp.MetaSessionIDKey)
	}
}

// TestToolResultMeta_ConnectionScopeStillReportsTheResolvedRoot is the other
// scope, and the positive control for the one above: a call that moved the
// connection's pin must keep feeding the proxy's replay pin.
func TestToolResultMeta_ConnectionScopeStillReportsTheResolvedRoot(t *testing.T) {
	store, ss := newOriginStore(t)
	parent, worktree := worktreeUnderParent(t)

	cases := []struct {
		name  string
		setup func(s *connSession)
		ctx   func() context.Context
		scope bool // scope: "connection" on the call
	}{
		{
			name:  "an identified agent asks for scope connection",
			setup: func(s *connSession) { s.recordLogicalAgentAttach("coordinator"); s.recordLogicalAgentAttach("agent-A") },
			ctx: func() context.Context {
				return mcp.WithLogicalAgent(context.Background(), "agent-A")
			},
			scope: true,
		},
		{
			name:  "an anonymous caller on a connection nobody shares",
			setup: func(*connSession) {},
			ctx:   context.Background,
		},
		{
			name:  "the only identity the connection has seen",
			setup: func(s *connSession) { s.recordLogicalAgentAttach("agent-A") },
			ctx: func() context.Context {
				return mcp.WithLogicalAgent(context.Background(), "agent-A")
			},
		},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newPersistSession(t, store, ss, "proxy-scope-conn-"+string(rune('a'+i)))
			s.attachWorkspace(context.Background(), "file://"+parent)
			c.setup(s)

			ctx := mcp.WithResultNotes(c.ctx())
			rep, err := s.repinWorkspace(ctx, worktree, "", true, c.scope)
			if err != nil {
				t.Fatalf("session_start: %v", err)
			}
			if rep.Scope != "connection" {
				t.Fatalf("precondition: the re-pin moved scope %q, want connection", rep.Scope)
			}

			meta := s.toolResultMeta(ctx, "session_start", sessionStartArgs(t, worktree))
			if got := meta[mcp.MetaResolvedWorkspaceKey]; got != s.workspace() || got != worktree {
				t.Errorf("_meta[%s] = %v, want the connection's new root %q", mcp.MetaResolvedWorkspaceKey, got, worktree)
			}
			if got := meta[mcp.MetaPinScopeKey]; got != mcp.PinScopeConnection {
				t.Errorf("_meta[%s] = %v, want %q", mcp.MetaPinScopeKey, got, mcp.PinScopeConnection)
			}
		})
	}
}

// TestToolResultMeta_UnreportedScopeKeepsTheEarlierBehaviour: a call that made no
// scope decision known to the hook (no scratchpad: an in-process call) is read as
// the connection's, which is what every caller got before the key existed.
func TestToolResultMeta_UnreportedScopeKeepsTheEarlierBehaviour(t *testing.T) {
	store, ss := newOriginStore(t)
	_, worktree := worktreeUnderParent(t)
	s := newPersistSession(t, store, ss, "proxy-scope-none")
	if _, err := s.repinWorkspace(context.Background(), worktree, "", false, false); err != nil {
		t.Fatalf("session_start: %v", err)
	}

	meta := s.toolResultMeta(context.Background(), "session_start", sessionStartArgs(t, worktree))
	if got := meta[mcp.MetaResolvedWorkspaceKey]; got != worktree {
		t.Errorf("_meta[%s] = %v, want %q", mcp.MetaResolvedWorkspaceKey, got, worktree)
	}
	if got, ok := meta[mcp.MetaPinScopeKey]; ok {
		t.Errorf("_meta[%s] = %v with no scope reported; the daemon must not invent one", mcp.MetaPinScopeKey, got)
	}
}

// TestSessionStartScope_RoundTripsToTheProxysReplayPin runs the daemon's actual
// `_meta` through the proxy's actual commit, for both scopes and for the case the
// daemon cannot name a connection root at all. Each half is tested alone above; a
// scope the daemon reports under one key name and the proxy reads under another
// would pass both.
func TestSessionStartScope_RoundTripsToTheProxysReplayPin(t *testing.T) {
	store, ss := newOriginStore(t)
	parent, worktree := worktreeUnderParent(t)

	cases := []struct {
		name       string
		attach     bool // the connection already has a root, from the client's roots
		connScope  bool
		wantPinned string
	}{
		{"an agent's own pin on a connection the client's roots attached", true, false, ""},
		{"an agent's own pin on a connection with no root at all", false, false, ""},
		{"the connection's pin", true, true, worktree},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newPersistSession(t, store, ss, "proxy-scope-roundtrip-"+string(rune('a'+i)))
			if c.attach {
				s.attachWorkspace(context.Background(), "file://"+parent)
			}
			s.recordLogicalAgentAttach("coordinator")
			s.recordLogicalAgentAttach("agent-A")

			ctx := mcp.WithResultNotes(mcp.WithLogicalAgent(context.Background(), "agent-A"))
			if _, err := s.repinWorkspace(ctx, worktree, "", false, c.connScope); err != nil {
				t.Fatalf("session_start: %v", err)
			}
			meta := s.toolResultMeta(ctx, "session_start", sessionStartArgs(t, worktree))
			metaJSON, err := json.Marshal(meta)
			if err != nil {
				t.Fatal(err)
			}

			p := newPinProxy()
			p.observeClientRequest(sessionStartFrame("1", worktree))
			p.commitSessionStartPin([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"ok"}],"_meta":` + string(metaJSON) + `}}`))

			if got := p.pinnedWorkspace(); got != c.wantPinned {
				t.Errorf("the proxy's replay pin = %q, want %q (daemon _meta %s)", got, c.wantPinned, metaJSON)
			}
		})
	}
}

// TestSessionStartScope_AnOlderProxyRecordsTheConnectionsRootNotTheWorktree runs
// the daemon's actual `_meta` through the commit of a proxy that predates the
// scope key. A daemon restart does not restart `plumb serve`, so this pairing is
// the normal state right after an upgrade, and the pairing the first version of
// the scope key got wrong: it withheld the resolved workspace for an agent-scope
// call, the older proxy fell back to the raw argument, and one agent's worktree
// became the replay pin every agent on the connection came back to.
//
// The older proxy cannot tell the scopes apart, so what it records is whatever
// the daemon names. For an agent-scope call that must be the connection's own
// root, which is what the proxy recorded before the key existed.
func TestSessionStartScope_AnOlderProxyRecordsTheConnectionsRootNotTheWorktree(t *testing.T) {
	store, ss := newOriginStore(t)
	parent, worktree := worktreeUnderParent(t)

	cases := []struct {
		name       string
		connScope  bool
		wantPinned string
	}{
		{"an agent's own pin leaves the connection's root as the replay pin", false, parent},
		{"a connection-scope pin is still the replay pin", true, worktree},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newPersistSession(t, store, ss, "proxy-old-proxy-"+string(rune('a'+i)))
			s.attachWorkspace(context.Background(), "file://"+parent) // the client's roots
			s.recordLogicalAgentAttach("coordinator")
			s.recordLogicalAgentAttach("agent-A")

			ctx := mcp.WithResultNotes(mcp.WithLogicalAgent(context.Background(), "agent-A"))
			if _, err := s.repinWorkspace(ctx, worktree, "", false, c.connScope); err != nil {
				t.Fatalf("session_start: %v", err)
			}
			metaJSON, err := json.Marshal(s.toolResultMeta(ctx, "session_start", sessionStartArgs(t, worktree)))
			if err != nil {
				t.Fatal(err)
			}

			p := newPinProxy()
			p.observeClientRequest(sessionStartFrame("1", worktree))
			commitSessionStartPinBeforePinScope(p, []byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"ok"}],"_meta":`+string(metaJSON)+`}}`))

			if got := p.pinnedWorkspace(); got != c.wantPinned {
				t.Errorf("a proxy that ignores the scope recorded %q, want %q (daemon _meta %s)", got, c.wantPinned, metaJSON)
			}
		})
	}
}
