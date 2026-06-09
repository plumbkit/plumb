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
