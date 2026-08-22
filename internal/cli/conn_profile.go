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
// capabilities — the progressive ladder (strategy doc §5 W2-15): the baseline
// every client gets with zero positive detection is now LEAN (a session_start
// pointer names what tools/list omits, so nothing is actually unreachable —
// see session_start_guidance.go's writeGenericGuidance), and each rung above
// that is a reviewed, evidence-based capability that earns MORE, never less.
// Detection only ever adds.
//
// Order matters, and each rung is evaluated only once the ones above it have
// ruled themselves out:
//
//  1. SchemaDiscoveryOnly — the client builds its tool set (including any
//     ToolSearch deferred list) purely from tools/list, so a lean-hidden tool
//     has no schema to load and is unreachable, not merely undisplayed (e.g.
//     Claude Code, Kimi Code). Always "full", regardless of every other flag —
//     this is the one rung a positive ReliableDeferredToolDiscovery cannot
//     override, because the two facts would be contradictory.
//  2. ReliableDeferredToolDiscovery — reviewed, evidence-based proof (G8) that
//     the client's model reliably discovers and invokes a tool absent from its
//     initial tools/list surface. No shipped client carries this yet; it is
//     never inferred from native file/search/shell possession.
//  3. ClientSideAllowlist — `plumb setup <client> --lean` can write a tool
//     allowlist into the client's OWN config (Kimi Code, Codex, Gemini CLI).
//     Plumb still SERVES full — the filter, if any, is applied client-side,
//     invisibly to plumb — but the reason is now distinct from the generic
//     conservative default so callers can render the client-side-filter
//     caveat truthfully (see tools.ClientSideAllowlistNote).
//  4. An actually-unrecognised client (Name == "unknown", i.e. it matched no
//     registry prefix) gets the new lean baseline: zero positive evidence, so
//     zero reason to withhold it, and the session_start pointer keeps it
//     working. This does NOT apply to a REGISTERED client that simply has none
//     of the flags above (e.g. Claude Desktop, Junie) — those keep today's
//     conservative default below, because at least one of them (Claude
//     Desktop) is documented to have zero native fallback for the commodity
//     tools lean hides, and flipping a working, already-shipped default without
//     evidence would be exactly the tool-removal the Do-NOT forbids.
//  5. Every other client (registered, but none of the above) defaults to
//     "full" until one of the flags above is verified true — today's
//     behaviour for claude-desktop and junie, unchanged.
func autoProfileFor(caps clientcaps.Capabilities) (profile, reason string) {
	if caps.SchemaDiscoveryOnly {
		return "full", "schema-discovery-only-client"
	}
	if caps.ReliableDeferredToolDiscovery {
		return "lean", "verified-deferred-discovery"
	}
	if caps.ClientSideAllowlist {
		return "full", "client-side-allowlist"
	}
	if caps.Name == "unknown" {
		return "lean", "unknown-client-baseline"
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
