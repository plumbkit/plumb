// Package mcp implements a Model Context Protocol server over stdio.
// Transport: newline-delimited JSON-RPC 2.0 (not LSP Content-Length framing).
// Protocol version: 2024-11-05.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

const protocolVersion = "2024-11-05"

const maxMessageBytes = 4 << 20 // 4 MiB per newline-delimited JSON-RPC message

// DefaultWriteTimeout bounds a single response write to the transport. On a
// net.Conn (the daemon's Unix socket) a blocked write would otherwise hold the
// per-connection write mutex forever and wedge every later reply on that
// connection. 30s is far longer than any healthy local write yet decisively
// shorter than a client's request timeout, so a genuinely stuck write fails
// fast and tears the connection down (the resilient proxy then reconnects)
// instead of hanging to the client timeout. Transports that do not support
// SetWriteDeadline (e.g. test pipes) are unaffected. 0 disables the deadline.
const DefaultWriteTimeout = 30 * time.Second

// JSON-RPC 2.0 standard error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

// RequestFn sends a server-initiated JSON-RPC request to the MCP client and
// returns the decoded result payload, or an error if the call fails or times out.
type RequestFn func(ctx context.Context, method string, params any) (json.RawMessage, error)

// ─── wire types ──────────────────────────────────────────────────────────────

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"` // string | number | null
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *mcpError `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// scanLine carries one message from the reader goroutine to the main loop.
type scanLine struct {
	data     []byte
	err      error
	tooLarge bool
}

// ─── Server ──────────────────────────────────────────────────────────────────

// ServerInfo identifies this server to the MCP client.
type ServerInfo struct {
	Name    string
	Version string
	// Instructions is included in the MCP initialize response. When non-empty
	// it overrides DefaultInstructions; set to "-" to send no instructions.
	Instructions string
}

// Server is an MCP server. Register tools, then call Serve.
//
// OnInit, if set, is called once in a goroutine after a successful
// initialize exchange. It receives a RequestFn the callback can use to make
// requests back to the MCP client (e.g. roots/list).
//
// OnRootsChanged, if set, is called in a goroutine each time the client sends
// a notifications/roots/listChanged notification.
//
// Concurrency: Register and setting callbacks must finish before Serve is called.
// Serve handles individual requests concurrently.
type Server struct {
	info      ServerInfo
	mu        sync.RWMutex
	tools     map[string]Tool
	argShapes map[string]*shape // parsed argument contract per tool; nil when unguardable
	order     []string          // insertion order for tools/list

	// OnInit is called once after a successful MCP initialize exchange.
	OnInit func(ctx context.Context, request RequestFn)

	// OnRootsChanged is called each time the client notifies that its roots changed.
	OnRootsChanged func(ctx context.Context, request RequestFn)

	// OnBeforeTool is called synchronously before each tools/call execution.
	OnBeforeTool func(ctx context.Context, name string, args json.RawMessage)

	// OnAfterTool is called synchronously after each tools/call execution.
	// output is the tool's text result (empty when isError is true). errMsg
	// is the error string (empty when the call succeeded). The two are kept
	// separate so observers can record them without conflating success and
	// failure paths — e.g. the stats DB stores errMsg in error_msg and only
	// stores output in output_text.
	OnAfterTool func(ctx context.Context, name string, args json.RawMessage, output, errMsg string, duration time.Duration, isError bool)

	// OnClientInfo is called once during the initialize exchange with the
	// client's self-reported name and version.
	OnClientInfo func(ctx context.Context, name, version string)

	// Resources, if set, is consulted by resources/list and resources/read.
	// Leaving it nil disables the resources capability entirely.
	Resources ResourceProvider

	// WriteTimeout bounds a single response write when the transport supports
	// SetWriteDeadline. Defaults to DefaultWriteTimeout (set by New); the daemon
	// overrides it from PLUMB_WRITE_TIMEOUT. 0 disables the deadline. Must be set
	// before Serve.
	WriteTimeout time.Duration // see DefaultWriteTimeout

	// pending tracks in-flight server-initiated requests by string ID.
	pendingMu  sync.Mutex
	pending    map[string]chan json.RawMessage
	reqCounter atomic.Int64

	// prompts registry.
	promptMu    sync.RWMutex
	prompts     map[string]Prompt
	promptOrder []string // insertion order for prompts/list
}

// New creates a Server with the given identity.
func New(info ServerInfo) *Server {
	return &Server{
		info:         info,
		tools:        make(map[string]Tool),
		argShapes:    make(map[string]*shape),
		pending:      make(map[string]chan json.RawMessage),
		prompts:      make(map[string]Prompt),
		WriteTimeout: DefaultWriteTimeout,
	}
}

// Register adds t to the server's tool registry. Calling Register for an
// already-registered name replaces the previous tool.
func (s *Server) Register(t Tool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.tools[t.Name()]; !exists {
		s.order = append(s.order, t.Name())
	}
	s.tools[t.Name()] = t
	if sh, ok := parseShape(t.InputSchema()); ok {
		s.argShapes[t.Name()] = sh
	} else {
		delete(s.argShapes, t.Name())
		slog.Warn("mcp: tool schema not guardable; arguments left unchecked", "tool", t.Name())
	}
}

// resolveToolArgs rewrites recognised parameter aliases to their canonical
// names and validates a tool call's arguments against the declared schema
// before dispatch. It returns the (possibly rewritten) arguments, a warning per
// applied alias, and a validation error. When the tool has no guardable shape
// the arguments pass through unchanged.
func (s *Server) resolveToolArgs(name string, args json.RawMessage) (json.RawMessage, []string, error) {
	s.mu.RLock()
	sh := s.argShapes[name]
	s.mu.RUnlock()
	return resolveArgs(sh, args, name)
}

// ─── serveState ──────────────────────────────────────────────────────────────

// deadlineWriter is the optional capability a transport exposes to bound a
// blocking write. net.Conn satisfies it; pipes used in tests do not.
type deadlineWriter interface {
	SetWriteDeadline(time.Time) error
}

// serveState holds the mutable per-Serve-call state shared across the scan
// goroutine, request dispatcher, and response writer.
//
// Concurrency: enc/wd are written through wrMu; broken is read and written only
// under wrMu. cancel is set once before any goroutine starts and only read
// afterwards.
type serveState struct {
	s            *Server
	enc          *json.Encoder
	wd           deadlineWriter // nil when the transport has no SetWriteDeadline
	writeTimeout time.Duration
	cancel       context.CancelFunc // tears the connection down on a fatal write error
	wrMu         sync.Mutex
	broken       bool // a write failed; further writes are no-ops (guarded by wrMu)
	wg           sync.WaitGroup
}

func newServeState(s *Server, w io.Writer) *serveState {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	ss := &serveState{s: s, enc: enc, writeTimeout: s.WriteTimeout}
	if dw, ok := w.(deadlineWriter); ok {
		ss.wd = dw
	}
	return ss
}

// encode writes one message, bounding the write with a deadline when the
// transport supports it. Caller must hold wrMu. The deadline is cleared after
// the write so an idle connection between replies carries none.
func (ss *serveState) encode(v any) error {
	if ss.wd != nil && ss.writeTimeout > 0 {
		_ = ss.wd.SetWriteDeadline(time.Now().Add(ss.writeTimeout))
		defer func() { _ = ss.wd.SetWriteDeadline(time.Time{}) }()
	}
	return ss.enc.Encode(v)
}

// fail marks the connection broken and cancels Serve. A write error on the
// socket (including a write-deadline timeout) is not recoverable for this
// connection: tearing it down lets the resilient proxy reconnect, where leaving
// it up would wedge wrMu and hang every later reply. Caller must hold wrMu.
//
// A lapsed write deadline is the wedge this guards against, so it logs at WARN.
// Any other write error means the client/proxy disconnected mid-write (broken
// pipe, reset, EOF) — expected churn, logged at Debug so it does not drown the
// log on every routine disconnect.
func (ss *serveState) fail(err error) {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		slog.Warn("mcp: response write timed out — closing connection", "err", err, "timeout", ss.writeTimeout)
	} else {
		slog.Debug("mcp: write failed — closing connection", "err", err)
	}
	ss.broken = true
	if ss.cancel != nil {
		ss.cancel()
	}
}

func (ss *serveState) write(resp mcpResponse) {
	ss.wrMu.Lock()
	defer ss.wrMu.Unlock()
	if ss.broken {
		return
	}
	if err := ss.encode(resp); err != nil {
		ss.fail(err)
	}
}

// makeRequest sends a server-initiated JSON-RPC request and waits for the
// response, satisfying the RequestFn signature.
func (ss *serveState) makeRequest(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := fmt.Sprintf("srv-%d", ss.s.reqCounter.Add(1))
	ch := make(chan json.RawMessage, 1)

	ss.s.pendingMu.Lock()
	ss.s.pending[id] = ch
	ss.s.pendingMu.Unlock()

	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	ss.wrMu.Lock()
	var encErr error
	if ss.broken {
		encErr = errors.New("connection closed")
	} else if encErr = ss.encode(msg); encErr != nil {
		ss.fail(encErr)
	}
	ss.wrMu.Unlock()
	if encErr != nil {
		ss.s.pendingMu.Lock()
		delete(ss.s.pending, id)
		ss.s.pendingMu.Unlock()
		return nil, fmt.Errorf("sending %s: %w", method, encErr)
	}

	select {
	case raw := <-ch:
		return ss.parseResponse(method, raw)
	case <-ctx.Done():
		ss.s.pendingMu.Lock()
		delete(ss.s.pending, id)
		ss.s.pendingMu.Unlock()
		return nil, ctx.Err()
	}
}

func (ss *serveState) parseResponse(method string, raw json.RawMessage) (json.RawMessage, error) {
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("parsing %s response: %w", method, err)
	}
	if r.Error != nil {
		return nil, fmt.Errorf("%s: %s", method, r.Error.Message)
	}
	return r.Result, nil
}

// dispatchMessage handles one inbound message in a wg.Go goroutine.
func (ss *serveState) dispatchMessage(ctx context.Context, data []byte, initOnce *sync.Once) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("mcp: handler panic", "err", r)
			// Best-effort: try to send an error response so the client
			// doesn't hang waiting for a reply that will never come.
			var req mcpRequest
			if json.Unmarshal(data, &req) == nil && req.ID != nil {
				ss.write(errResp(req.ID, -32603, fmt.Sprintf("internal error: %v", r)))
			}
		}
	}()

	// Peek at method before full handling (needed for post-init hook).
	var peek struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(data, &peek)

	resp, isRequest := ss.s.handle(ctx, data)
	if !isRequest {
		if peek.Method == "notifications/roots/listChanged" && ss.s.OnRootsChanged != nil {
			go safeRun("OnRootsChanged", func() { ss.s.OnRootsChanged(ctx, ss.makeRequest) })
		}
		return
	}
	ss.write(resp)

	if peek.Method == "initialize" && resp.Error == nil && ss.s.OnInit != nil {
		initOnce.Do(func() {
			go safeRun("OnInit", func() { ss.s.OnInit(ctx, ss.makeRequest) })
		})
	}
}

// startScanGoroutine spawns the reader goroutine and returns a channel that
// delivers one scanLine per inbound message until the reader is exhausted or
// ctx is cancelled.
func startScanGoroutine(ctx context.Context, reader *bufio.Reader) <-chan scanLine {
	ch := make(chan scanLine)
	go func() {
		defer close(ch)
		for {
			b, tooLarge, err := readMessageLine(reader, maxMessageBytes)
			if err != nil {
				if errors.Is(err, io.EOF) && len(b) == 0 && !tooLarge {
					return
				}
				select {
				case ch <- scanLine{data: b, err: err, tooLarge: tooLarge}:
				case <-ctx.Done():
				}
				return
			}
			select {
			case ch <- scanLine{data: b, tooLarge: tooLarge}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch
}

// Serve reads newline-delimited JSON-RPC 2.0 messages from r and writes
// responses to w until r is exhausted or ctx is cancelled. Each request is
// handled concurrently; Serve waits for all in-flight handlers before returning.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	// A fatal write error cancels this derived context so the loop below returns
	// and the connection is torn down, rather than wedging on a held wrMu.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ss := newServeState(s, w)
	ss.cancel = cancel
	scanCh := startScanGoroutine(ctx, bufio.NewReader(r))
	var initOnce sync.Once

	for {
		select {
		case <-ctx.Done():
			ss.wg.Wait()
			return ctx.Err()
		case line, ok := <-scanCh:
			if !ok {
				ss.wg.Wait()
				return nil
			}
			data := line.data
			if line.tooLarge {
				ss.write(errResp(extractID(data), codeInvalidRequest, fmt.Sprintf("message exceeds %d byte limit", maxMessageBytes)))
				continue
			}
			if line.err != nil {
				ss.wg.Wait()
				return line.err
			}
			ss.wg.Go(func() { ss.dispatchMessage(ctx, data, &initOnce) })
		}
	}
}

// safeRun calls f and recovers from any panic, logging it with a stack trace.
// Use for goroutines that must not crash the daemon (OnInit, OnRootsChanged, …).
func safeRun(name string, f func()) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("mcp: goroutine panic — daemon kept alive",
				"goroutine", name,
				"err", r,
				"stack", string(debug.Stack()))
		}
	}()
	f()
}

// handle parses one message. Returns (response, true) for requests, or
// (_, false) for notifications and responses to server-initiated requests.
func (s *Server) handle(ctx context.Context, raw []byte) (mcpResponse, bool) {
	var req mcpRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return errResp(nil, codeParseError, "parse error: "+err.Error()), true
	}

	// No method means this is a response to a server-initiated request.
	if req.Method == "" {
		s.routeResponse(req.ID, raw)
		return mcpResponse{}, false
	}

	if req.JSONRPC != "2.0" {
		return errResp(req.ID, codeInvalidRequest, `jsonrpc must be "2.0"`), true
	}

	// Notifications carry no ID and require no response.
	if req.ID == nil {
		slog.Debug("mcp: notification", "method", req.Method)
		return mcpResponse{}, false
	}

	switch req.Method {
	case "initialize":
		return s.handleInitialize(ctx, req), true
	case "ping":
		return okResp(req.ID, struct{}{}), true
	case "tools/list":
		return s.handleToolsList(req), true
	case "tools/call":
		return s.handleToolsCall(ctx, req), true
	case "resources/list":
		return s.handleResourcesList(ctx, req), true
	case "resources/read":
		return s.handleResourcesRead(ctx, req), true
	case "prompts/list":
		return s.handlePromptsList(req), true
	case "prompts/get":
		return s.handlePromptsGet(ctx, req), true
	default:
		return errResp(req.ID, codeMethodNotFound, fmt.Sprintf("method not found: %s", req.Method)), true
	}
}

// routeResponse delivers a response to the pending channel for its request ID.
func (s *Server) routeResponse(id any, raw []byte) {
	idStr, ok := id.(string)
	if !ok {
		return
	}
	s.pendingMu.Lock()
	ch := s.pending[idStr]
	if ch != nil {
		delete(s.pending, idStr)
	}
	s.pendingMu.Unlock()
	if ch != nil {
		ch <- raw
	}
}
