package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"time"
)

func (s *Server) handleInitialize(ctx context.Context, req mcpRequest) mcpResponse {
	s.fireInitParamHooks(ctx, req.Params)
	// The OnProtocolNegotiated hook is NOT fired here: it fires under the
	// per-connection once-guard in dispatchMessage, so a client re-sending
	// initialize cannot double-record the negotiation.
	offered, _ := clientProtocolParams(req.Params)
	answered := negotiateProtocolVersion(offered)

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
	serverCaps := map[string]any{"tools": map[string]any{"listChanged": true}}
	if s.Resources != nil {
		serverCaps["resources"] = map[string]any{}
	}
	s.promptMu.RLock()
	hasPrompts := len(s.prompts) > 0
	s.promptMu.RUnlock()
	if hasPrompts {
		serverCaps["prompts"] = map[string]any{}
	}
	instructions := s.info.Instructions
	switch instructions {
	case "":
		instructions = DefaultInstructions
	case "-":
		instructions = ""
	}
	res := result{
		ProtocolVersion: answered,
		Capabilities:    serverCaps,
		ServerInfo:      serverInfoWire{Name: s.info.Name, Version: s.info.Version},
		Instructions:    instructions,
	}
	return okResp(req.ID, res)
}

// fireInitParamHooks dispatches the hooks that consume per-connection metadata
// from the initialize params: the client identity, and the `plumb serve`
// transport keys (allow-dirs, the stable proxy session ID, the cwd workspace
// hint). All fire synchronously, before OnInit, and each is skipped when its
// hook is unset or its value is absent/empty — a client that sends nothing
// changes nothing.
func (s *Server) fireInitParamHooks(ctx context.Context, params json.RawMessage) {
	if params == nil {
		return
	}
	if s.OnClientInfo != nil {
		var p struct {
			ClientInfo struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"clientInfo"`
		}
		if err := json.Unmarshal(params, &p); err == nil && p.ClientInfo.Name != "" {
			s.OnClientInfo(ctx, p.ClientInfo.Name, p.ClientInfo.Version)
		}
	}
	if s.OnAllowDirs != nil {
		if dirs := allowDirsFromParams(params); len(dirs) > 0 {
			s.OnAllowDirs(ctx, dirs)
		}
	}
	if s.OnProxySession != nil {
		if id := proxySessionFromParams(params); id != "" {
			s.OnProxySession(ctx, id)
		}
	}
	if s.OnWorkspaceHint != nil {
		if dir := workspaceHintFromParams(params); dir != "" {
			s.OnWorkspaceHint(ctx, dir)
		}
	}
	if s.OnPinnedWorkspace != nil {
		if dir := pinnedWorkspaceFromParams(params); dir != "" {
			s.OnPinnedWorkspace(ctx, dir)
		}
	}
	if s.OnSessionID != nil {
		if id := stringFromMeta(params, MetaSessionIDKey); id != "" {
			s.OnSessionID(ctx, id)
		}
	}
}

// toolSnapshot is an immutable copy of one registered tool's advertised
// metadata, captured under s.mu so tools/list can filter and marshal the
// response with the lock released.
type toolSnapshot struct {
	name        string
	description string
	schema      json.RawMessage
}

// snapshotTools copies every registered tool's advertised metadata in insertion
// order under s.mu, then releases the lock. The copy is deliberately cheap so
// the lock is held only across the map reads — ToolFilter (which may resolve a
// client profile of unbounded cost) and the response marshal both run on the
// caller's side, off the lock.
//
// Concurrency: takes s.mu in read mode for the duration of the copy only; the
// returned slice shares no mutable state with the server.
//
// Tool.Description() is therefore called UNDER s.mu.RLock, which makes it the
// one metadata method that must not reach back into the connection. Every
// description in the tree is a fixed string today; a client-aware one would be
// re-entering, and sync.RWMutex is not recursive when a writer is queued — a
// description that consulted session_start's 3-value profile accessor would go
// hiddenToolCount → Server.ToolNames → s.mu.RLock and deadlock. Vary a
// description by client only by hoisting the decision out to registration time.
func (s *Server) snapshotTools() []toolSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snaps := make([]toolSnapshot, 0, len(s.order))
	for _, name := range s.order {
		t := s.tools[name]
		schema := s.pubSchema[name]
		if schema == nil {
			schema = t.InputSchema()
		}
		snaps = append(snaps, toolSnapshot{name: t.Name(), description: t.Description(), schema: schema})
	}
	return snaps
}

// clientProtocolParams extracts the protocol revision and capabilities the
// client offered in its initialize params. Fail-safe like the other init-param
// extractors: structurally malformed JSON yields "" / nil, and negotiation
// then answers with the newest supported revision. A field with the wrong JSON
// type (e.g. a numeric protocolVersion) zeroes only that field — the other is
// still returned, since json.Unmarshal decodes past a type error.
func clientProtocolParams(params json.RawMessage) (string, json.RawMessage) {
	if params == nil {
		return "", nil
	}
	var p struct {
		ProtocolVersion string          `json:"protocolVersion"`
		Capabilities    json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		var typeErr *json.UnmarshalTypeError
		if !errors.As(err, &typeErr) {
			return "", nil
		}
	}
	// A "capabilities":null literal decodes to the RawMessage "null", not nil;
	// normalise it to absent so the session record omits the field.
	if bytes.Equal(p.Capabilities, []byte("null")) {
		p.Capabilities = nil
	}
	return p.ProtocolVersion, p.Capabilities
}

// allowDirsFromParams extracts the extra read-write roots from the initialize
// params' _meta[MetaAllowDirsKey] field. Fail-safe: any shape mismatch (no
// _meta, wrong key, non-array, malformed JSON) yields nil, and empty/blank
// entries are dropped — so a client that sends nothing changes nothing.
func allowDirsFromParams(params json.RawMessage) []string {
	var p struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil
	}
	raw, ok := p.Meta[MetaAllowDirsKey]
	if !ok {
		return nil
	}
	var dirs []string
	if err := json.Unmarshal(raw, &dirs); err != nil {
		return nil
	}
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		if d != "" {
			out = append(out, d)
		}
	}
	return out
}

// proxySessionFromParams extracts the stable proxy session ID from the
// initialize params' _meta[MetaProxySessionKey] field. Fail-safe: any shape
// mismatch (no _meta, wrong key, non-string, malformed JSON) yields "", so a
// client that sends nothing changes nothing.
func proxySessionFromParams(params json.RawMessage) string {
	return stringFromMeta(params, MetaProxySessionKey)
}

// workspaceHintFromParams extracts the serve proxy's working-directory attach
// hint from the initialize params' _meta[MetaWorkspaceKey] field. Fail-safe
// like proxySessionFromParams: any shape mismatch yields "", so a client that
// sends nothing changes nothing.
func workspaceHintFromParams(params json.RawMessage) string {
	return stringFromMeta(params, MetaWorkspaceKey)
}

// pinnedWorkspaceFromParams extracts the workspace the caller chose with an
// explicit session_start, replayed by the serve proxy in
// _meta[MetaPinnedWorkspaceKey]. Fail-safe like the others: any shape mismatch
// yields "", so a proxy that predates the key changes nothing.
func pinnedWorkspaceFromParams(params json.RawMessage) string {
	return stringFromMeta(params, MetaPinnedWorkspaceKey)
}

// stringFromMeta extracts a single string value stored under key in the given
// params' _meta object. Fail-safe: any shape mismatch (no _meta, wrong key,
// non-string, malformed JSON) yields "".
func stringFromMeta(params json.RawMessage, key string) string {
	var p struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return ""
	}
	raw, ok := p.Meta[key]
	if !ok {
		return ""
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	return v
}

// logicalAgentFromMeta extracts the logical-agent identity from an
// already-decoded tools/call _meta map, fail-safe: an absent, wrong-type or
// malformed value yields "". Kept as a helper so handleToolsCall does not pay a
// gocyclo branch for the extraction.
func logicalAgentFromMeta(meta map[string]json.RawMessage) string {
	raw, ok := meta[MetaLogicalAgentKey]
	if !ok {
		return ""
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	return v
}

// logicalAgentCtxKey is the context key under which a tools/call's logical-agent
// identity travels. Unexported, so nothing outside this package can forge a value
// under it; the only reader is the exported LogicalAgentFromCtx.
type logicalAgentCtxKey struct{}

// withLogicalAgent derives the per-request ctx for a tools/call: a child ctx
// carrying the call's logical-agent identity. It is the transport half of the
// per-agent keying contract (PLAN-286 §3) — the identity must ride ctx because
// mcp.Serve dispatches requests concurrently and a mutable per-connection field
// would let two racing requests read each other's agent. An empty id returns the
// parent ctx unchanged (no value stored), so a non-multiplexing client pays nothing.
func withLogicalAgent(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, logicalAgentCtxKey{}, id)
}

// LogicalAgentFromCtx returns the logical-agent identity carried in a tools/call
// ctx (set by handleToolsCall), or "" when the call declared none. It is the
// single source the daemon's per-agent state resolvers key on.
func LogicalAgentFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(logicalAgentCtxKey{}).(string); ok {
		return v
	}
	return ""
}

func (s *Server) handleToolsList(req mcpRequest) mcpResponse {
	type toolDef struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"inputSchema"`
		// Meta carries per-tool `_meta`; omitempty keeps a non-pinned entry
		// byte-identical to the pre-AlwaysLoad payload.
		Meta map[string]any `json:"_meta,omitempty"`
	}
	// Snapshot the registry under s.mu, then apply ToolFilter and build the
	// response with the lock released. A lightweight probe (ping, daemon_info)
	// contending on the per-connection write mutex must never queue behind a slow
	// filter or marshal held under the shared registry lock.
	snaps := s.snapshotTools()
	filter := s.ToolFilter     // set before Serve; read without the lock
	alwaysLoad := s.AlwaysLoad // set before Serve; read without the lock
	defs := make([]toolDef, 0, len(snaps))
	for _, sn := range snaps {
		// A filtered-out tool is hidden from the advertised list but stays
		// callable by name — handleToolsCall does not consult ToolFilter.
		if filter != nil && !filter(sn.name) {
			continue
		}
		def := toolDef{
			Name:        sn.name,
			Description: sn.description,
			InputSchema: sn.schema,
		}
		// Pin the hot tools into the client's context so it never runs an MCP
		// tool-search round-trip (or guesses parameter names) for them; the long
		// tail stays deferred, preserving that context saving.
		if alwaysLoad != nil && alwaysLoad(sn.name) {
			def.Meta = map[string]any{MetaAlwaysLoadKey: true}
		}
		defs = append(defs, def)
	}
	return okResp(req.ID, map[string]any{"tools": defs})
}

// content is one MCP text block in a tools/call result.
type content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// callResult is the tools/call result payload. Meta carries per-result `_meta`;
// omitempty keeps a successful call — and a failure plumb cannot classify —
// byte-identical to the pre-envelope payload.
type callResult struct {
	Content []content      `json:"content"`
	IsError bool           `json:"isError"`
	Meta    map[string]any `json:"_meta,omitempty"`
}

// refusalResponse asks the OnToolRefusal hook whether a call must be refused
// and returns the refusal response when it must; nil when the call may proceed.
// Extracted from handleToolsCall to keep that function under the gocyclo cap:
// the refusal adds two branches (hook set, hook declined) but no dispatch logic.
func (s *Server) refusalResponse(ctx context.Context, req mcpRequest, name, logicalAgent string) *mcpResponse {
	if s.OnToolRefusal == nil {
		return nil
	}
	if refusalErr := s.OnToolRefusal(ctx, name, logicalAgent); refusalErr != nil {
		slog.Warn("mcp: tool refused", "tool", name, "err", refusalErr)
		resp := okResp(req.ID, callResult{
			Content: []content{{Type: "text", Text: "error: " + refusalErr.Error()}},
			IsError: true,
		})
		return &resp
	}
	return nil
}

func (s *Server) handleToolsCall(ctx context.Context, req mcpRequest) mcpResponse {
	var params struct {
		Name      string                     `json:"name"`
		Arguments json.RawMessage            `json:"arguments"`
		Meta      map[string]json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return errRespData(req.ID, codeInvalidParams, "invalid params: "+err.Error(), invalidCallEnvelope(""))
	}

	// A retired tool name is resolved onto its canonical tool BEFORE the registry
	// lookup, so the canonical name is what the hooks, the parameter-alias
	// resolver, execTool, and the recorded stats all see. See toolalias.go.
	aliasUsed := ""
	if canonical, adapted, ok := resolveToolAlias(params.Name, params.Arguments); ok {
		aliasUsed, params.Name, params.Arguments = params.Name, canonical, adapted
	}

	s.mu.RLock()
	t, ok := s.tools[params.Name]
	s.mu.RUnlock()
	if !ok {
		return errRespData(req.ID, codeMethodNotFound, s.unknownToolMessage(params.Name),
			invalidCallEnvelope(params.Name))
	}

	logicalAgent := logicalAgentFromMeta(params.Meta)
	ctx = withLogicalAgent(ctx, logicalAgent)
	if resp := s.refusalResponse(ctx, req, params.Name, logicalAgent); resp != nil {
		return *resp
	}
	if s.OnBeforeTool != nil {
		runHookSafely("OnBeforeTool", func() { s.OnBeforeTool(ctx, params.Name, params.Arguments, logicalAgent) })
	}

	start := time.Now()
	var text string
	args, warnings, err := s.resolveToolArgs(params.Name, params.Arguments)
	if err == nil {
		text, err = s.execTool(ctx, t, params.Name, args)
	}
	dur := time.Since(start)

	// Classify ONCE, here, and hand the same value to both consumers below: the
	// observer that records the failure and the `_meta` envelope the client
	// reads. See classifyOnce.
	failure := classifyOnce(err, params.Name)

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
		runHookSafely("OnAfterTool", func() {
			s.OnAfterTool(ctx, params.Name, params.Arguments, afterText, errMsg, dur, err != nil, failure)
		})
	}

	if err != nil {
		slog.Warn("mcp: tool error", "tool", params.Name, "err", err)
		// The notice leads the FAILURE too. An aliased call that errors reports a
		// tool the caller never named ("find_files: …" from a list_directory call),
		// which reads as plumb answering a different question; without the notice
		// there is nothing in the response tying the two names together.
		msg := "error: " + err.Error()
		if aliasUsed != "" {
			msg = toolAliasNotice(aliasUsed, params.Name) + msg
		}
		return okResp(req.ID, callResult{
			Content: []content{{Type: "text", Text: msg}},
			IsError: true,
			Meta:    toolErrorMeta(failure),
		})
	}
	if len(warnings) > 0 {
		text = aliasNotice(warnings) + text
	}
	if aliasUsed != "" {
		text = toolAliasNotice(aliasUsed, params.Name) + text
	}
	if s.EnrichToolOutput != nil {
		runHookSafely("EnrichToolOutput", func() {
			text = s.EnrichToolOutput(ctx, params.Name, params.Arguments, text)
		})
	}
	res := callResult{
		Content: []content{{Type: "text", Text: text}},
		IsError: false,
	}
	if s.ToolResultMeta != nil {
		runHookSafely("ToolResultMeta", func() {
			res.Meta = s.ToolResultMeta(ctx, params.Name, params.Arguments)
		})
	}
	return okResp(req.ID, res)
}

// runHookSafely runs an observability/enrichment hook, recovering from a panic
// so a misbehaving hook cannot turn a successful tool call into a client-visible
// -32603 and drop the real output. The tool's result is preserved unchanged
// (an EnrichToolOutput panic leaves text at its pre-call value).
func runHookSafely(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("mcp: tool hook panicked; ignoring", "hook", name, "panic", r)
		}
	}()
	fn()
}

// appendUpTo appends as much of part to *out as limit allows, reporting whether
// part had to be truncated — including the case where *out was already at the
// limit and part was dropped entirely. That boolean is the "message was longer
// than we are willing to buffer" signal readMessageLine acts on.
func appendUpTo(out *[]byte, part []byte, limit int) bool {
	remaining := limit - len(*out)
	if remaining <= 0 {
		return true
	}
	if len(part) > remaining {
		*out = append(*out, part[:remaining]...)
		return true
	}
	*out = append(*out, part...)
	return false
}

func readMessageLine(r *bufio.Reader, limit int) ([]byte, bool, error) {
	var out []byte
	for {
		part, err := r.ReadSlice('\n')
		if len(part) > 0 {
			truncated := appendUpTo(&out, part, limit)
			if len(out) >= limit && (errors.Is(err, bufio.ErrBufferFull) || truncated) {
				if dErr := discardMessageRest(r); dErr != nil && !errors.Is(dErr, io.EOF) {
					return out, true, dErr
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
	_, after, ok := bytes.Cut(prefix, []byte(key))
	if !ok {
		return nil
	}
	rest := after
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
	return errRespData(id, code, msg, nil)
}

// errRespData is errResp plus the structured error envelope. An empty or nil
// data map leaves `error.data` off the wire entirely, so every existing
// errResp caller produces a byte-identical payload.
func errRespData(id any, code int, msg string, data map[string]any) mcpResponse {
	e := &mcpError{Code: code, Message: msg}
	if len(data) > 0 {
		e.Data = data
	}
	return mcpResponse{JSONRPC: "2.0", ID: id, Error: e}
}
