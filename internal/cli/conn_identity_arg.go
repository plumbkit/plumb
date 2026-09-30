package cli

import "strings"

// conn_identity_arg.go — which clients need the identity stamp key declared in
// every tool schema before it will reach the daemon.

// localAgentModeClientPrefix is the clientInfo.name prefix of Claude desktop's
// connector ("local-agent-mode-<server key in claude_desktop_config.json>").
// One such connection serves every desktop conversation, chat and Code tab
// alike, across projects, so per-call identity is the only thing that tells
// them apart. The connector applies the PreToolUse hook's updatedInput but
// forwards only the argument keys a tool's schema declares; observed
// 2026-09-30: a hook-added session_id arrived, an undeclared probe key and the
// reverse-DNS stamp did not.
const localAgentModeClientPrefix = "local-agent-mode-"

// clientStripsUndeclaredArgs reports whether this connection's client drops
// tool arguments its schema does not declare, so tools/list must advertise
// mcp.ArgLogicalAgentDeclaredKey for the identity stamp to survive.
func (s *connSession) clientStripsUndeclaredArgs() bool {
	return strings.HasPrefix(s.clientNameStr(), localAgentModeClientPrefix)
}

// isHookClient reports a client the Claude Code identity hook stamps: Claude
// Code itself, or Claude desktop's connector (whose Code-tab sessions run it).
func (s *connSession) isHookClient() bool {
	name := s.clientNameStr()
	return name == "claude-code" || strings.HasPrefix(name, localAgentModeClientPrefix)
}
