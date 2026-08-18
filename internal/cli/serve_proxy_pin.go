package cli

// serve_proxy_pin.go — the proxy's memory of the caller's chosen workspace.
//
// A daemon restart destroys every connSession. The `plumb serve` proxy does not
// restart with it: it reconnects and replays the captured initialize handshake.
// That makes the proxy the only component that spans the restart, and therefore
// the right place to remember which workspace the caller actually chose.
//
// So the proxy watches for a SUCCESSFUL session_start carrying a `workspace`
// argument and replays that path in the initialize _meta on every reconnect,
// under mcp.MetaPinnedWorkspaceKey. The fresh daemon treats it as rung 1 of the
// attach ladder — the same authority as the live call that produced it — which
// is why it outranks the client's roots. See conn_attach_oninit.go.
//
// This carries the same fact as the persisted pin's session_start origin, by an
// independent route: the proxy's memory survives a pruned or disabled
// session-state database, and the database survives a proxy that predates this
// code. Whichever arrives first attaches; the ladder is first-wins, so the other
// no-ops. They cannot disagree — the same session_start call feeds both.
//
// Scope: one pin per proxy, because a proxy has one connSession. A client that
// multiplexes several agent sessions over one `plumb serve` shares that pin, and
// a peer's session_start re-pins them all. That is issue #182 — a limitation
// this inherits from the connection model, not one it introduces.

import (
	"encoding/json"
	"path/filepath"

	"github.com/plumbkit/plumb/internal/mcp"
)

// sessionStartTool is the only tool whose workspace argument re-pins a
// connection. Keep in step with internal/tools/session_start.go.
const sessionStartTool = "session_start"

// observeClientRequest records a session_start request that carries an absolute
// workspace argument, keyed by its JSON-RPC id, so the response can confirm it.
// Everything else is ignored — in particular a session_start WITHOUT a workspace
// arg never enters the map, so an orientation call can never clear a deliberate
// pin.
//
// Runs on the client→daemon pump.
func (p *reconnectingProxy) observeClientRequest(frame []byte) {
	e := parseEnvelope(frame)
	if !e.isRequest() || e.Method != "tools/call" {
		return
	}
	ws := sessionStartWorkspace(frame)
	if ws == "" {
		return
	}
	p.pinMu.Lock()
	defer p.pinMu.Unlock()
	if p.pending == nil {
		p.pending = map[string]string{}
	}
	p.pending[idKey(e.ID)] = ws
}

// sessionStartWorkspace returns the absolute workspace argument of a
// session_start tools/call frame, or "" for anything else. Fail-safe: any frame
// that does not parse into the expected shape yields "".
//
// A relative path is rejected. The daemon is a singleton whose working directory
// is unrelated to any workspace, so a relative path replayed into a fresh daemon
// would resolve against the wrong thing — exactly the class of bug this file
// exists to prevent.
func sessionStartWorkspace(frame []byte) string {
	var req struct {
		Params struct {
			Name      string `json:"name"`
			Arguments struct {
				Workspace string `json:"workspace"`
			} `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(frame, &req); err != nil {
		return ""
	}
	if req.Params.Name != sessionStartTool {
		return ""
	}
	ws := req.Params.Arguments.Workspace
	if ws == "" || !filepath.IsAbs(ws) {
		return ""
	}
	return ws
}

// commitSessionStartPin promotes a pending session_start workspace to the live
// pin, but only when the daemon actually accepted the re-pin. A rejected call
// (a JSON-RPC error, or a tool result flagged isError) is discarded: replaying a
// workspace the live daemon refused would pin the fresh one to a path it would
// have refused too.
//
// The daemon echoes the CANONICAL root it pinned in the result's _meta
// (mcp.MetaResolvedWorkspaceKey); that spelling is committed in preference to
// the caller's raw argument, so the replayed pin is always a resolved root the
// restore path can verify — never an alias or a subdirectory that re-resolves
// against state the proxy cannot see. A daemon that predates the key sends no
// _meta; the raw argument stands as before.
//
// Runs on the daemon→client pump, which is single-threaded; the mutex guards
// against the client pump writing p.pending concurrently.
func (p *reconnectingProxy) commitSessionStartPin(frame []byte) {
	e := parseEnvelope(frame)
	if !e.isResponse() {
		return
	}
	key := idKey(e.ID)
	p.pinMu.Lock()
	ws, waiting := p.pending[key]
	delete(p.pending, key)
	if waiting && toolCallSucceeded(frame) {
		if resolved := resolvedWorkspaceMeta(frame); resolved != "" {
			ws = resolved
		}
		p.pinned = ws
		if id := sessionIDMeta(frame); id != "" {
			p.heldSessionID = id
		}
	}
	p.pinMu.Unlock()
}

// resolvedWorkspaceMeta extracts the canonical workspace root the daemon echoed
// in a session_start result's _meta, or "" when absent (a daemon that predates
// the key) or malformed. Fail-safe like toolCallSucceeded: anything it cannot
// parse yields "", leaving the caller's raw spelling in place.
func resolvedWorkspaceMeta(frame []byte) string {
	var resp struct {
		Result *struct {
			Meta map[string]json.RawMessage `json:"_meta"`
		} `json:"result"`
	}
	if err := json.Unmarshal(frame, &resp); err != nil || resp.Result == nil {
		return ""
	}
	raw, ok := resp.Result.Meta[mcp.MetaResolvedWorkspaceKey]
	if !ok {
		return ""
	}
	var ws string
	if err := json.Unmarshal(raw, &ws); err != nil {
		return ""
	}
	return ws
}

// sessionIDMeta extracts the plumb session ID the daemon echoed in a
// session_start result's _meta (mcp.MetaSessionIDKey), or "" when absent or
// malformed. Mirrors resolvedWorkspaceMeta: fail-safe, so anything it cannot
// parse yields "" and the proxy holds no stale identity.
func sessionIDMeta(frame []byte) string {
	var resp struct {
		Result *struct {
			Meta map[string]json.RawMessage `json:"_meta"`
		} `json:"result"`
	}
	if err := json.Unmarshal(frame, &resp); err != nil || resp.Result == nil {
		return ""
	}
	raw, ok := resp.Result.Meta[mcp.MetaSessionIDKey]
	if !ok {
		return ""
	}
	var id string
	if err := json.Unmarshal(raw, &id); err != nil {
		return ""
	}
	return id
}

// toolCallSucceeded reports whether a JSON-RPC response carries a tool result
// that did not fail. Fail-safe: anything it cannot parse is treated as a
// failure, so an unrecognised shape never commits a pin.
func toolCallSucceeded(frame []byte) bool {
	var resp struct {
		Error  json.RawMessage `json:"error"`
		Result *struct {
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(frame, &resp); err != nil {
		return false
	}
	if len(resp.Error) > 0 || resp.Result == nil {
		return false
	}
	return !resp.Result.IsError
}

// pinnedWorkspace returns the workspace the caller last chose via session_start,
// or "" when they never did.
func (p *reconnectingProxy) pinnedWorkspace() string {
	p.pinMu.Lock()
	defer p.pinMu.Unlock()
	return p.pinned
}

// sessionID returns the plumb session ID the daemon last echoed, or "" when the
// proxy has not learned it yet (no successful session_start has completed).
func (p *reconnectingProxy) sessionID() string {
	p.pinMu.Lock()
	defer p.pinMu.Unlock()
	return p.heldSessionID
}

// replayInitMeta builds the _meta folded into a replayed initialize frame: the
// pinned workspace (when one was chosen) and the stable session ID the daemon
// echoed (PLAN-296). Both are learned from a session_start result after the
// handshake, so neither can be baked in at capture time; nil means no meta to
// fold, leaving a first connect byte-identical.
func (p *reconnectingProxy) replayInitMeta() map[string]json.RawMessage {
	meta := pinnedWorkspaceMeta(p.pinnedWorkspace())
	if id := p.sessionID(); id != "" {
		if raw, err := json.Marshal(id); err == nil {
			if meta == nil {
				meta = map[string]json.RawMessage{}
			}
			meta[mcp.MetaSessionIDKey] = raw
		}
	}
	return meta
}

// pinnedWorkspaceMeta is the _meta fragment folded into a REPLAYED initialize
// frame. It cannot be built at captureHandshake time: the pin is learned from a
// tool call that happens long after the handshake, so it is layered onto the
// captured frame at replay. An empty pin returns nil, leaving the frame
// byte-identical — the first connect behaves exactly as it always did.
func pinnedWorkspaceMeta(workspace string) map[string]json.RawMessage {
	if workspace == "" {
		return nil
	}
	raw, err := json.Marshal(workspace)
	if err != nil {
		return nil
	}
	return map[string]json.RawMessage{mcp.MetaPinnedWorkspaceKey: raw}
}
