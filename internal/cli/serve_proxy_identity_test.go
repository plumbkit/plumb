package cli

// serve_proxy_identity_test.go — what the proxy may believe about its own
// identity, and on what evidence.
//
// The proxy is the only component that spans a daemon restart, so it is the one
// component whose belief about the connection's identity has to be right. Every
// test here is about a way that belief could be corrupted: by a frame that is
// not an initialize response, by a daemon that says nothing, by a snapshot that
// arrives out of order, or by a temporary identity being mistaken for the real
// one.

import (
	"encoding/json"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
)

// initFrame builds an initialize response carrying an identity snapshot and a
// daemon-process marker, as the daemon emits them.
func initFrame(t *testing.T, id, name string, revision int64, recovery, instance string) []byte {
	t.Helper()
	identity := map[string]any{
		identityMetaRecovery: recovery,
	}
	if id != "" {
		identity[identityMetaSessionID] = id
		identity[identityMetaName] = name
		identity[identityMetaRevision] = revision
	}
	meta := map[string]any{mcp.MetaSessionIdentityKey: identity}
	if instance != "" {
		meta[mcp.MetaDaemonInstanceKey] = instance
	}
	frame, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1,
		"result": map[string]any{
			"protocolVersion": "2024-11-05",
			"serverInfo":      map[string]any{"name": "plumb", "version": "1.2.3"},
			"_meta":           meta,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

// TestProxyIdentity_CapturedFromAnInitializeResponse is the base case: the
// snapshot arrives on the one exchange every connection performs, so the proxy
// holds an identity before any tool call — which is the whole reason the
// handshake was chosen over a session_start result.
func TestProxyIdentity_CapturedFromAnInitializeResponse(t *testing.T) {
	t.Parallel()
	p := &reconnectingProxy{}
	p.observeInitializeResponse(initFrame(t, "sess-1", "calm-stag", 3, string(recoveryRestored), "daemon-a"))

	got := p.heldIdentity()
	if !got.known || got.sessionID != "sess-1" || got.name != "calm-stag" || got.revision != 3 {
		t.Fatalf("identity = %+v, want the acknowledged snapshot", got)
	}
	if got.recovery != string(recoveryRestored) {
		t.Errorf("recovery = %q, want %q", got.recovery, recoveryRestored)
	}
	// The replayed ID follows the snapshot, so the next reconnect carries it.
	if p.sessionID() != "sess-1" {
		t.Errorf("held session ID = %q, want sess-1", p.sessionID())
	}
}

// TestProxyIdentity_NonEvidenceNeverClearsAHeldIdentity. Absence proves nothing:
// a legacy daemon sends no snapshot, an error response carries none, and a
// malformed one cannot be trusted. Treating any of those as "the identity is now
// empty" would lose a working session's name for no reason at all.
func TestProxyIdentity_NonEvidenceNeverClearsAHeldIdentity(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		frame string
	}{
		{"an error response", `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"boom"}}`},
		{"a legacy daemon with no _meta", `{"jsonrpc":"2.0","id":1,"result":{"serverInfo":{"version":"0.1.0"}}}`},
		{"a snapshot of the wrong shape", `{"jsonrpc":"2.0","id":1,"result":{"_meta":{"dev.plumbkit/session-identity":"not-an-object"}}}`},
		{"not json at all", `nonsense`},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p := &reconnectingProxy{}
			p.observeInitializeResponse(initFrame(t, "sess-1", "calm-stag", 1, string(recoveryRestored), "daemon-a"))
			p.observeInitializeResponse([]byte(c.frame))

			got := p.heldIdentity()
			if got.sessionID != "sess-1" || got.name != "calm-stag" {
				t.Fatalf("%s cleared the held identity to %+v; absence of evidence is not "+
					"evidence of a changed identity", c.name, got)
			}
		})
	}
}

// TestProxyIdentity_DegradedOutcomeDoesNotReplaceTheHeldIdentity. A degraded
// recovery reports a TEMPORARY stand-in: the daemon found the durable record and
// could not apply it this time. Recording the stand-in would make the proxy
// replay it, and each reconnect would make the transient failure look more like
// the truth.
func TestProxyIdentity_DegradedOutcomeDoesNotReplaceTheHeldIdentity(t *testing.T) {
	t.Parallel()
	p := &reconnectingProxy{}
	p.observeInitializeResponse(initFrame(t, "sess-1", "calm-stag", 1, string(recoveryRestored), "daemon-a"))
	p.observeInitializeResponse(initFrame(t, "temp-9", "velvet-bison", 1, string(recoveryDegraded), "daemon-b"))

	got := p.heldIdentity()
	if got.sessionID != "sess-1" || got.name != "calm-stag" {
		t.Fatalf("a degraded recovery replaced the held identity with %+v; the temporary "+
			"stand-in must not be adopted as the identity to come back to", got)
	}
	// The OUTCOME is still recorded, because the note has to report it.
	if got.recovery != string(recoveryDegraded) {
		t.Errorf("recovery = %q, want %q — the failure must be reportable even though the "+
			"identity is not adopted", got.recovery, recoveryDegraded)
	}
}

// TestProxyIdentity_StaleRevisionCannotUndoARename. After an explicit rename, an
// older response still in flight must not put the previous name back.
func TestProxyIdentity_StaleRevisionCannotUndoARename(t *testing.T) {
	t.Parallel()
	p := &reconnectingProxy{}
	p.observeInitializeResponse(initFrame(t, "sess-1", "renamed-stag", 5, string(recoveryRestored), "daemon-a"))
	p.observeInitializeResponse(initFrame(t, "sess-1", "calm-stag", 4, string(recoveryRestored), "daemon-a"))

	if got := p.heldIdentity(); got.name != "renamed-stag" || got.revision != 5 {
		t.Fatalf("a stale revision overwrote the newer name: %+v", got)
	}

	// A NEWER revision is applied, or the guard would freeze the name forever.
	p.observeInitializeResponse(initFrame(t, "sess-1", "later-stag", 6, string(recoveryRestored), "daemon-a"))
	if got := p.heldIdentity(); got.name != "later-stag" || got.revision != 6 {
		t.Fatalf("a newer revision was rejected: %+v", got)
	}

	// A DIFFERENT session ID is not staleness, it is the daemon deciding which
	// identity this connection is — take it whatever its revision.
	p.observeInitializeResponse(initFrame(t, "sess-2", "other-stag", 1, string(recoveryRestored), "daemon-a"))
	if got := p.heldIdentity(); got.sessionID != "sess-2" || got.name != "other-stag" {
		t.Fatalf("a decision about a different identity was rejected as stale: %+v", got)
	}
}

// TestProxyIdentity_DaemonRestartIsObservedNotAssumed. This is the fact the
// original incident turned on: an agent was told the daemon had "reconnected",
// read that as a restart, and reported an identity change that never happened.
// The proxy may only distinguish the two when it has a marker from BOTH sides.
func TestProxyIdentity_DaemonRestartIsObservedNotAssumed(t *testing.T) {
	t.Parallel()

	t.Run("one handshake cannot establish anything", func(t *testing.T) {
		t.Parallel()
		p := &reconnectingProxy{}
		p.observeInitializeResponse(initFrame(t, "sess-1", "calm-stag", 1, string(recoveryRestored), "daemon-a"))
		if _, known := p.daemonRestarted(); known {
			t.Error("a single handshake claimed to know whether the daemon restarted; there is " +
				"nothing to compare it against")
		}
	})

	t.Run("the same marker twice is a transport reconnect", func(t *testing.T) {
		t.Parallel()
		p := &reconnectingProxy{}
		p.observeInitializeResponse(initFrame(t, "sess-1", "calm-stag", 1, string(recoveryRestored), "daemon-a"))
		p.observeInitializeResponse(initFrame(t, "sess-1", "calm-stag", 1, string(recoveryRestored), "daemon-a"))
		restarted, known := p.daemonRestarted()
		if !known || restarted {
			t.Errorf("daemonRestarted = (%v, %v), want (false, true) — the same process answered "+
				"twice, which is an idle eviction, not a restart", restarted, known)
		}
	})

	t.Run("a changed marker is a restart", func(t *testing.T) {
		t.Parallel()
		p := &reconnectingProxy{}
		p.observeInitializeResponse(initFrame(t, "sess-1", "calm-stag", 1, string(recoveryRestored), "daemon-a"))
		p.observeInitializeResponse(initFrame(t, "sess-1", "calm-stag", 1, string(recoveryRestored), "daemon-b"))
		restarted, known := p.daemonRestarted()
		if !known || !restarted {
			t.Errorf("daemonRestarted = (%v, %v), want (true, true)", restarted, known)
		}
	})

	t.Run("a daemon that sends no marker leaves it unknown", func(t *testing.T) {
		t.Parallel()
		p := &reconnectingProxy{}
		p.observeInitializeResponse(initFrame(t, "sess-1", "calm-stag", 1, string(recoveryRestored), ""))
		p.observeInitializeResponse(initFrame(t, "sess-1", "calm-stag", 1, string(recoveryRestored), ""))
		if _, known := p.daemonRestarted(); known {
			t.Error("a daemon that reported no process marker was nonetheless compared; version " +
				"equality and connection loss cannot stand in for it")
		}
	})
}

// TestProxyIdentity_NoWorkspaceSessionStartCapturesIDWithoutTouchingThePin is the
// other half of the workspace-gate repair.
//
// Emitting the session ID for a no-workspace session_start (the daemon half) is
// useless if the proxy then treats the result as a pin update, and dangerous if
// it clears the pin. Both facts ride the same response and must stay separate.
func TestProxyIdentity_NoWorkspaceSessionStartCapturesIDWithoutTouchingThePin(t *testing.T) {
	t.Parallel()
	p := &reconnectingProxy{pending: map[string]pendingStart{}}

	// A deliberate pin, established by a session_start that named a workspace.
	p.observeClientRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"session_start","arguments":{"workspace":"/chosen"}}}`))
	p.commitSessionStartPin([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[],"_meta":{"dev.plumbkit/resolved-workspace":"/chosen","dev.plumbkit/session-id":"sess-1"}}}`))
	if got := p.pinnedWorkspace(); got != "/chosen" {
		t.Fatalf("pin = %q, want /chosen", got)
	}

	// An orientation call: no workspace argument at all.
	p.observeClientRequest([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"session_start","arguments":{"session_id":"conv-1"}}}`))
	p.commitSessionStartPin([]byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[],"_meta":{"dev.plumbkit/session-id":"sess-2"}}}`))

	if got := p.sessionID(); got != "sess-2" {
		t.Errorf("held session ID = %q, want sess-2 — a session_start that named no workspace "+
			"still reports the connection's identity, and gating that on the workspace argument "+
			"is what left an orientation-only caller unable to prove itself", got)
	}
	if got := p.pinnedWorkspace(); got != "/chosen" {
		t.Errorf("pin = %q after a workspace-less call, want the untouched /chosen — a call that "+
			"named no workspace must say nothing about the pin", got)
	}
}

// TestProxyIdentity_AFailedSessionStartCommitsNothing: a refused call is not
// evidence of anything, for either fact it might have carried.
func TestProxyIdentity_AFailedSessionStartCommitsNothing(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		frame string
	}{
		{"a JSON-RPC error", `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"refused"}}`},
		{"a tool result flagged isError", `{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[],"_meta":{"dev.plumbkit/session-id":"sess-1","dev.plumbkit/resolved-workspace":"/chosen"}}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p := &reconnectingProxy{pending: map[string]pendingStart{}}
			p.observeClientRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"session_start","arguments":{"workspace":"/chosen"}}}`))
			p.commitSessionStartPin([]byte(c.frame))
			if got := p.sessionID(); got != "" {
				t.Errorf("%s committed a session ID %q", c.name, got)
			}
			if got := p.pinnedWorkspace(); got != "" {
				t.Errorf("%s committed a pin %q", c.name, got)
			}
		})
	}
}

// TestProxyIdentity_OnlyASessionStartIsTracked keeps the pending map from
// growing with every tool call, and keeps an unrelated tool's result from being
// mistaken for identity evidence.
func TestProxyIdentity_OnlyASessionStartIsTracked(t *testing.T) {
	t.Parallel()
	p := &reconnectingProxy{pending: map[string]pendingStart{}}
	p.observeClientRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_file","arguments":{"workspace":"/chosen"}}}`))
	if len(p.pending) != 0 {
		t.Fatalf("a non-session_start call entered the pending map: %v", p.pending)
	}
	// Its response carries an ID-shaped _meta and must still change nothing:
	// arbitrary tool output cannot rewrite proxy identity.
	p.commitSessionStartPin([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[],"_meta":{"dev.plumbkit/session-id":"forged"}}}`))
	if got := p.sessionID(); got != "" {
		t.Fatalf("an unrelated tool result set the session ID to %q", got)
	}
}
