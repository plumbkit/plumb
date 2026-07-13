package cli

import (
	"strings"

	"github.com/plumbkit/plumb/internal/clientcaps"
	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/tools"
)

// resolveToolProfile decides the effective tool profile for this connection:
// an explicit per-client override wins, then an explicit [tools] profile, then
// the auto-mode policy (autoProfile). The profile is always "lean" or "full"
// (auto is resolved away); reason documents which rule fired —
// "client-override", "explicit-config", or one of autoProfileFor's auto
// reasons. An override or config value of "auto" falls through to the next
// rule rather than counting as an override/explicit hit, so the auto reason
// still surfaces.
func (s *connSession) resolveToolProfile() (profile, reason string) {
	cfg := s.toolsConfig()
	client := s.clientNameStr()
	if p := lookupClientProfile(cfg.ClientProfiles, client); p != "" && p != "auto" {
		return p, "client-override"
	}
	if cfg.Profile != "" && cfg.Profile != "auto" {
		return cfg.Profile, "explicit-config"
	}
	return autoProfile(client)
}

// autoProfile resolves a client name to its declared capabilities and
// delegates the auto-mode policy decision to autoProfileFor, which is a pure
// function unit-testable against synthetic Capabilities.
func autoProfile(client string) (string, string) {
	return autoProfileFor(clientcaps.Lookup(client))
}

// autoProfileFor is the auto-mode policy given a client's declared
// capabilities. Lean is opt-in: it requires an explicit, reviewed
// ReliableDeferredToolDiscovery declaration, never an inference from native
// file/search/shell possession — a client can have strong native tooling and
// still be unable to reliably discover or invoke a tool absent from its
// initial tools/list surface. Order matters: an UNKNOWN client (unproven by
// definition) and a SchemaDiscoveryOnly client (one that builds its tool set,
// including any ToolSearch deferred list, purely from tools/list — a
// lean-hidden tool is unreachable rather than merely undisplayed, e.g. Claude
// Code) both always get "full" regardless of the deferred-discovery flag.
// Every other client defaults to "full" until verified true.
func autoProfileFor(caps clientcaps.Capabilities) (profile, reason string) {
	if caps.Name == "unknown" {
		return "full", "unknown-deferred-discovery"
	}
	if caps.SchemaDiscoveryOnly {
		return "full", "schema-discovery-only-client"
	}
	if caps.ReliableDeferredToolDiscovery {
		return "lean", "verified-deferred-discovery"
	}
	return "full", "unverified-deferred-discovery"
}

// toolVisible is the mcp.Server.ToolFilter body: the bootstrap set is always
// advertised regardless of profile; otherwise under the lean profile only the
// lean set is advertised, and under full every tool is. A hidden tool is still
// callable by name (handleToolsCall ignores the filter).
func (s *connSession) toolVisible(name string) bool {
	// The bootstrap invariant is checked BEFORE the profile, independent of
	// LeanTools membership (see tools.BootstrapTools) — a future profile
	// change can never silently drop session_start/git/read_file/edit_file
	// from the initial tools/list.
	if tools.IsBootstrap(name) {
		return true
	}
	profile, _ := s.resolveToolProfile()
	if profile == "lean" {
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
