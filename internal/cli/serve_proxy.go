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
	// killDaemon terminates a specific hung daemon PID (SIGTERM→SIGKILL). The
	// proxy passes the PID it connected to, so a hang on one client never kills a
	// different (e.g. freshly-respawned) daemon. Called only on the hang path.
	killDaemon func(pid int)

	// allowDirs are extra read-write roots (--allow-dir / PLUMB_ALLOWED_DIRS)
	// folded into the captured initialize frame's _meta, so they reach the daemon
	// per-connection and ride every handshake replay. Empty ⇒ frame untouched.
	allowDirs []string

	// proxySessionID is this serve proxy's stable per-process session ID, folded
	// into the captured initialize frame's _meta. Identical across every handshake
	// replay, so the daemon recognises a reconnected connection (after a daemon
	// restart) as a continuation and rehydrates its persisted state. Empty ⇒ frame
	// untouched (and the daemon falls back to a fresh, non-rehydrated session).
	proxySessionID string

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
	// daemonVersion is the serverInfo.version the connected daemon reported in
	// its initialize response — the authoritative version for the reconnect
	// note. A long-lived proxy predates an upgraded daemon, so its own compiled
	// Version must never stand in for the daemon's; "" until a handshake
	// response has been seen.
	daemonVersion string

	reqMu       sync.Mutex
	outstanding map[string]outstandingReq

	// reconnected is set when the proxy transparently re-establishes the daemon
	// connection (only ever inside reconnect(), which fires solely on an
	// existing connection's failure — never for the initial connect, so it
	// cannot false-fire for a brand-new client). The first content-bearing tool
	// result after it is set carries a one-shot reconnect note, then it clears.
	reconnected atomic.Bool

	pongMu sync.Mutex
	pongCh map[string]chan struct{}

	lastRecvNanos atomic.Int64

	// daemonPID is the PID of the daemon this proxy is currently connected to,
	// captured from the PID file after each (re)connect. The hang-kill path
	// targets exactly this PID — never "whatever is in the PID file now" — so a
	// second client, or a slow cold-starting replacement daemon, is never killed.
	daemonPID atomic.Int64
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
		outstanding:  make(map[string]outstandingReq),
		pongCh:       make(map[string]chan struct{}),
	}
	p.daemonPID.Store(int64(readDaemonPID()))
	return p
}

// run drives both pumps (and the optional heartbeat) until the client closes
// stdin (clean shutdown, nil) or the context is cancelled. It no longer exits
// when the daemon is unreachable: a prolonged daemon outage (e.g. a
// restart/upgrade window) is ridden out by background reconnect attempts that
// keep the client connection alive, so the host never de-registers the plumb
// tools mid-session.
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
		frame = p.captureHandshake(frame)
		for {
			gen, werr := p.writeDaemon(frame)
			if werr == nil {
				p.trackOutstanding(frame, gen) // only once the frame has actually reached the daemon
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

// captureHandshake records the handshake frames for replay and returns the
// frame to forward — for initialize the _meta-augmented frame (injectInitMeta:
// allow-dirs + the stable proxy session ID), so they reach the daemon on the
// first connect and on every replay. Runs *before* the write so the handshake stays
// replayable even if the daemon dies mid-write; in-flight tracking is not done
// here — see trackOutstanding.
func (p *reconnectingProxy) captureHandshake(frame []byte) []byte {
	e := parseEnvelope(frame)
	switch {
	case e.Method == "initialize" && e.hasID():
		frame = injectInitMeta(frame, buildInitMeta(p.deps.allowDirs, p.deps.proxySessionID))
		p.hsMu.Lock()
		p.initializeFrame = cloneBytes(frame)
		p.initializeID = idKey(e.ID)
		p.hsMu.Unlock()
	case e.Method == "notifications/initialized":
		p.hsMu.Lock()
		p.initializedFrame = cloneBytes(frame)
		p.hsMu.Unlock()
	}
	return frame
}

// outstandingReq is one confirmed-sent, unanswered request: its wire id plus
// the connection generation it was written under. The generation is what lets
// a reconnect sweep distinguish requests sent to a dead daemon (gen < current)
// from requests already re-issued on the fresh connection.
type outstandingReq struct {
	id  json.RawMessage
	gen uint64
}

// trackOutstanding records a request id as in-flight — but only AFTER the frame
// was successfully written to the daemon. Tracking before the write would let a
// reconnect's sweep synthesise a -32000 for a request the pump then re-sends to
// the fresh daemon: a double response, and an auto-replay of a write the "never
// auto-replay" contract forbids. By tracking only confirmed-sent requests, a
// request whose write failed is simply re-sent once (it never reached a
// daemon), while a confirmed-sent request that the daemon dies before answering
// gets exactly one synthesised retryable error. The initialize request is
// excluded — it is resolved by replayHandshake, not the sweep.
//
// Track-after-write leaves one race: the daemon can die — and the reconnect
// sweep run — in the gap between the successful write and the store below,
// which would orphan the request forever (the client hangs until its own
// timeout; reproduced as the proxy-test family's long-standing load flake).
// The post-store generation check closes it: if the connection generation
// advanced past writeGen while we were storing, the entry was written to a
// dead daemon and a sweep may already have missed it — sweep again now.
// Whichever of the two sweeps deletes the entry synthesises the error, so the
// client gets exactly one response either way.
func (p *reconnectingProxy) trackOutstanding(frame []byte, writeGen uint64) {
	e := parseEnvelope(frame)
	if !e.isRequest() {
		return
	}
	key := idKey(e.ID)
	p.hsMu.Lock()
	isInit := key == p.initializeID
	p.hsMu.Unlock()
	if isInit {
		return
	}
	p.reqMu.Lock()
	p.outstanding[key] = outstandingReq{id: cloneBytes(e.ID), gen: writeGen}
	p.reqMu.Unlock()
	if gen := p.generation(); gen != writeGen {
		p.failOutstandingBelow(gen)
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
		p.resolveResponse(key, frame)
		frame = p.annotateReconnect(frame)
	}
	p.writeClient(frame)
}

// annotateReconnect appends a one-shot "daemon reconnected" note to the first
// content-bearing tool result after a transparent reconnect, so a
// silently-changed tool contract (e.g. a rebuilt daemon's new output format) is
// attributable rather than spooky. It is called for every daemon response while
// the flag is set and consumes the flag ONLY when injection actually succeeds —
// so the note lands on a real tool result, not on a ping/initialize/error
// response that happens to be the first frame back. The shape check inside
// injectReconnectNote (a `result.content` array) is the filter, which is why no
// request-id correlation is needed: the response can race ahead of its own
// request being tracked (track-after-write), so id-matching would be unreliable.
//
// pumpDaemonToClient is the sole caller and runs single-threaded, so the
// Load/Store pair needs no CAS.
func (p *reconnectingProxy) annotateReconnect(frame []byte) []byte {
	if !p.reconnected.Load() {
		return frame
	}
	p.hsMu.Lock()
	daemonV := p.daemonVersion
	p.hsMu.Unlock()
	annotated, ok := injectReconnectNote(frame, daemonV, Version)
	if !ok {
		return frame // not a tool result — keep the note armed for the next response
	}
	p.reconnected.Store(false)
	return annotated
}

// resolveResponse marks the initialize handshake answered or de-tracks a
// completed request so it is not error-synthesised on a later reconnect. The
// initialize response also carries the daemon's serverInfo.version, captured
// here so the reconnect note can report the daemon's version rather than this
// proxy's own.
func (p *reconnectingProxy) resolveResponse(key string, frame []byte) {
	p.hsMu.Lock()
	isInit := key == p.initializeID
	if isInit {
		p.initializeAnswered = true
		if v := serverInfoVersion(frame); v != "" {
			p.daemonVersion = v
		}
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
		p.deps.killDaemon(int(p.daemonPID.Load()))
	}
	p.closeCurrent()

	backoff := p.deps.baseBackoff
	failedFast := false
	for attempt := 1; ; attempt++ {
		conn, err := p.deps.dial(ctx)
		if err == nil {
			fr, herr := p.replayHandshake(conn)
			if herr == nil {
				// Arm the one-shot reconnect note BEFORE publishing: publish makes
				// the new connection live to the pumps, so a tool call can be
				// written and its result processed the instant publish returns. Set
				// the flag first so that first post-reconnect result always sees it.
				p.reconnected.Store(true)
				gen := p.publish(conn, fr)
				// Sweep AFTER the generation bump so a request tracked late (the
				// track-after-write gap) is detectable as stale by gen comparison;
				// entries written on the fresh connection carry the new gen and are
				// never swept.
				p.failOutstandingBelow(gen)
				p.daemonPID.Store(int64(readDaemonPID())) // track the PID we are now connected to
				slog.Warn("serve: reconnected to daemon after failure", "attempt", attempt, "generation", gen)
				return nil
			}
			_ = conn.Close()
			err = herr
		}
		slog.Warn("serve: daemon reconnect attempt failed", "attempt", attempt, "error", err)
		// Once the bounded fast phase is exhausted the daemon is still gone (a
		// restart/upgrade window that outlasts the quick retries). Exiting here
		// would close the client's stdio and make the host de-register every
		// plumb tool for the rest of the session — the worst possible outcome and
		// the exact opposite of the proxy's purpose. Instead fail the in-flight
		// requests ONCE (so the client gets a retryable error instead of hanging
		// for the whole outage) and keep retrying in the background at the capped
		// backoff. A prolonged daemon outage becomes a pause, not a session-ending
		// outage: the next tool call succeeds once a daemon is back. Only ctx
		// cancellation (clean shutdown / the client closing stdin) ends the loop.
		if attempt >= p.deps.maxReconnects && !failedFast {
			failedFast = true
			p.failAllOutstanding()
			slog.Warn("serve: daemon unreachable after fast retries — keeping the client connection alive and retrying in the background",
				"attempts", attempt)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > defaultMaxBackoff {
			backoff = defaultMaxBackoff
		}
	}
}

// failOutstandingBelow synthesises a retryable JSON-RPC error for every
// in-flight request written under a connection generation older than gen, so
// the client is never left waiting for a response a dead daemon will never
// send. Requests written on the current connection (gen == current) are left
// alone. The initialize request is excluded — it is resolved by
// replayHandshake.
func (p *reconnectingProxy) failOutstandingBelow(gen uint64) {
	p.reqMu.Lock()
	ids := make([]json.RawMessage, 0, len(p.outstanding))
	for k, req := range p.outstanding {
		if req.gen >= gen {
			continue
		}
		ids = append(ids, req.id)
		delete(p.outstanding, k)
	}
	p.reqMu.Unlock()

	for _, raw := range ids {
		resp := fmt.Sprintf(
			`{"jsonrpc":"2.0","id":%s,"error":{"code":-32000,"message":"plumb daemon restarted mid-request; this request's outcome is unconfirmed — for a write, re-read the file to check whether it landed before retrying"}}`,
			raw)
		p.writeClient([]byte(resp))
	}
}

// failAllOutstanding synthesises the retryable error for EVERY in-flight
// request, whatever its connection generation. Used when the fast reconnect
// phase is exhausted and the proxy drops to slow background retry: the client
// must not stay blocked on an in-flight call for the whole outage.
func (p *reconnectingProxy) failAllOutstanding() {
	p.failOutstandingBelow(p.generation() + 1)
}
