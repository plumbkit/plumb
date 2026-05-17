// Package mcp implements a Model Context Protocol server over stdio.
// Transport: newline-delimited JSON-RPC 2.0 (not LSP Content-Length framing).
// Protocol version: 2024-11-05.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

const protocolVersion = "2024-11-05"

const maxMessageBytes = 4 << 20 // 4 MiB per newline-delimited JSON-RPC message

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
	info  ServerInfo
	mu    sync.RWMutex
	tools map[string]Tool
	order []string // insertion order for tools/list

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
		info:    info,
		tools:   make(map[string]Tool),
		pending: make(map[string]chan json.RawMessage),
		prompts: make(map[string]Prompt),
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
}

// Serve reads newline-delimited JSON-RPC 2.0 messages from r and writes
// responses to w until r is exhausted or ctx is cancelled. Each request is
// handled concurrently; Serve waits for all in-flight handlers before returning.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	reader := bufio.NewReader(r)

	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	var wrMu sync.Mutex

	write := func(resp mcpResponse) {
		wrMu.Lock()
		defer wrMu.Unlock()
		if err := enc.Encode(resp); err != nil {
			slog.Error("mcp: write error", "err", err)
		}
	}

	// requestFn sends a server-initiated request and waits for the response.
	requestFn := func(ctx context.Context, method string, params any) (json.RawMessage, error) {
		id := fmt.Sprintf("srv-%d", s.reqCounter.Add(1))
		ch := make(chan json.RawMessage, 1)

		s.pendingMu.Lock()
		s.pending[id] = ch
		s.pendingMu.Unlock()

		msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
		if params != nil {
			msg["params"] = params
		}
		wrMu.Lock()
		encErr := enc.Encode(msg)
		wrMu.Unlock()
		if encErr != nil {
			s.pendingMu.Lock()
			delete(s.pending, id)
			s.pendingMu.Unlock()
			return nil, fmt.Errorf("sending %s: %w", method, encErr)
		}

		select {
		case raw := <-ch:
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
		case <-ctx.Done():
			s.pendingMu.Lock()
			delete(s.pending, id)
			s.pendingMu.Unlock()
			return nil, ctx.Err()
		}
	}

	// scanCh carries lines from the reader goroutine.
	type scanLine struct {
		data     []byte
		err      error
		tooLarge bool
	}
	scanCh := make(chan scanLine)
	go func() {
		defer close(scanCh)
		for {
			b, tooLarge, err := readMessageLine(reader, maxMessageBytes)
			if err != nil {
				if errors.Is(err, io.EOF) && len(b) == 0 && !tooLarge {
					return
				}
				select {
				case scanCh <- scanLine{data: b, err: err, tooLarge: tooLarge}:
				case <-ctx.Done():
				}
				return
			}
			select {
			case scanCh <- scanLine{data: b, tooLarge: tooLarge}:
			case <-ctx.Done():
				return
			}
		}
	}()

	var wg sync.WaitGroup
	var initOnce sync.Once

	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		case line, ok := <-scanCh:
			if !ok {
				wg.Wait()
				return nil
			}
			data := line.data
			if line.tooLarge {
				write(errResp(extractID(data), codeInvalidRequest, fmt.Sprintf("message exceeds %d byte limit", maxMessageBytes)))
				continue
			}
			if line.err != nil {
				wg.Wait()
				return line.err
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() {
					if r := recover(); r != nil {
						slog.Error("mcp: handler panic", "err", r)
						// Best-effort: try to send an error response so the client
						// doesn't hang waiting for a reply that will never come.
						var req mcpRequest
						if json.Unmarshal(data, &req) == nil && req.ID != nil {
							write(errResp(req.ID, -32603, fmt.Sprintf("internal error: %v", r)))
						}
					}
				}()

				// Peek at method before full handling (needed for post-init hook).
				var peek struct {
					Method string `json:"method"`
				}
				_ = json.Unmarshal(data, &peek)

				resp, isRequest := s.handle(ctx, data)
				if !isRequest {
					if peek.Method == "notifications/roots/listChanged" && s.OnRootsChanged != nil {
						go safeRun("OnRootsChanged", func() { s.OnRootsChanged(ctx, requestFn) })
					}
					return
				}
				write(resp)

				if peek.Method == "initialize" && resp.Error == nil && s.OnInit != nil {
					initOnce.Do(func() {
						go safeRun("OnInit", func() { s.OnInit(ctx, requestFn) })
					})
				}
			}()
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

func (s *Server) handleInitialize(ctx context.Context, req mcpRequest) mcpResponse {
	if s.OnClientInfo != nil && req.Params != nil {
		var p struct {
			ClientInfo struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"clientInfo"`
		}
		if err := json.Unmarshal(req.Params, &p); err == nil && p.ClientInfo.Name != "" {
			s.OnClientInfo(ctx, p.ClientInfo.Name, p.ClientInfo.Version)
		}
	}

	type serverInfoWire struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	type result struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Capabilities    map[string]any `json:"capabilities"`
		ServerInfo      serverInfoWire `json:"serverInfo"`
		Instructions    string         `json:"instructions,omitempty"`
	}
	caps := map[string]any{"tools": map[string]any{}}
	if s.Resources != nil {
		caps["resources"] = map[string]any{}
	}
	s.promptMu.RLock()
	hasPrompts := len(s.prompts) > 0
	s.promptMu.RUnlock()
	if hasPrompts {
		caps["prompts"] = map[string]any{}
	}
	instructions := s.info.Instructions
	if instructions == "" {
		instructions = DefaultInstructions
	} else if instructions == "-" {
		instructions = ""
	}
	res := result{
		ProtocolVersion: protocolVersion,
		Capabilities:    caps,
		ServerInfo:      serverInfoWire{Name: s.info.Name, Version: s.info.Version},
		Instructions:    instructions,
	}
	return okResp(req.ID, res)
}

func (s *Server) handleToolsList(req mcpRequest) mcpResponse {
	type toolDef struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"inputSchema"`
	}
	s.mu.RLock()
	defs := make([]toolDef, 0, len(s.order))
	for _, name := range s.order {
		t := s.tools[name]
		defs = append(defs, toolDef{
			Name:        t.Name(),
			Description: t.Description(),
			InputSchema: t.InputSchema(),
		})
	}
	s.mu.RUnlock()
	return okResp(req.ID, map[string]any{"tools": defs})
}

func (s *Server) handleToolsCall(ctx context.Context, req mcpRequest) mcpResponse {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return errResp(req.ID, codeInvalidParams, "invalid params: "+err.Error())
	}

	s.mu.RLock()
	t, ok := s.tools[params.Name]
	s.mu.RUnlock()
	if !ok {
		return errResp(req.ID, codeMethodNotFound, fmt.Sprintf("unknown tool: %s", params.Name))
	}

	if s.OnBeforeTool != nil {
		s.OnBeforeTool(ctx, params.Name, params.Arguments)
	}

	start := time.Now()
	text, err := t.Execute(ctx, params.Arguments)
	dur := time.Since(start)

	if s.OnAfterTool != nil {
		errMsg := ""
		afterText := text
		if err != nil {
			errMsg = err.Error()
			// Tools that return an error usually return "" alongside it; clear
			// any partial output so observers don't see stale text for failed
			// calls.
			afterText = ""
		}
		s.OnAfterTool(ctx, params.Name, params.Arguments, afterText, errMsg, dur, err != nil)
	}

	type content struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type callResult struct {
		Content []content `json:"content"`
		IsError bool      `json:"isError"`
	}
	if err != nil {
		slog.Warn("mcp: tool error", "tool", params.Name, "err", err)
		return okResp(req.ID, callResult{
			Content: []content{{Type: "text", Text: "error: " + err.Error()}},
			IsError: true,
		})
	}
	return okResp(req.ID, callResult{
		Content: []content{{Type: "text", Text: text}},
		IsError: false,
	})
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func readMessageLine(r *bufio.Reader, limit int) ([]byte, bool, error) {
	var out []byte
	for {
		part, err := r.ReadSlice('\n')
		if len(part) > 0 {
			remaining := limit - len(out)
			if remaining > 0 {
				if len(part) > remaining {
					out = append(out, part[:remaining]...)
				} else {
					out = append(out, part...)
				}
			}
			if len(out) >= limit && (err == bufio.ErrBufferFull || len(part) > remaining) {
				if err := discardMessageRest(r); err != nil && !errors.Is(err, io.EOF) {
					return out, true, err
				}
				return out, true, nil
			}
		}
		switch {
		case err == nil:
			return trimTrailingNewline(out), false, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if len(out) > 0 {
				return out, false, nil
			}
			return nil, false, io.EOF
		default:
			return out, false, err
		}
	}
}

func discardMessageRest(r *bufio.Reader) error {
	for {
		part, err := r.ReadSlice('\n')
		if len(part) > 0 && part[len(part)-1] == '\n' {
			return nil
		}
		if err == nil || errors.Is(err, io.EOF) {
			return err
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return err
		}
	}
}

func trimTrailingNewline(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	if len(b) > 0 && b[len(b)-1] == '\r' {
		b = b[:len(b)-1]
	}
	return b
}

func extractID(prefix []byte) any {
	var req struct {
		ID any `json:"id"`
	}
	if json.Unmarshal(prefix, &req) == nil {
		return req.ID
	}
	const key = `"id"`
	idx := bytes.Index(prefix, []byte(key))
	if idx < 0 {
		return nil
	}
	rest := prefix[idx+len(key):]
	colon := bytes.IndexByte(rest, ':')
	if colon < 0 {
		return nil
	}
	rest = bytes.TrimSpace(rest[colon+1:])
	end := len(rest)
	for i, b := range rest {
		if b == ',' || b == '}' || b == '\n' || b == '\r' {
			end = i
			break
		}
	}
	var id any
	if json.Unmarshal(bytes.TrimSpace(rest[:end]), &id) == nil {
		return id
	}
	return nil
}

func okResp(id, result any) mcpResponse {
	return mcpResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func errResp(id any, code int, msg string) mcpResponse {
	return mcpResponse{JSONRPC: "2.0", ID: id, Error: &mcpError{Code: code, Message: msg}}
}
