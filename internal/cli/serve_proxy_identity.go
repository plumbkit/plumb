package cli

// serve_proxy_identity.go — the proxy's memory of who the daemon says it is.
//
// The `plumb serve` proxy is the only component that spans a daemon restart, so
// it is the right place to hold the connection's identity. It used to learn that
// identity from a session_start RESULT, and only from one that carried a
// workspace argument — two gates that have nothing to do with identity. A
// connection that never called the tool, or called it to link a conversation
// without re-pinning, held nothing at all.
//
// The handshake has neither gate. Every MCP connection performs initialize
// exactly once, the proxy replays it on every reconnect, and the daemon now
// answers with an identity snapshot (mcp.MetaSessionIdentityKey). So the proxy
// learns the identity before the first tool call and re-learns it on every
// reconnect, whatever the client goes on to do.
//
// Two rules keep the snapshot honest, and both exist because a proxy that
// reports identity must not report it wrongly:
//
//   - Only an actual initialize RESPONSE may write it. Tool output cannot; a
//     JSON-RPC error cannot; a frame that does not parse cannot. Absence of the
//     key is not evidence of anything, so it never clears what is already held.
//   - A DEGRADED recovery never replaces the held identity. The ID the daemon
//     is reporting in that case is a temporary stand-in, and adopting it would
//     be the proxy helping to make a transient failure permanent.

import (
	"encoding/json"

	"github.com/plumbkit/plumb/internal/mcp"
)

// proxyIdentity is the identity snapshot the daemon acknowledged: the internal
// session ID, the current name, the revision ordering name changes, and the
// outcome of the recovery attempt.
//
// known distinguishes "the daemon said this session has no addressable identity"
// from "the daemon never said anything" — a legacy daemon that predates the key.
// The two look identical in every field and call for opposite reports.
type proxyIdentity struct {
	sessionID string
	name      string
	revision  int64
	recovery  string
	known     bool
}

// sessionIdentityMeta extracts the identity snapshot from an initialize response
// frame. ok is false for anything that is not a well-formed success response
// carrying the key: an error response, a missing or malformed `_meta`, a daemon
// that predates the key. Fail-safe throughout, because the caller's fallback —
// keep what is already held — is always safer than adopting a guess.
func sessionIdentityMeta(frame []byte) (proxyIdentity, bool) {
	var resp struct {
		Error  json.RawMessage `json:"error"`
		Result *struct {
			Meta map[string]json.RawMessage `json:"_meta"`
		} `json:"result"`
	}
	if err := json.Unmarshal(frame, &resp); err != nil || len(resp.Error) > 0 || resp.Result == nil {
		return proxyIdentity{}, false
	}
	raw, present := resp.Result.Meta[mcp.MetaSessionIdentityKey]
	if !present {
		return proxyIdentity{}, false
	}
	var wire struct {
		SessionID string `json:"session_id"`
		Name      string `json:"name"`
		Revision  int64  `json:"name_revision"`
		Recovery  string `json:"recovery"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return proxyIdentity{}, false
	}
	return proxyIdentity{
		sessionID: wire.SessionID,
		name:      wire.Name,
		revision:  wire.Revision,
		recovery:  wire.Recovery,
		known:     true,
	}, true
}

// isErrorResponse reports whether a frame is a JSON-RPC error response. Anything
// that does not parse is not treated as one — the callers' own shape checks are
// fail-safe, so an unparseable frame contributes nothing either way.
func isErrorResponse(frame []byte) bool {
	var resp struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(frame, &resp); err != nil {
		return false
	}
	return len(resp.Error) > 0
}

// daemonInstanceMeta extracts the daemon's process marker from an initialize
// response frame, or "" when absent or malformed. Fail-safe like its sibling: an
// unreadable marker becomes "unknown", never a false equality.
func daemonInstanceMeta(frame []byte) string {
	var resp struct {
		Result *struct {
			Meta map[string]json.RawMessage `json:"_meta"`
		} `json:"result"`
	}
	if err := json.Unmarshal(frame, &resp); err != nil || resp.Result == nil {
		return ""
	}
	raw, ok := resp.Result.Meta[mcp.MetaDaemonInstanceKey]
	if !ok {
		return ""
	}
	var instance string
	if err := json.Unmarshal(raw, &instance); err != nil {
		return ""
	}
	return instance
}

// observeInitializeResponse folds an initialize response's identity snapshot and
// daemon-process marker into the proxy's state. It is called from both places an
// initialize response is seen — the first connect (resolveResponse) and every
// replay (consumeInitializeResponse) — because an identity captured on only one
// of them is an identity the proxy loses at exactly the wrong moment.
//
// The previous daemon instance is retained alongside the current one so the
// reconnect note can say whether the process actually changed. Comparing
// versions cannot answer that: a restart onto the same build reports the same
// version, and so does an idle eviction that restarted nothing.
func (p *reconnectingProxy) observeInitializeResponse(frame []byte) {
	if isErrorResponse(frame) {
		// A failed initialize is not a handshake with a daemon; it establishes
		// nothing about which process answered, and shifting the markers on it
		// would make the next two reconnect notes report "unknown" for no reason.
		return
	}
	instance := daemonInstanceMeta(frame)
	snapshot, ok := sessionIdentityMeta(frame)

	p.pinMu.Lock()
	defer p.pinMu.Unlock()
	// Shift UNCONDITIONALLY, including an empty marker. Guarding the update on a
	// non-empty value looks harmless and is not: a reconnect onto a daemon that
	// sends no marker would leave the previous PAIR in place, and the next
	// comparison would answer a question about a transition it never observed.
	// Downgrade a daemon mid-session and the note confidently reports the restart
	// from two reconnects ago. An empty marker means "unknown", and unknown has to
	// propagate.
	p.prevDaemonInstance = p.daemonInstance
	p.daemonInstance = instance
	if !ok {
		// A legacy daemon, an error response, or a malformed snapshot. Hold what
		// we have: absence proves nothing, and clearing a valid identity because
		// one response omitted it is how a working session loses its name.
		return
	}
	p.applyIdentityLocked(snapshot)
}

// applyIdentityLocked merges an acknowledged snapshot into the held identity.
// Caller holds pinMu.
//
// A DEGRADED outcome reports a temporary stand-in identity: the daemon found the
// durable record but could not apply it this time (typically the predecessor
// connection had not finished detaching). Recording the stand-in would make the
// proxy replay it, and the transient failure would look increasingly like the
// truth. So the outcome is recorded and the identity is not.
//
// A stale REVISION for the same session is rejected for the mirror-image reason:
// after an explicit rename, an older response still in flight must not put the
// previous name back. A different session ID is not stale, it is a decision the
// daemon made about which identity this connection is, and it is taken as given.
func (p *reconnectingProxy) applyIdentityLocked(snapshot proxyIdentity) {
	if snapshot.recovery == string(recoveryDegraded) && p.identity.known {
		p.identity.recovery = snapshot.recovery
		return
	}
	if p.identity.known && p.identity.sessionID == snapshot.sessionID && snapshot.revision < p.identity.revision {
		p.identity.recovery = snapshot.recovery
		return
	}
	p.identity = snapshot
	if snapshot.sessionID != "" {
		p.heldSessionID = snapshot.sessionID
	}
}

// heldIdentity returns the identity snapshot the daemon last acknowledged.
func (p *reconnectingProxy) heldIdentity() proxyIdentity {
	p.pinMu.Lock()
	defer p.pinMu.Unlock()
	return p.identity
}

// daemonRestarted reports whether the daemon PROCESS changed across the most
// recent reconnect, and whether that question could be answered at all.
//
// known is false whenever either marker is missing — a daemon that predates the
// key on one side of the reconnect, or a proxy whose first handshake was with
// such a daemon. The note then says the honest thing rather than inferring a
// restart from a dropped connection, which is what produced the original
// complaint this work came from.
func (p *reconnectingProxy) daemonRestarted() (restarted, known bool) {
	p.pinMu.Lock()
	defer p.pinMu.Unlock()
	if p.daemonInstance == "" || p.prevDaemonInstance == "" {
		return false, false
	}
	return p.daemonInstance != p.prevDaemonInstance, true
}
