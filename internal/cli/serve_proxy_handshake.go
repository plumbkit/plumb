package cli

import (
	"fmt"
	"net"
	"time"
)

// Handshake replay for the resilient `plumb serve` proxy. The client sends
// `initialize` exactly once; on every daemon reconnect the proxy re-establishes
// the MCP session by replaying the captured (and, when --allow-dir is set,
// allow-dirs-augmented) initialize frame, so a respawned daemon comes up with
// the same session contract the client believes it already negotiated.

// replayHandshake re-establishes the MCP session on a fresh connection by
// resending the captured initialize request, consuming its response, then
// resending the initialized notification. It owns the connection exclusively
// (no pump reads it until publish), so reading the handshake response here
// races with nothing. The returned reader is handed to the daemon pump so it
// continues from any bytes already buffered.
func (p *reconnectingProxy) replayHandshake(conn net.Conn) (*frameReader, error) {
	fr := newFrameReader(conn)

	p.hsMu.Lock()
	initFrame := p.initializeFrame
	initID := p.initializeID
	initzFrame := p.initializedFrame
	p.hsMu.Unlock()

	if initFrame == nil {
		return fr, nil // client has not completed the handshake yet
	}
	// Layer the caller's chosen workspace onto the captured frame. It cannot be
	// baked in at capture time: the pin is learned from a session_start that
	// happens long after the handshake. With no pin the frame is unchanged.
	initFrame = injectInitMeta(initFrame, p.replayInitMeta())
	if err := writeFrame(conn, initFrame); err != nil {
		return nil, fmt.Errorf("replaying initialize: %w", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(p.deps.handshakeWait))
	if err := p.consumeInitializeResponse(fr, initID); err != nil {
		return nil, err
	}
	_ = conn.SetReadDeadline(time.Time{})

	if initzFrame != nil {
		if err := writeFrame(conn, initzFrame); err != nil {
			return nil, fmt.Errorf("replaying initialized: %w", err)
		}
	}
	return fr, nil
}

// consumeInitializeResponse reads frames until the replayed initialize response
// arrives. The response is swallowed (the client already received its
// initialize result from the original daemon) unless the client never got one
// because the daemon died mid-handshake, in which case it is forwarded.
func (p *reconnectingProxy) consumeInitializeResponse(fr *frameReader, initID string) error {
	for {
		frame, err := fr.read()
		if err != nil {
			return fmt.Errorf("awaiting initialize response: %w", err)
		}
		e := parseEnvelope(frame)
		if e.isResponse() && idKey(e.ID) == initID {
			p.hsMu.Lock()
			answered := p.initializeAnswered
			p.initializeAnswered = true
			// The replayed response comes from the freshly started daemon — the
			// version the upcoming reconnect note must report. Adopt it
			// *unconditionally*, including "" for a legacy daemon with no
			// serverInfo: a modern→legacy replacement must fall back to the proxy
			// version in the note, not keep reporting the dead modern daemon's
			// stale version.
			p.daemonVersion = serverInfoVersion(frame)
			p.hsMu.Unlock()
			if !answered {
				p.writeClient(frame)
			} else {
				// A reconnect: the client already has its initialize result, and
				// re-sending it would be a duplicate response to a settled id. It
				// does need to re-list tools, but that notification is emitted at
				// the END of the reconnect (see reconnect()), not here — inserting
				// a frame mid-handshake puts it ahead of the synthesised errors
				// for in-flight requests, and a client waiting on its own response
				// should not have to read past an unrelated notification first.
				p.relistOnReconnect.Store(true)
			}
			return nil
		}
		p.writeClient(frame) // an unrelated notification arrived mid-handshake
	}
}

func (p *reconnectingProxy) handshakeComplete() bool {
	p.hsMu.Lock()
	defer p.hsMu.Unlock()
	return p.initializeFrame != nil
}

// notifyToolsListChanged tells the client to re-list tools, as a server-initiated
// MCP notification with no id (it expects no response).
//
// The daemon advertises tools.listChanged=true during initialize and fires this
// itself when a connection's tool PROFILE changes. The proxy has to fire it too,
// for the case the daemon cannot see: a restart swaps the daemon out entirely, so
// the connection that would have noticed no longer exists, while the client's
// view of the server — this proxy — persists across the gap.
func (p *reconnectingProxy) notifyToolsListChanged() {
	p.writeClient([]byte(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`))
}
