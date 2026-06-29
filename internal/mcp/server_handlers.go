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
	"time"
)

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

	if s.OnAllowDirs != nil && req.Params != nil {
		if dirs := allowDirsFromParams(req.Params); len(dirs) > 0 {
			s.OnAllowDirs(ctx, dirs)
		}
	}

	if s.OnProxySession != nil && req.Params != nil {
		if id := proxySessionFromParams(req.Params); id != "" {
			s.OnProxySession(ctx, id)
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
	caps := map[string]any{"tools": map[string]any{"listChanged": true}}
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
	switch instructions {
	case "":
		instructions = DefaultInstructions
	case "-":
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
	var p struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return ""
	}
	raw, ok := p.Meta[MetaProxySessionKey]
	if !ok {
		return ""
	}
	var id string
	if err := json.Unmarshal(raw, &id); err != nil {
		return ""
	}
	return id
}

func (s *Server) handleToolsList(req mcpRequest) mcpResponse {
	type toolDef struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"inputSchema"`
	}
	// Snapshot the registry under s.mu, then apply ToolFilter and build the
	// response with the lock released. A lightweight probe (ping, daemon_info)
	// contending on the per-connection write mutex must never queue behind a slow
	// filter or marshal held under the shared registry lock.
	snaps := s.snapshotTools()
	filter := s.ToolFilter // set before Serve; read without the lock
	defs := make([]toolDef, 0, len(snaps))
	for _, sn := range snaps {
		// A filtered-out tool is hidden from the advertised list but stays
		// callable by name — handleToolsCall does not consult ToolFilter.
		if filter != nil && !filter(sn.name) {
			continue
		}
		defs = append(defs, toolDef{
			Name:        sn.name,
			Description: sn.description,
			InputSchema: sn.schema,
		})
	}
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
	var text string
	args, warnings, err := s.resolveToolArgs(params.Name, params.Arguments)
	if err == nil {
		text, err = t.Execute(ctx, args)
	}
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
	if len(warnings) > 0 {
		text = aliasNotice(warnings) + text
	}
	if s.EnrichToolOutput != nil {
		text = s.EnrichToolOutput(ctx, params.Name, params.Arguments, text)
	}
	return okResp(req.ID, callResult{
		Content: []content{{Type: "text", Text: text}},
		IsError: false,
	})
}

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
	return mcpResponse{JSONRPC: "2.0", ID: id, Error: &mcpError{Code: code, Message: msg}}
}
