// Package jsonrpc implements a JSON-RPC 2.0 client with LSP content framing
// (Content-Length headers over stdio).
package jsonrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// slowCallThreshold is the round-trip latency above which a request is logged
// at WARN. A warm gopls answers queries in well under a second; a multi-second
// round-trip signals the server is still indexing or otherwise saturated,
// which is the most common cause of an LSP tool appearing to hang.
const slowCallThreshold = 2 * time.Second

// Caller abstracts the JSON-RPC connection so adapters can be tested with a
// mock without spawning a real process.
// Concurrency: all methods must be safe for concurrent use.
type Caller interface {
	Call(ctx context.Context, method string, params, result any) error
	Notify(ctx context.Context, method string, params any) error
	SetNotificationHandler(fn func(method string, params json.RawMessage))
	SetRequestHandler(fn RequestHandler)
	Close() error
}

// RequestHandler is invoked when the server initiates a request. It must
// return a JSON-RPC result (or error) which is sent back to the server.
// Return (nil, nil) to send an empty success response.
type RequestHandler func(ctx context.Context, method string, params json.RawMessage) (result any, err error)

// jsonRPCError is sent in error responses. Code -32601 is method-not-found
// per the JSON-RPC 2.0 spec.
const (
	errCodeMethodNotFound = -32601
	errCodeInternal       = -32603
)

// ─── wire types ──────────────────────────────────────────────────────────────

// wireMessage is a JSON-RPC 2.0 message on the wire.
//
// ID is json.RawMessage rather than *int64 because the spec allows string IDs
// and some servers (jdtls sends "1" for client/registerCapability) use them.
// We use the raw JSON bytes as a map key; our own Call() always sends integer
// IDs so there is no ambiguity when matching responses to outbound calls.
type wireMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *wireError      `json:"error,omitempty"`
}

type wireError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *wireError) Error() string {
	return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message)
}

// ─── pending ─────────────────────────────────────────────────────────────────

type pending struct {
	ch  chan wireMessage
	ctx context.Context //nolint:containedctx
}

// ─── Conn ────────────────────────────────────────────────────────────────────

// Conn is a JSON-RPC 2.0 connection using LSP content framing.
// Concurrency: all exported methods are safe for concurrent use.
type Conn struct {
	wrMu   sync.Mutex
	writer io.Writer

	nextID  atomic.Int64
	pending sync.Map // int64 → *pending

	notifyMu sync.RWMutex
	onNotify func(method string, params json.RawMessage)

	reqMu     sync.RWMutex
	onRequest RequestHandler

	// notifyQ carries server notifications to a single delivery goroutine so they
	// reach the handler in wire order (see notifyLoop).
	notifyQ chan notifyItem

	done chan struct{}
	once sync.Once
}

// notifyItem is one server notification queued for in-order delivery.
type notifyItem struct {
	method string
	params json.RawMessage
}

// notifyQueueSize bounds buffered server notifications. The handler (e.g. the
// diagnostics cache invalidator) is fast, so this is generous headroom; if it
// ever fills, the read loop applies backpressure rather than spawning unbounded
// goroutines.
const notifyQueueSize = 1024

// NewConn creates a Conn over r/w and starts the read loop.
// The caller owns the lifecycle of r and w; close them after calling Close.
func NewConn(r io.Reader, w io.Writer) *Conn {
	c := &Conn{
		writer:  w,
		notifyQ: make(chan notifyItem, notifyQueueSize),
		done:    make(chan struct{}),
	}
	go c.readLoop(bufio.NewReader(r))
	go c.notifyLoop()
	return c
}

// SetNotificationHandler registers fn to be called for every server-initiated
// notification. Only one handler is active at a time. fn is invoked serially,
// in wire order, from a single delivery goroutine (see notifyLoop), so it must
// not block — in particular it must not make a blocking Call back into this
// Conn, which would stall all later notifications.
func (c *Conn) SetNotificationHandler(fn func(method string, params json.RawMessage)) {
	c.notifyMu.Lock()
	c.onNotify = fn
	c.notifyMu.Unlock()
}

// SetRequestHandler registers fn to be called for every server-initiated
// request (a message with an ID and a method, expecting a response). The
// handler's return value is encoded as the response result. Returning an
// error sends an error response with code -32603 (internal error) unless
// the error is a *MethodNotFoundError, in which case code -32601 is used.
//
// If no handler is set, server requests are responded to with method-not-found.
func (c *Conn) SetRequestHandler(fn RequestHandler) {
	c.reqMu.Lock()
	c.onRequest = fn
	c.reqMu.Unlock()
}

// RequestHandler returns the currently-registered server-request handler, or
// nil if none is set. It lets a caller (the pool) layer extra server-request
// handling — e.g. workspace/diagnostic/refresh — IN FRONT of an adapter's
// handler by grabbing the adapter's handler, wrapping it, and re-setting the
// wrapper, so refresh is wired in one place without every adapter threading an
// extension through its own registration.
func (c *Conn) RequestHandler() RequestHandler {
	c.reqMu.RLock()
	defer c.reqMu.RUnlock()
	return c.onRequest
}

// MethodNotFoundError can be returned from a RequestHandler to send the
// JSON-RPC method-not-found error code (-32601) back to the server.
type MethodNotFoundError struct{ Method string }

func (e *MethodNotFoundError) Error() string {
	return fmt.Sprintf("method not found: %s", e.Method)
}

// IsMethodNotFound reports whether err is, or wraps, a JSON-RPC
// method-not-found (-32601) error: either a *MethodNotFoundError this process
// raised, or an error response a server returned with code -32601. Adapters
// wrap the raw Call error with fmt.Errorf("... %w", err), so the check unwraps.
// Callers use it to detect that a negotiated pull-diagnostics request is
// unsupported by the server and downgrade the connection to push.
func IsMethodNotFound(err error) bool {
	if err == nil {
		return false
	}
	var mnf *MethodNotFoundError
	if errorsAs(err, &mnf) {
		return true
	}
	var we *wireError
	if errorsAs(err, &we) {
		return we.Code == errCodeMethodNotFound
	}
	return false
}

// Call sends a request and blocks until a response arrives or ctx is cancelled.
// result is JSON-decoded from the response; pass nil if not needed.
func (c *Conn) Call(ctx context.Context, method string, params, result any) error {
	if err := ctx.Err(); err != nil {
		return err // already cancelled — don't put a request on the wire only to cancel it
	}
	rawID := json.RawMessage(strconv.FormatInt(c.nextID.Add(1), 10))
	idKey := string(rawID)

	encoded, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("jsonrpc call %s: marshaling params: %w", method, err)
	}

	ch := make(chan wireMessage, 1)
	c.pending.Store(idKey, &pending{ch: ch, ctx: ctx})
	defer c.pending.Delete(idKey)

	// Send the request WITHOUT aborting the write on ctx — cancellation is handled
	// solely by the response-wait select below, which emits $/cancelRequest. Watching
	// ctx on the write raced: when the write completed and ctx was cancelled in the
	// same instant, sendCtx's select could pick ctx.Done() over the just-completed
	// write, return ctx.Err() before the response-wait select ran, and so never emit
	// the $/cancelRequest the server needs to abandon the now-in-flight request — a
	// CI-only deadlock the cancel test hit only under scheduling load. The write stays
	// bounded by writeStallTimeout and c.done inside sendCtx.
	if err := c.sendCtx(context.Background(), wireMessage{
		JSONRPC: "2.0",
		ID:      rawID,
		Method:  method,
		Params:  encoded,
	}); err != nil {
		return err
	}

	start := time.Now()
	defer func() {
		if d := time.Since(start); d > slowCallThreshold {
			slog.Warn("jsonrpc: slow call", "method", method, "elapsed", d.Round(time.Millisecond))
		}
	}()

	select {
	case <-ctx.Done():
		c.cancelRequest(rawID)
		return ctx.Err()
	case <-c.done:
		return fmt.Errorf("jsonrpc: connection closed")
	case resp := <-ch:
		if resp.Error != nil {
			return resp.Error
		}
		if result != nil && len(resp.Result) > 0 && string(resp.Result) != "null" {
			if err := json.Unmarshal(resp.Result, result); err != nil {
				return fmt.Errorf("jsonrpc call %s: decoding result: %w", method, err)
			}
		}
		return nil
	}
}

// cancelRequest sends an LSP $/cancelRequest notification for id, telling the
// server to abandon an in-flight request whose context was cancelled so it
// does not keep computing a result we will discard. Best effort: a send error
// means the connection is already dying, which the read loop will surface.
func (c *Conn) cancelRequest(id json.RawMessage) {
	params, err := json.Marshal(map[string]json.RawMessage{"id": id})
	if err != nil {
		return
	}
	_ = c.sendCtx(context.Background(), wireMessage{JSONRPC: "2.0", Method: "$/cancelRequest", Params: params})
}

// Notify sends a notification (no ID, no response expected).
//
// The write is performed in a goroutine so that a stalled language-server
// pipe (e.g. the server is saturated by a large analysis and not draining
// stdin) cannot block the caller indefinitely. If ctx is cancelled or the
// connection closes before the write completes, Notify returns the context
// error; the write goroutine continues in the background and will finish
// once the server reads from its stdin buffer.
func (c *Conn) Notify(ctx context.Context, method string, params any) error {
	encoded, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("jsonrpc notify %s: marshaling params: %w", method, err)
	}
	msg := wireMessage{JSONRPC: "2.0", Method: method, Params: encoded}
	return c.sendCtx(ctx, msg)
}

// Close signals the connection to stop. It does not close the underlying
// io.Reader/Writer — the caller is responsible for that.
func (c *Conn) Close() error {
	c.once.Do(func() { close(c.done) })
	return nil
}

// writeStallTimeout bounds a single write to the language server's stdin. A pipe
// write blocks only when the OS buffer is full and the server has stopped
// draining it — a wedged (not crashed) server. Kept distinctly below the smallest
// expected request deadline (the default [lsp_query] timeout is 30s) so a stalled
// write is detected and the connection torn down *before* the request deadline
// fires — otherwise an equal 30s/30s pair would race on which trips first. A var,
// not a const, so tests can lower it.
var writeStallTimeout = 15 * time.Second

// sendCtx writes msg without ever blocking the caller indefinitely on a stalled
// language-server pipe. The raw write runs in a goroutine; the caller returns
// early if ctx is cancelled or the connection closes. Crucially, if the write
// itself stalls past writeStallTimeout the server is wedged, so the connection
// is closed — unblocking every pending call with a clear error and letting the
// pool tear it down — rather than leaving the held write lock to wedge every
// future call (which no ctx deadline could rescue, since acquiring the lock
// precedes the ctx-aware select).
func (c *Conn) sendCtx(ctx context.Context, msg wireMessage) error {
	select {
	case <-c.done:
		return fmt.Errorf("jsonrpc: connection closed")
	default:
	}
	errc := make(chan error, 1)
	go func() { errc <- c.send(msg) }()
	timer := time.NewTimer(writeStallTimeout)
	defer timer.Stop()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return fmt.Errorf("jsonrpc: connection closed")
	case <-timer.C:
		slog.Warn("jsonrpc: write stalled — closing connection", "method", msg.Method, "timeout", writeStallTimeout)
		_ = c.Close()
		return fmt.Errorf("jsonrpc: write stalled after %s, connection closed", writeStallTimeout)
	}
}

func (c *Conn) send(msg wireMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("jsonrpc: marshaling message: %w", err)
	}
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))

	c.wrMu.Lock()
	defer c.wrMu.Unlock()
	if _, err := io.WriteString(c.writer, header); err != nil {
		return fmt.Errorf("jsonrpc: writing header: %w", err)
	}
	if _, err := c.writer.Write(data); err != nil {
		return fmt.Errorf("jsonrpc: writing body: %w", err)
	}
	return nil
}

func (c *Conn) readLoop(r *bufio.Reader) {
	for {
		select {
		case <-c.done:
			return
		default:
		}

		msg, err := readMessage(r)
		if err != nil {
			// EOF or broken pipe — signal done so pending calls unblock.
			c.once.Do(func() { close(c.done) })
			return
		}
		c.dispatch(msg)
	}
}

func (c *Conn) dispatch(msg wireMessage) {
	// A message has an ID when it is either a response to one of our calls or a
	// server-initiated request. The spec allows string or integer IDs; we store
	// the raw JSON as the map key so both forms round-trip without conversion.
	if len(msg.ID) > 0 && string(msg.ID) != "null" {
		if v, ok := c.pending.Load(string(msg.ID)); ok {
			p := v.(*pending)
			select {
			case p.ch <- msg:
			case <-p.ctx.Done():
			case <-c.done:
			}
			return
		}
		// Server-initiated request — handle in a goroutine so the read loop
		// can keep draining the wire.
		if msg.Method != "" {
			go c.handleServerRequest(msg)
		}
		return
	}
	// Server-initiated notification.
	if msg.Method == "" {
		return
	}
	// Queue for in-order, single-goroutine delivery. Spawning a goroutine per
	// notification raced: an out-of-order publishDiagnostics for a URI could leave
	// the stale set winning, and a flood spawned unbounded goroutines.
	select {
	case c.notifyQ <- notifyItem{method: msg.Method, params: msg.Params}:
	case <-c.done:
	}
}

// notifyLoop delivers queued server notifications to the registered handler one
// at a time, in the order they arrived on the wire. A single delivery goroutine
// (rather than one per notification) preserves ordering — critical for
// publishDiagnostics, where a later set must not be overtaken by an earlier one
// — and bounds goroutine growth under a notification flood.
func (c *Conn) notifyLoop() {
	for {
		select {
		case <-c.done:
			return
		case it := <-c.notifyQ:
			c.notifyMu.RLock()
			fn := c.onNotify
			c.notifyMu.RUnlock()
			if fn != nil {
				fn(it.method, it.params)
			}
		}
	}
}

// handleServerRequest dispatches one incoming server request through the
// registered RequestHandler and sends back a response. Runs in its own
// goroutine so long-running handlers don't stall the read loop.
func (c *Conn) handleServerRequest(req wireMessage) {
	c.reqMu.RLock()
	fn := c.onRequest
	c.reqMu.RUnlock()

	var (
		result any
		err    error
	)
	if fn == nil {
		err = &MethodNotFoundError{Method: req.Method}
	} else {
		result, err = fn(context.Background(), req.Method, req.Params)
	}

	resp := wireMessage{
		JSONRPC: "2.0",
		ID:      req.ID,
	}
	if err != nil {
		code := errCodeInternal
		var mnf *MethodNotFoundError
		if errorsAs(err, &mnf) {
			code = errCodeMethodNotFound
		}
		resp.Error = &wireError{Code: code, Message: err.Error()}
	} else {
		if result == nil {
			resp.Result = json.RawMessage("null")
		} else {
			encoded, mErr := json.Marshal(result)
			if mErr != nil {
				resp.Error = &wireError{Code: errCodeInternal, Message: "encoding result: " + mErr.Error()}
			} else {
				resp.Result = encoded
			}
		}
	}
	if err := c.sendCtx(context.Background(), resp); err != nil {
		// We can't do anything useful here; the connection is dying. The
		// read loop's EOF handler will close c.done shortly.
		_ = err
	}
}

// errorsAs is a tiny indirection so we can avoid importing "errors" in this
// hot file just for As. Inlined As semantics.
func errorsAs[T any](err error, target *T) bool {
	for e := err; e != nil; {
		if v, ok := e.(T); ok {
			*target = v
			return true
		}
		type unwrapper interface{ Unwrap() error }
		if u, ok := e.(unwrapper); ok {
			e = u.Unwrap()
			continue
		}
		break
	}
	return false
}

// readMessage reads one LSP-framed JSON-RPC message from r.
func readMessage(r *bufio.Reader) (wireMessage, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return wireMessage{}, fmt.Errorf("reading header: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if after, ok := strings.CutPrefix(line, "Content-Length: "); ok {
			n, err := strconv.Atoi(after)
			if err != nil {
				return wireMessage{}, fmt.Errorf("parsing Content-Length: %w", err)
			}
			length = n
		}
	}
	if length < 0 {
		return wireMessage{}, fmt.Errorf("missing Content-Length header")
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return wireMessage{}, fmt.Errorf("reading body: %w", err)
	}
	var msg wireMessage
	if err := json.Unmarshal(buf, &msg); err != nil {
		return wireMessage{}, fmt.Errorf("parsing message body: %w", err)
	}
	return msg, nil
}
