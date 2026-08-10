// Package clientcaps is the single source of truth for what an MCP client can do
// natively, and the counterfactual savings model layered on top of it.
//
// It mirrors the internal/langsupport pattern: a client is one struct literal in
// a registry, so adding Cursor, Zed, or a custom agent is a data change, not a
// code change. The package depends on the standard library only, so it can sit
// below either the cli scorer that drives it or the stats package.
//
// Concurrency: every value here is immutable package-level data, initialised once
// and only read thereafter. All exported functions are pure and safe for
// concurrent use.
package clientcaps

import "strings"

// Family identifies a tokeniser family. Different model families pack a different
// number of characters into a token, so a byte count maps to a token estimate
// through a family- and content-specific ratio (see tokeniser.go).
type Family string

const (
	FamilyClaude Family = "claude"
	FamilyGPT    Family = "gpt"
	FamilyGemini Family = "gemini"
)

// Capabilities declares what one MCP client can do without plumb. The booleans
// gate the counterfactual model: a client that cannot read files natively is
// credited the full value of a plumb read (capability), whereas one that can is
// credited only the efficiency delta.
type Capabilities struct {
	// Name is the canonical key for this client.
	Name string
	// Prefixes are the case-insensitive clientInfo.name prefixes that resolve to
	// this entry. This subsumes the old normaliseClient switch: alias handling is
	// data, co-located with the capabilities it selects.
	Prefixes []string

	NativeFileRead bool // native file read (Read tool, cat, etc.)
	NativeSearch   bool // native content search (grep/ripgrep equivalent)
	NativeShell    bool // arbitrary shell access
	NativeLSP      bool // native semantic/LSP understanding of code

	// SchemaDiscoveryOnly is true when the client can only invoke tools it has
	// been advertised in tools/list (it builds its tool set, including any
	// deferred-tool/ToolSearch list, from that response). A tool hidden from
	// tools/list is then unreachable — the lean profile's "callable by name"
	// escape hatch does not apply — so such a client must get the full profile.
	SchemaDiscoveryOnly bool

	// ReliableDeferredToolDiscovery is true only when integration coverage has
	// demonstrated the client's model can reliably discover and invoke a tool
	// absent from its initial tools/list surface (deferred/lazy tool
	// registries, e.g. a ToolSearch-style mechanism). Unknown or unproven ⇒
	// false. This is the sole gate for the auto-mode lean profile: lean is
	// opt-in via this explicit, reviewed declaration, never inferred from
	// native file/search/shell capability. Promoting a client to true is a
	// reviewed data change, not an inference.
	ReliableDeferredToolDiscovery bool

	// ClientSideAllowlist is true when `plumb setup <client> --lean` can write a
	// tool allowlist into THIS client's own MCP config — Kimi Code's
	// enabledTools, Codex's enabled_tools, Gemini CLI's includeTools.
	//
	// It governs what plumb may SAY, never what it serves. The filter is applied
	// by the client before a call reaches plumb, and plumb cannot observe whether
	// it is in force: tools/call arrives identically either way, and the daemon is
	// shared and long-lived, so its environment and home directory are not
	// reliably the connecting client's — reading the client's config from here
	// would sometimes read a different machine account's file and claim knowledge
	// plumb does not have. So the honest response is to write guidance that is
	// correct in BOTH states: for these clients, name only tools in the lean set
	// (the set --lean pins), because anything else may have been filtered out.
	//
	// This is strictly stronger than the lean PROFILE's constraint. A profile-lean
	// tool is merely undisplayed and stays callable by name; a client-side
	// allowlist removes it, and there is no escape hatch.
	ClientSideAllowlist bool

	Tokeniser Family
}

// registry holds one entry per known client. Claude Desktop is the thin client
// (no native filesystem, search, shell, or LSP); the CLI agents (Claude Code,
// Codex, Gemini, Kimi Code) carry strong local file/search/shell access but no
// native LSP; unknownCaps is the conservative default for any unrecognised
// client — it assumes capable local tooling, which credits efficiency (small)
// rather than capability (large), keeping estimates defensibly low.
var registry = []Capabilities{
	{
		Name:     "claude-desktop",
		Prefixes: []string{"claude-desktop", "claude-ai", "claude"},
		// Thin client: no native filesystem, search, shell, or LSP.
		Tokeniser: FamilyClaude,
	},
	{
		// Claude Code builds its tool list (and its ToolSearch deferred-tool list)
		// only from tools/list, so a lean-hidden tool has no schema to load and
		// cannot be invoked — it therefore needs the full profile regardless of
		// ReliableDeferredToolDiscovery. Codex and Gemini leave
		// ReliableDeferredToolDiscovery unset (false), so auto mode gives them the
		// full profile too, until integration coverage proves their deferred-tool
		// invocation behaviour and a reviewed change flips the flag.
		Name:                "claude-code",
		Prefixes:            []string{"claude-code"},
		NativeFileRead:      true,
		NativeSearch:        true,
		NativeShell:         true,
		SchemaDiscoveryOnly: true,
		Tokeniser:           FamilyClaude,
	},
	{
		Name:           "codex",
		Prefixes:       []string{"codex"},
		NativeFileRead: true,
		NativeSearch:   true,
		NativeShell:    true,
		// `plumb setup codex --lean` writes tools.LeanToolNames() into the
		// enabled_tools key of [mcp_servers.plumb] in the user's config.toml.
		ClientSideAllowlist: true,
		Tokeniser:           FamilyGPT,
	},
	{
		Name:           "gemini",
		Prefixes:       []string{"gemini"},
		NativeFileRead: true,
		NativeSearch:   true,
		NativeShell:    true,
		// `plumb setup gemini --lean` writes tools.LeanToolNames() into the
		// includeTools key of mcpServers.plumb in the user's settings.json.
		ClientSideAllowlist: true,
		Tokeniser:           FamilyGemini,
	},
	{
		// Kimi Code is schema-discovery-only: it builds its tool set purely from
		// tools/list and has no deferred-tool/ToolSearch mechanism, so a
		// lean-hidden tool would be unreachable rather than merely undisplayed —
		// auto mode must serve it "full" (reason schema-discovery-only-client).
		// ReliableDeferredToolDiscovery is deliberately left unset: it is the
		// evidence-gated lean opt-in, and no deferred-discovery behaviour has been
		// demonstrated here. The token relief for Kimi Code comes instead from a
		// CLIENT-side allowlist — `plumb setup kimi-code --lean` writes
		// tools.LeanToolNames() into the enabledTools key of Kimi's own mcp.json.
		// Kimi Code was the first client to get one, not the only one: Codex and
		// Gemini CLI now carry the same ClientSideAllowlist flag, and it is that
		// flag, not this entry, that guidance keys off.
		//
		// Tokeniser: Kimi K2's BPE is tiktoken-lineage, so FamilyGPT is the
		// closest modelled family; a FamilyKimi with invented ratios would be fake
		// precision, not better accuracy.
		//
		// Prefixes: "kimi-code" is the clientInfo.name observed from a real Kimi
		// Code handshake (recorded as client_name in the session registry); the
		// bare "kimi" alias covers sibling products (Kimi Desktop) that share the
		// same mcp.json. Longest-prefix matching in Lookup keeps "kimi-code"
		// winning over "kimi". If a future build reports some other name, Lookup
		// simply falls through to unknownCaps and the client degrades gracefully
		// to unrecognised-client behaviour (still "full", reason
		// unknown-deferred-discovery) — never to a broken lean surface.
		//
		// WHY THE BARE ALIAS SHARES THIS ENTRY, unlike claude/claude-desktop.
		// Reading it as "Kimi Desktop is handed the CLI's native-capability
		// flags" overstates the difference: unknownCaps — where a bare "kimi"
		// would land without the alias — ALSO sets NativeFileRead, NativeSearch
		// and NativeShell true, so all three are identical either way. The
		// claude/claude-desktop split exists for the opposite reason: Claude
		// Desktop is *known* to be thin, so its entry DEPARTS from those
		// conservative defaults on evidence. No such evidence exists for Kimi
		// Desktop, and inventing a thin profile for it would be a claim, not a
		// finding — the same fake-precision trap the tokeniser note above avoids.
		//
		// The alias only changes the two fields where sharing is the safer
		// answer. SchemaDiscoveryOnly resolves the profile to "full"
		// (schema-discovery-only-client) where unknownCaps resolves it to "full"
		// too (unknown-deferred-discovery) — same served surface, and the
		// dangerous direction would be a wrongly-absent flag on a client that
		// cannot invoke an unadvertised tool, never a wrongly-present one.
		// Tokeniser FamilyGPT is nearer a Kimi model than unknownCaps'
		// FamilyClaude. So the alias is strictly better-informed than the
		// fallback and cannot cost capability.
		//
		// This is deliberately NOT the rule isKimiCode follows
		// (session_start_detect.go), which refuses the bare alias. The two
		// answer different questions: this one picks the closest capability
		// profile among defensible options, where being approximately right is
		// the goal; that one gates PROSE naming Kimi Code's own edit tools and
		// mcp.json, where being approximately right means telling a sibling
		// product something false. Capability estimates degrade; wrong advice
		// does not.
		Name:                "kimi-code",
		Prefixes:            []string{"kimi-code", "kimi"},
		NativeFileRead:      true,
		NativeSearch:        true,
		NativeShell:         true,
		SchemaDiscoveryOnly: true,
		ClientSideAllowlist: true,
		Tokeniser:           FamilyGPT,
	},
}

// unknownCaps is returned for any client name that matches no registry prefix.
// Conservative: assume capable local tooling so unrecognised clients earn the
// efficiency delta, not the larger capability credit.
var unknownCaps = Capabilities{
	Name:           "unknown",
	NativeFileRead: true,
	NativeSearch:   true,
	NativeShell:    true,
	Tokeniser:      FamilyClaude,
}

// Lookup resolves a raw MCP clientInfo.name to its Capabilities. Matching is
// case-insensitive on the longest registered prefix, so a versioned identifier
// ("claude-code/1.2.3") and the more specific "claude-code" both beat the bare
// "claude" entry. Unrecognised clients get the conservative unknown profile.
func Lookup(clientName string) Capabilities {
	n := strings.ToLower(strings.TrimSpace(clientName))
	best := unknownCaps
	bestLen := 0
	for _, c := range registry {
		for _, p := range c.Prefixes {
			if len(p) > bestLen && strings.HasPrefix(n, p) {
				best = c
				bestLen = len(p)
			}
		}
	}
	return best
}
