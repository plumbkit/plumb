package cli

import (
	"strings"

	"github.com/plumbkit/plumb/internal/clientcaps"
	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/tools"
)

// resolveToolProfile decides the effective tool profile for this connection:
// an explicit per-client override wins, then an explicit [tools] profile, then
// "auto". The result is "lean" or "full" (auto is always resolved away).
func (s *connSession) resolveToolProfile() string {
	cfg := s.toolsConfig()
	client := s.clientNameStr()
	if p := lookupClientProfile(cfg.ClientProfiles, client); p != "" && p != "auto" {
		return p
	}
	if cfg.Profile != "" && cfg.Profile != "auto" {
		return cfg.Profile
	}
	return autoProfile(client)
}

// autoProfile is the auto-mode decision: "lean" only for a RECOGNISED CLI agent
// that reads files and searches natively (so the hidden commodity tools have a
// safe native equivalent). Claude Desktop (a thin client) and any UNKNOWN client
// get "full" — the unknown fallback in clientcaps reports native tooling for
// scoring purposes, so we gate on the recognised name, not the bare booleans.
func autoProfile(client string) string {
	caps := clientcaps.Lookup(client)
	if caps.Name == "unknown" || caps.Name == "claude-desktop" {
		return "full"
	}
	if caps.NativeFileRead && caps.NativeSearch {
		return "lean"
	}
	return "full"
}

// toolVisible is the mcp.Server.ToolFilter body: under the lean profile only the
// lean set is advertised; under full every tool is. A hidden tool is still
// callable by name (handleToolsCall ignores the filter).
func (s *connSession) toolVisible(name string) bool {
	if s.resolveToolProfile() == "lean" {
		return tools.IsLean(name)
	}
	return true
}

// hiddenToolCount reports how many registered tools the lean profile suppresses
// from tools/list (used only for the session_start note). Full-profile tools are
// everything not in the lean set.
func hiddenToolCount(srv *mcp.Server) int {
	n := 0
	for _, name := range srv.ToolNames() {
		if !tools.IsLean(name) {
			n++
		}
	}
	return n
}

// lookupClientProfile resolves a per-client profile override by case-insensitive
// longest-prefix match on the configured client keys, mirroring clientcaps.Lookup
// so "claude-code/1.2" matches a "claude-code" key. Returns "" when none match.
func lookupClientProfile(profiles map[string]string, client string) string {
	if len(profiles) == 0 || client == "" {
		return ""
	}
	n := strings.ToLower(strings.TrimSpace(client))
	best := ""
	bestLen := -1
	for k, v := range profiles {
		kl := strings.ToLower(strings.TrimSpace(k))
		if len(kl) > bestLen && strings.HasPrefix(n, kl) {
			best = v
			bestLen = len(kl)
		}
	}
	return best
}
