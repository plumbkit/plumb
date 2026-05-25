package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"syscall"
	"time"
)

// Hang detection for the resilient serve proxy (Tier 1).
//
// A crashed daemon is caught by the pumps' read/write errors. A *hung* daemon —
// alive but not responding — needs active probing. The proxy injects MCP `ping`
// requests (which the server answers concurrently, even while a tool call is in
// flight, so a missed pong is a real signal, not a busy daemon) on a reserved id
// namespace and consumes the pongs. A pong that never arrives within the timeout,
// with no other daemon traffic in the meantime, means the daemon is hung: kill it
// and run the ordinary reconnect path, which spawns a fresh one.
//
// Blast radius: the daemon is shared across every connected client, so killing it
// disconnects them all — but a genuine hang already broke every session, and each
// proxy independently recovers onto the fresh daemon. The conservative interval +
// timeout and the "other traffic seen" guard keep false positives rare.

const pingIDPrefix = "__plumb_hb_"

func (p *reconnectingProxy) runHeartbeat(ctx context.Context) {
	ticker := time.NewTicker(p.deps.heartbeatInterval)
	defer ticker.Stop()

	var seq uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if !p.handshakeComplete() {
			continue // nothing to probe until the client has initialised
		}
		seq++
		if p.ping(ctx, fmt.Sprintf("%s%d", pingIDPrefix, seq)) {
			continue
		}
		slog.Warn("serve: daemon heartbeat timed out — assuming hung; killing and reconnecting")
		_, _, gen := p.current()
		if err := p.reconnect(ctx, gen, true); err != nil {
			return // give-up: run() observes the pump error and exits
		}
	}
}

// ping sends one heartbeat request and reports whether the daemon is responsive.
// It returns true (responsive) when the pong arrives, when the context ends, or
// when the write fails (a write failure is a crash, which the read pump handles —
// the daemon must not be double-killed). It returns false only on a genuine
// silent timeout.
func (p *reconnectingProxy) ping(ctx context.Context, id string) bool {
	key := idKey(json.RawMessage(fmt.Sprintf("%q", id)))
	ch := make(chan struct{}, 1)
	p.pongMu.Lock()
	p.pongCh[key] = ch
	p.pongMu.Unlock()
	defer func() {
		p.pongMu.Lock()
		delete(p.pongCh, key)
		p.pongMu.Unlock()
	}()

	before := p.lastRecvNanos.Load()
	frame := fmt.Appendf(nil, `{"jsonrpc":"2.0","id":%q,"method":"ping"}`, id)
	if _, err := p.writeDaemon(frame); err != nil {
		return true // connection already broken; the read pump drives recovery
	}

	select {
	case <-ch:
		return true
	case <-ctx.Done():
		return true
	case <-time.After(p.deps.pingTimeout):
		// A live-but-busy daemon answers ping promptly; only treat it as hung if
		// nothing at all arrived from the daemon since we sent the probe.
		return p.lastRecvNanos.Load() != before
	}
}

// killHungDaemon terminates the daemon recorded in the PID file: SIGTERM, then
// SIGKILL after a grace period. Used only on the hang path; a crashed daemon is
// already gone.
func killHungDaemon() {
	pid := readDaemonPID()
	if pid <= 0 {
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = proc.Signal(syscall.SIGTERM)
	deadline := time.Now().Add(defaultKillGrace)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = proc.Signal(syscall.SIGKILL)
}
