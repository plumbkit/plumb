// Package mcp implements a Model Context Protocol server over stdio.
// Transport: newline-delimited JSON-RPC 2.0 (not LSP Content-Length framing).
// Protocol version: negotiated per connection from supportedProtocolVersions.
package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"runtime/debug"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/plumbkit/plumb/internal/toolerror"
)

// supportedProtocolVersions lists the MCP protocol revisions this server
// genuinely implements, newest first. The initialize handshake answers with the
// client's offered revision when it is in this set and with the newest entry
// otherwise — never with an offered revision plumb has not implemented, which
// would be a false claim of support the client would then rely on.
var supportedProtocolVersions = []string{"2024-11-05"}

// negotiateProtocolVersion picks the revision to answer an initialize request
// with: the client's offered revision when plumb implements it, else the newest
// supported revision (per the spec the client then decides whether it can
// proceed). An empty or unknown offer yields the newest supported revision.
func negotiateProtocolVersion(offered string) string {
	if slices.Contains(supportedProtocolVersions, offered) {
		return offered
	}
	return supportedProtocolVersions[0]
}

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

// DefaultToolExecTimeout bounds a single Execute call for tools that opt in via
// ExecTimeoutBounded (read_file, which performs blocking syscalls with no
// internal deadline). Without it a stat/open/readdir on a slow or unresponsive
// mount (a stalled network/iCloud/FUSE volume) runs unbounded until the MCP
// client abandons the call at its own multi-minute timeout. 10s is far longer
// than any healthy local read yet short enough to fail fast; the daemon
// overrides it from PLUMB_TOOL_EXEC_TIMEOUT. 0 disables the bound.
const DefaultToolExecTimeout = 10 * time.Second

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

// NotifyFn sends a server-initiated JSON-RPC notification (no id, no response)
// to the MCP client. Returns an error if the connection write fails.
type NotifyFn func(method string, params any) error

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
	// Data carries the structured error envelope for a request plumb rejected
	// before any tool ran. omitempty keeps every existing errResp caller
	// byte-identical to the pre-envelope payload.
	Data any `json:"data,omitempty"`
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
	// it overrides InstructionsForClient's render; set to "-" for none.
	Instructions string
}

// Server is an MCP server. Register tools, then call Serve.
//
// OnInit, if set, is called once in a goroutine after a successful
// initialize exchange. It receives a RequestFn the callback can use to make
// requests back to the MCP client (e.g. roots/list) and a NotifyFn it can use
// to send server-initiated notifications (e.g. notifications/tools/list_changed).
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
	argShapes map[string]*shape          // parsed argument contract per tool; nil when unguardable
	pubSchema map[string]json.RawMessage // alias-tolerant schema advertised in tools/list
	order     []string                   // insertion order for tools/list

	// OnInit is called once after a successful MCP initialize exchange. notify
	// lets the callback send server-initiated notifications to the client.
	OnInit func(ctx context.Context, request RequestFn, notify NotifyFn)

	// OnRootsChanged is called each time the client notifies that its roots changed.
	OnRootsChanged func(ctx context.Context, request RequestFn)

	// OnBeforeTool is called synchronously before each tools/call execution.
	// logicalAgent is the client-declared logical-agent identity carried in the
	// call's `_meta` (MetaLogicalAgentKey), or "" when the client supplies none.
	OnBeforeTool func(ctx context.Context, name string, args json.RawMessage, logicalAgent string)

	// OnToolRefusal is called before a tools/call executes with the canonical
	// tool name and logical-agent identity; a non-nil error refuses the call.
	OnToolRefusal func(ctx context.Context, name, logicalAgent string) error

	// OnAfterTool is called synchronously after each tools/call execution.
	// output is the tool's text result (empty when isError is true). errMsg
	// is the error string (empty when the call succeeded). The two are kept
	// separate so observers can record them without conflating success and
	// failure paths — e.g. the stats DB stores errMsg in error_msg and only
	// stores output in output_text.
	//
	// failure is the SAME classification this call put on the wire in its
	// `_meta` envelope, not a second derivation of it — see classifyOnce. It is
	// nil when the call succeeded and when the failure carries no
	// classification; an observer must record that as "no structured claim"
	// rather than substituting a default, because that is exactly what the
	// client was told by the envelope's absence.
	OnAfterTool func(ctx context.Context, name string, args json.RawMessage, output, errMsg string, duration time.Duration, isError bool, failure *toolerror.Error)

	// EnrichToolOutput, if set, may append advisory text to a SUCCESSFUL tool
	// result before it is returned to the client (OnAfterTool cannot — it is
	// fire-and-forget). Called synchronously on the response path, so it must be
	// cheap and must never block. Returning text unchanged is a no-op.
	EnrichToolOutput func(ctx context.Context, name string, args json.RawMessage, text string) string

	// ToolResultMeta, if set, contributes `_meta` entries to a SUCCESSFUL
	// tools/call result (a failure carries its error envelope under
	// MetaToolErrorKey instead). Called synchronously on the response path, so
	// it must be cheap and must never block. A nil return leaves the result
	// byte-identical to the pre-`_meta` payload.
	ToolResultMeta func(ctx context.Context, name string, args json.RawMessage) map[string]any

	// OnClientInfo is called once during the initialize exchange with the
	// client's self-reported name and version.
	OnClientInfo func(ctx context.Context, name, version string)

	// OnProtocolNegotiated is called once per connection during the initialize
	// exchange — guarded by the same once as OnInit, so a client that re-sends
	// initialize cannot double-record the negotiation — with the protocol
	// revision the client offered, the revision plumb answered (see
	// negotiateProtocolVersion), and the client-advertised capabilities as raw
	// JSON (nil when absent). offered is "" when the client sent no
	// protocolVersion. It runs after the initialize response is written,
	// synchronously before OnInit.
	OnProtocolNegotiated func(ctx context.Context, offered, answered string, capabilities json.RawMessage)

	// OnAllowDirs is called once during the initialize exchange with the extra
	// read-write roots the client transported in the initialize params'
	// _meta[MetaAllowDirsKey] field (see `plumb serve --allow-dir`). It runs
	// synchronously, before OnInit attaches the workspace, so the roots are
	// available when the connection's PathPolicy is first built. Empty/absent ⇒
	// not called.
	OnAllowDirs func(ctx context.Context, dirs []string)

	// OnProxySession is called once during the initialize exchange with the stable
	// proxy session ID the client transported in _meta[MetaProxySessionKey]. It
	// runs synchronously, before OnInit attaches the workspace, so the ID is
	// available when the connection rehydrates persisted state. Empty/absent ⇒
	// not called.
	OnProxySession func(ctx context.Context, id string)

	// OnWorkspaceHint is called once during initialize with the serve proxy's
	// explicit workspace pre-pin (_meta[MetaWorkspaceKey] — --workspace or
	// PLUMB_WORKSPACE; without either, no key is sent and serve starts unattached) — advisory, not an authoritative root. Absent/empty ⇒ never called.
	OnWorkspaceHint func(ctx context.Context, dir string)

	// OnPinnedWorkspace is called once during initialize with the workspace the
	// caller last chose via session_start, replayed in _meta[MetaPinnedWorkspaceKey].
	OnPinnedWorkspace func(ctx context.Context, dir string)

	// OnSessionID is called once during initialize with the replayed stable
	// plumb session ID (_meta[MetaSessionIDKey]) for adoption (PLAN-296).
	OnSessionID func(ctx context.Context, id string)

	// InitializeMeta, if set, contributes `_meta` to the initialize RESULT. It is
	// called once per initialize, AFTER the param hooks above have run, so the
	// application may report facts it only establishes while handling them —
	// which session this connection turned out to be, and whether that identity
	// was recovered or freshly minted (PLAN-426).
	//
	// The map is opaque here on purpose. internal/mcp is transport: it owns the
	// `_meta` key vocabulary (meta_keys.go) and the wire envelope, and knows
	// nothing about sessions, so keeping the value untyped is what stops a
	// transport package growing a dependency on the session model. Returning nil
	// or an empty map omits `_meta` entirely, leaving the response byte-identical
	// to one from a build without this hook.
	//
	// Nothing secret may travel here: the result goes to the client.
	InitializeMeta func(ctx context.Context) map[string]any

	// ToolFilter, if set, decides which tools appear in tools/list: a tool whose
	// name it rejects is hidden from the advertised set but STAYS CALLABLE via
	// tools/call (hidden ≠ unregistered). It is consulted per tools/list, so it
	// may resolve a profile that depends on the (by then known) client identity.
	// Must be set before Serve.
	// Runs with the registry lock RELEASED: handleToolsList snapshots the tools
	// under s.mu.RLock, then filters outside it (see #15), so the filter must
	// not assume serialisation with Register. Keep it fast — it runs for every
	// registered tool on each tools/list.
	ToolFilter func(name string) bool

	// AlwaysLoad, if set, decides which advertised tools are pinned into the
	// client's context at session start rather than deferred behind MCP tool
	// search: a tool whose name it accepts is emitted with
	// _meta[MetaAlwaysLoadKey]=true in tools/list. Consulted per tools/list with
	// the registry lock released, exactly like ToolFilter. Must be set before
	// Serve.
	AlwaysLoad func(name string) bool

	// Resources, if set, is consulted by resources/list and resources/read.
	// Leaving it nil disables the resources capability entirely.
	Resources ResourceProvider

	// WriteTimeout bounds a single response write when the transport supports
	// SetWriteDeadline. Defaults to DefaultWriteTimeout (set by New); the daemon
	// overrides it from PLUMB_WRITE_TIMEOUT. 0 disables the deadline. Must be set
	// before Serve.
	WriteTimeout time.Duration // see DefaultWriteTimeout

	// ToolExecTimeout bounds a single Execute call for tools that implement
	// ExecTimeoutBounded. Defaults to DefaultToolExecTimeout (set by New); the
	// daemon overrides it from PLUMB_TOOL_EXEC_TIMEOUT. 0 disables the bound. Must
	// be set before Serve.
	ToolExecTimeout time.Duration // see DefaultToolExecTimeout

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
		info:            info,
		tools:           make(map[string]Tool),
		argShapes:       make(map[string]*shape),
		pubSchema:       make(map[string]json.RawMessage),
		pending:         make(map[string]chan json.RawMessage),
		prompts:         make(map[string]Prompt),
		WriteTimeout:    DefaultWriteTimeout,
		ToolExecTimeout: DefaultToolExecTimeout,
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
	// The advertised schema is alias-tolerant (relaxed required + additionalProperties)
	// so a pre-validating host forwards alias-bearing calls; the daemon still validates
	// against the original strict shape below. publishSchema is fail-open, so it is
	// always safe to cache — even for an unguardable schema (returned unchanged).
	s.pubSchema[t.Name()] = publishSchema(t.InputSchema())
	if sh, ok := parseShape(t.InputSchema()); ok {
		s.argShapes[t.Name()] = sh
	} else {
		delete(s.argShapes, t.Name())
		slog.Warn("mcp: tool schema not guardable; arguments left unchecked", "tool", t.Name())
	}
}

// ToolNames returns the registered tool names in insertion order. Safe for
// concurrent use; the returned slice is a copy. Independent of ToolFilter — it
// reports every registered tool, not just the advertised set.
func (s *Server) ToolNames() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.order)
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
		return errResp(req.ID, codeMethodNotFound, "method not found: "+req.Method), true
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
