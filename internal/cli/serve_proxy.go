package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Resilient `plumb serve` proxy.
//
// The plain proxyStdio is a raw byte pump: when the daemon dies, the io.Copy
// returns and the serve process exits, killing the client's MCP server.
// reconnectingProxy instead keeps the client's stdio open across a daemon
// crash or hang. It is the only durable anchor in the system — the client owns
// it and it outlives any single daemon — so it transparently respawns/reconnects
// the daemon and replays the MCP handshake on the client's behalf (the client
// sends `initialize` exactly once and never re-sends it).
//
// "Good enough" transparency: the session survives a brief pause; an in-flight
// tool call at the moment of failure receives one synthesised retryable error;
// per-session daemon state (read-tracking, caches) is rebuilt, not preserved.
//
// Concurrency: two pumps run concurrently — pumpClientToDaemon and
// pumpDaemonToClient — plus an optional heartbeat goroutine. The live daemon
// connection (conn/fr/gen) is guarded by connMu; reconnect is serialised by
// reconnectMu and is idempotent per generation; writes to stdout are serialised
// by outMu and writes to the daemon by daemonWriteMu. No lock is ever held
// across blocking I/O.

const (
	defaultMaxReconnects = 10
	defaultBaseBackoff   = 200 * time.Millisecond
	defaultMaxBackoff    = 5 * time.Second
	defaultHandshakeWait = 10 * time.Second

	defaultHeartbeatInterval = 30 * time.Second
	defaultPingTimeout       = 10 * time.Second
	defaultKillGrace         = 2 * time.Second
)

// proxyDeps bundles the reconnectingProxy's collaborators so production wiring
// and tests construct it the same way.
type proxyDeps struct {
	in      io.Reader
	out     io.Writer
	initial net.Conn
	// dial establishes a fresh daemon connection (dialing the socket or spawning
	// a daemon). It is called on every reconnect.
	dial func(ctx context.Context) (net.Conn, error)
	// killDaemon terminates a hung daemon (SIGTERM→SIGKILL via the PID file).
	// Called only on the heartbeat-detected hang path.
	killDaemon func()

	heartbeatInterval time.Duration // 0 disables hang detection
	pingTimeout       time.Duration
	maxReconnects     int
	handshakeWait     time.Duration
	baseBackoff       time.Duration
}

type reconnectingProxy struct {
	deps         proxyDeps
	clientReader *frameReader

	connMu sync.Mutex
	conn   net.Conn
	fr     *frameReader
	gen    uint64

	reconnectMu   sync.Mutex
	daemonWriteMu sync.Mutex
	outMu         sync.Mutex

	hsMu               sync.Mutex
	initializeFrame    []byte
	initializeID       string
	initializedFrame   []byte
	initializeAnswered bool

	reqMu       sync.Mutex
	outstanding map[string]json.RawMessage

	pongMu sync.Mutex
	pongCh map[string]chan struct{}

	lastRecvNanos atomic.Int64
}

func newReconnectingProxy(deps proxyDeps) *reconnectingProxy {
	if deps.maxReconnects <= 0 {
		deps.maxReconnects = defaultMaxReconnects
	}
	if deps.handshakeWait <= 0 {
		deps.handshakeWait = defaultHandshakeWait
	}
	if deps.pingTimeout <= 0 {
		deps.pingTimeout = defaultPingTimeout
	}
	if deps.baseBackoff <= 0 {
		deps.baseBackoff = defaultBaseBackoff
	}
	p := &reconnectingProxy{
		deps:         deps,
		clientReader: newFrameReader(deps.in),
		conn:         deps.initial,
		fr:           newFrameReader(deps.initial),
		gen:          1,
		outstanding:  make(map[string]json.RawMessage),
		pongCh:       make(map[string]chan struct{}),
	}
	return p
}

// run drives both pumps (and the optional heartbeat) until the client closes
// stdin (clean shutdown, nil), the context is cancelled, or the daemon becomes
// unreachable after the bounded retries (returns the give-up error, so the
// process exits exactly as the legacy proxy would).
func (p *reconnectingProxy) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, 2)
	go func() { errCh <- p.pumpClientToDaemon(ctx) }()
	go func() { errCh <- p.pumpDaemonToClient(ctx) }()
	if p.deps.heartbeatInterval > 0 {
		go p.runHeartbeat(ctx)
	}

	select {
	case <-ctx.Done():
		p.closeCurrent()
		return nil
	case err := <-errCh:
		cancel()
		p.closeCurrent()
		return err
	}
}

func (p *reconnectingProxy) current() (net.Conn, *frameReader, uint64) {
	p.connMu.Lock()
	defer p.connMu.Unlock()
	return p.conn, p.fr, p.gen
}

func (p *reconnectingProxy) generation() uint64 {
	p.connMu.Lock()
	defer p.connMu.Unlock()
	return p.gen
}

func (p *reconnectingProxy) closeCurrent() {
	p.connMu.Lock()
	conn := p.conn
	p.conn = nil
	p.fr = nil
	p.connMu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func (p *reconnectingProxy) publish(conn net.Conn, fr *frameReader) uint64 {
	p.connMu.Lock()
	defer p.connMu.Unlock()
	p.conn = conn
	p.fr = fr
	p.gen++
	return p.gen
}

// pumpClientToDaemon forwards client frames to the daemon, capturing the
// handshake and tracking in-flight request ids. A write failure triggers a
// reconnect and the frame is retried against the fresh connection.
func (p *reconnectingProxy) pumpClientToDaemon(ctx context.Context) error {
	for {
		frame, err := p.clientReader.read()
		if err != nil {
			return nil // client closed stdin — normal end of session
		}
		p.captureClientFrame(frame)
		for {
			gen, werr := p.writeDaemon(frame)
			if werr == nil {
				break
			}
			if ctx.Err() != nil {
				return nil
			}
			if rerr := p.reconnect(ctx, gen, false); rerr != nil {
				return rerr
			}
		}
	}
}

// pumpDaemonToClient forwards daemon frames to the client, swallowing heartbeat
// pongs and de-tracking answered requests. A read failure triggers a reconnect
// and the loop resumes on the fresh connection.
func (p *reconnectingProxy) pumpDaemonToClient(ctx context.Context) error {
	for {
		_, fr, gen := p.current()
		if fr == nil {
			if ctx.Err() != nil {
				return nil
			}
			if rerr := p.reconnect(ctx, gen, false); rerr != nil {
				return rerr
			}
			continue
		}
		frame, err := fr.read()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if rerr := p.reconnect(ctx, gen, false); rerr != nil {
				return rerr
			}
			continue
		}
		p.handleDaemonFrame(frame)
	}
}

func (p *reconnectingProxy) writeDaemon(frame []byte) (uint64, error) {
	p.daemonWriteMu.Lock()
	defer p.daemonWriteMu.Unlock()
	conn, _, gen := p.current()
	if conn == nil {
		return gen, net.ErrClosed
	}
	if err := writeFrame(conn, frame); err != nil {
		return gen, err
	}
	return gen, nil
}

func (p *reconnectingProxy) writeClient(frame []byte) {
	p.outMu.Lock()
	defer p.outMu.Unlock()
	_ = writeFrame(p.out(), frame)
}

func (p *reconnectingProxy) out() io.Writer { return p.deps.out }

func (p *reconnectingProxy) captureClientFrame(frame []byte) {
	e := parseEnvelope(frame)
	switch {
	case e.Method == "initialize" && e.hasID():
		p.hsMu.Lock()
		p.initializeFrame = cloneBytes(frame)
		p.initializeID = idKey(e.ID)
		p.hsMu.Unlock()
	case e.Method == "notifications/initialized":
		p.hsMu.Lock()
		p.initializedFrame = cloneBytes(frame)
		p.hsMu.Unlock()
	case e.isRequest():
		p.reqMu.Lock()
		p.outstanding[idKey(e.ID)] = cloneBytes(e.ID)
		p.reqMu.Unlock()
	}
}

func (p *reconnectingProxy) handleDaemonFrame(frame []byte) {
	p.lastRecvNanos.Store(time.Now().UnixNano())
	e := parseEnvelope(frame)
	if e.isResponse() {
		key := idKey(e.ID)
		if p.deliverPong(key) {
			return // heartbeat pong — never forwarded to the client
		}
		p.resolveResponse(key)
	}
	p.writeClient(frame)
}

// resolveResponse marks the initialize handshake answered or de-tracks a
// completed request so it is not error-synthesised on a later reconnect.
func (p *reconnectingProxy) resolveResponse(key string) {
	p.hsMu.Lock()
	isInit := key == p.initializeID
	if isInit {
		p.initializeAnswered = true
	}
	p.hsMu.Unlock()
	if !isInit {
		p.reqMu.Lock()
		delete(p.outstanding, key)
		p.reqMu.Unlock()
	}
}

func (p *reconnectingProxy) deliverPong(key string) bool {
	p.pongMu.Lock()
	ch, ok := p.pongCh[key]
	p.pongMu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- struct{}{}:
	default:
	}
	return true
}

// reconnect replaces the daemon connection. failedGen is the generation the
// caller saw fail; if the live generation has already moved past it another
// goroutine reconnected and this is a no-op (idempotent coalescing). When kill
// is true the stuck daemon is terminated first (hang path).
func (p *reconnectingProxy) reconnect(ctx context.Context, failedGen uint64, kill bool) error {
	p.reconnectMu.Lock()
	defer p.reconnectMu.Unlock()

	if p.generation() != failedGen {
		return nil // another goroutine already reconnected past this generation
	}
	if kill && p.deps.killDaemon != nil {
		p.deps.killDaemon()
	}
	p.closeCurrent()

	backoff := p.deps.baseBackoff
	for attempt := 1; attempt <= p.deps.maxReconnects; attempt++ {
		conn, err := p.deps.dial(ctx)
		if err == nil {
			fr, herr := p.replayHandshake(conn)
			if herr == nil {
				p.failOutstanding()
				gen := p.publish(conn, fr)
				slog.Warn("serve: reconnected to daemon after failure", "attempt", attempt, "generation", gen)
				return nil
			}
			_ = conn.Close()
			err = herr
		}
		slog.Warn("serve: daemon reconnect attempt failed", "attempt", attempt, "error", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > defaultMaxBackoff {
			backoff = defaultMaxBackoff
		}
	}
	return fmt.Errorf("plumb serve: daemon unreachable after %d reconnect attempts", p.deps.maxReconnects)
}

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
			p.hsMu.Unlock()
			if !answered {
				p.writeClient(frame)
			}
			return nil
		}
		p.writeClient(frame) // an unrelated notification arrived mid-handshake
	}
}

// failOutstanding synthesises a retryable JSON-RPC error for every in-flight
// request so the client is never left waiting for a response the dead daemon
// will never send. The initialize request is excluded — it is resolved by
// replayHandshake.
func (p *reconnectingProxy) failOutstanding() {
	p.reqMu.Lock()
	ids := make([]json.RawMessage, 0, len(p.outstanding))
	for k, raw := range p.outstanding {
		ids = append(ids, raw)
		delete(p.outstanding, k)
	}
	p.reqMu.Unlock()

	for _, raw := range ids {
		resp := fmt.Sprintf(
			`{"jsonrpc":"2.0","id":%s,"error":{"code":-32000,"message":"plumb daemon restarted; please retry"}}`,
			raw)
		p.writeClient([]byte(resp))
	}
}

func (p *reconnectingProxy) handshakeComplete() bool {
	p.hsMu.Lock()
	defer p.hsMu.Unlock()
	return p.initializeFrame != nil
}
