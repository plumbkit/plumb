package mcp

// server_initialize.go — the MCP initialize exchange.
//
// Split from server_handlers.go, which owns the tools surface (list, call,
// refusals) and the wire plumbing. Initialize is a distinct responsibility with
// its own contract: it is the one exchange every connection performs exactly
// once, it negotiates the protocol revision, it dispatches the per-connection
// param hooks, and — since PLAN-426 — it carries the application's identity
// `_meta` back to the client.

import (
	"context"
	"encoding/json"
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
		Meta            map[string]any `json:"_meta,omitempty"`
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
		instructions = InstructionsForClient(clientInfoNameFromParams(req.Params))
	case "-":
		instructions = ""
	}
	res := result{
		ProtocolVersion: answered,
		Capabilities:    serverCaps,
		ServerInfo:      serverInfoWire{Name: s.info.Name, Version: s.info.Version},
		Instructions:    instructions,
		Meta:            s.initializeMeta(ctx),
	}
	return okResp(req.ID, res)
}

// initializeMeta collects the `_meta` an application contributes to the
// initialize result. Nil when nothing is wired or the callback declines, which
// leaves the response byte-identical to the one that shipped before this hook
// existed — the compatibility property the whole additive design rests on.
//
// It runs AFTER fireInitParamHooks, and that ordering is load-bearing rather
// than incidental: those hooks are what tell the application which proxy
// session this connection is, and the application cannot state an identity it
// has not yet been asked to restore.
//
// The callback returns an opaque map. internal/mcp is transport and deliberately
// knows nothing about sessions, so the shape of what travels under each key is
// the application's business; this only owns the key vocabulary and the
// forward-compatible envelope.
func (s *Server) initializeMeta(ctx context.Context) map[string]any {
	if s.InitializeMeta == nil {
		return nil
	}
	meta := s.InitializeMeta(ctx)
	if len(meta) == 0 {
		return nil
	}
	return meta
}

// fireInitParamHooks dispatches the hooks that consume per-connection metadata
// from the initialize params: the client identity, and the `plumb serve`
// transport keys (allow-dirs, the stable proxy session ID, the workspace
// pre-pin). All fire synchronously, before OnInit, and each is skipped when its
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
