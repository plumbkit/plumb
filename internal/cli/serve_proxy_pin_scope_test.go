package cli

// serve_proxy_pin_scope_test.go — the proxy's replay pin is the CONNECTION's
// (issue #527, second half). A session_start that moved only one agent's shard
// says nothing about where the connection should come back, and replaying it
// would make a roots-derived connection pin sticky.

import (
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
)

// scopedResult answers a session_start the way a current daemon does: the
// session ID always, the resolved root and scope when the call carried a workspace.
func scopedResult(id, resolved, scope string) []byte {
	meta := `"` + mcp.MetaSessionIDKey + `":"sess-1"`
	if resolved != "" {
		meta += `,"` + mcp.MetaResolvedWorkspaceKey + `":"` + resolved + `"`
	}
	if scope != "" {
		meta += `,"` + mcp.MetaPinScopeKey + `":"` + scope + `"`
	}
	return []byte(`{"jsonrpc":"2.0","id":` + id + `,"result":{"content":[{"type":"text","text":"ok"}],"_meta":{` + meta + `}}}`)
}

func TestCommitSessionStartPin_AgentScopeIsNotTheReplayPin(t *testing.T) {
	p := newPinProxy()
	p.observeClientRequest(sessionStartFrame("1", "/Users/me/agent-worktree"))
	p.commitSessionStartPin(scopedResult("1", "", mcp.PinScopeAgent))

	if got := p.pinnedWorkspace(); got != "" {
		t.Fatalf("an agent-scope session_start became the connection's replay pin: %q — on reconnect it is replayed as a sticky connection pin", got)
	}
	if got := p.sessionID(); got != "sess-1" {
		t.Errorf("sessionID = %q, want sess-1: the identity channel does not share the pin's gate", got)
	}
}

// An agent-scope call must not disturb a connection pin the proxy already holds,
// whatever spelling the daemon reported: skipping means leaving the pin alone,
// not clearing it and not overwriting it with the raw argument.
func TestCommitSessionStartPin_AgentScopeLeavesAnEarlierConnectionPinAlone(t *testing.T) {
	p := newPinProxy()
	p.observeClientRequest(sessionStartFrame("1", "/Users/me/proj"))
	p.commitSessionStartPin(scopedResult("1", "/Users/me/proj", mcp.PinScopeConnection))

	p.observeClientRequest(sessionStartFrame("2", "/Users/me/agent-worktree"))
	p.commitSessionStartPin(scopedResult("2", "/Users/me/proj", mcp.PinScopeAgent))

	if got := p.pinnedWorkspace(); got != "/Users/me/proj" {
		t.Fatalf("pinnedWorkspace = %q, want the connection's /Users/me/proj untouched by an agent-scope call", got)
	}
}

// The control, in the direction the fix could break: a connection-scope call is
// recorded, with the daemon's canonical spelling.
func TestCommitSessionStartPin_ConnectionScopeIsTheReplayPin(t *testing.T) {
	p := newPinProxy()
	p.observeClientRequest(sessionStartFrame("1", "/Users/me/proj/subdir"))
	p.commitSessionStartPin(scopedResult("1", "/Users/me/proj", mcp.PinScopeConnection))

	if got := p.pinnedWorkspace(); got != "/Users/me/proj" {
		t.Fatalf("pinnedWorkspace = %q, want the daemon's canonical /Users/me/proj", got)
	}
}

// A daemon that predates the key sends no scope; the proxy must read that as the
// connection's, exactly as before the key existed.
func TestCommitSessionStartPin_NoScopeKeyIsTheConnectionsPin(t *testing.T) {
	p := newPinProxy()
	p.observeClientRequest(sessionStartFrame("1", "/Users/me/proj"))
	p.commitSessionStartPin(scopedResult("1", "/Users/me/proj", ""))

	if got := p.pinnedWorkspace(); got != "/Users/me/proj" {
		t.Fatalf("pinnedWorkspace = %q, want /Users/me/proj", got)
	}
}
