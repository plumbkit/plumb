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

	// SupportsMCPInstructions is true only where shipped evidence shows the
	// client surfaces the MCP `initialize` response's `instructions` field to
	// the model as a system-prompt-style hint (internal/mcp/instructions.go's
	// InstructionsForClient, internal/mcp/server_handlers.go — sent to every
	// client today regardless of this flag, since an unaware client just
	// ignores an unknown field). PLAN-366 renders a per-client body into that
	// field, drawn from the same internal/clienttemplates source as this
	// client's managed AGENTS.md/CLAUDE.md/GEMINI.md block (PLAN-364) — this
	// flag is evidence of observed CONSUMPTION, not a gate InstructionsForClient
	// reads: it renders for every client with a clienttemplates body whether or
	// not this flag is set. Unproven ⇒ false, the same evidence discipline as
	// ReliableDeferredToolDiscovery.
	SupportsMCPInstructions bool

	// SupportsAlwaysLoadPin is true only where shipped evidence shows the
	// client reads Claude Code's proprietary tools/list extension
	// `_meta["anthropic/alwaysLoad"]=true` (internal/mcp/meta_keys.go,
	// PLAN-355) to pin a tool's schema into context ahead of a ToolSearch
	// round-trip. Claude Code is the only proven case. Declared data only:
	// conn_register.go still sets srv.AlwaysLoad unconditionally for every
	// client, since the meta key costs a few bytes per tool that an unaware
	// client silently ignores — gating it per-client to reclaim those bytes is
	// a follow-up, not a correctness fix this flag forces in this PR.
	SupportsAlwaysLoadPin bool

	// DescriptionCapRunes, when nonzero, is a measured rune ceiling the client
	// truncates a single tool description at. Zero means no cap has been
	// measured for this client — NOT "no cap exists". Populate it only from a
	// reviewed measurement, the same rule ReliableDeferredToolDiscovery follows
	// — guessing a number here would be fake precision, not a real cap.
	//
	// claude-code is the one measured row (PLAN-370): a live Claude Code
	// session truncated six tool descriptions, and locating each one's last
	// surviving word in its source string puts every cut at exactly 2048 —
	// see internal/tools/profile_test.go's maxDescriptionChars comment for the
	// full evidence. No other client has been measured; a description
	// conformance check for an unmeasured client falls back to the strictest
	// known cap (clientcaps.StrictestDescriptionCapRunes) rather than skipping
	// the check — an absent number is evidence of nothing, not evidence of
	// safety.
	DescriptionCapRunes int

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
		// SupportsMCPInstructions: unmeasured. The `instructions` field's own
		// CHANGELOG entry names Claude Desktop as injecting it, but that is a
		// shipping blurb, not an observed integration result — no test exercises
		// it, and nothing consumes this flag yet. Leave false until a reviewed
		// measurement says otherwise, same discipline as ReliableDeferredToolDiscovery.
		Tokeniser: FamilyClaude,
	},
	{
		Name:           "junie",
		Prefixes:       []string{"junie-client", "junie"},
		NativeFileRead: true,
		NativeSearch:   true,
		NativeShell:    true,
		Tokeniser:      FamilyGPT,
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
		// SupportsMCPInstructions: Claude Code is dogfooded directly on this very
		// codebase — every Claude Code agent session working on plumb receives
		// this client's rendered `instructions` body as its own MCP server
		// preamble and visibly acts on it (opens with session_start, per the
		// tool's own description and the AGENTS.md quick reference this body's
		// content is aligned with — PLAN-366), which is first-hand observed
		// behaviour, not a shipping blurb. The claude-desktop/gemini rows have no
		// equivalent first-party evidence — their only source is the
		// `instructions` field's own CHANGELOG entry naming them — so they stay
		// false pending a real measurement.
		// SupportsAlwaysLoadPin: shipped — PLAN-355's AlwaysLoad pin ladder
		// (conn_register.go) is Claude-Code-specific in practice today (the sole
		// proven reader of _meta["anthropic/alwaysLoad"]).
		SupportsMCPInstructions: true,
		SupportsAlwaysLoadPin:   true,
		// DescriptionCapRunes: measured (PLAN-370) — see the field's doc comment
		// for the live-truncation evidence.
		DescriptionCapRunes: 2048,
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
		// SupportsMCPInstructions: unmeasured, same reasoning as claude-desktop
		// above — the CHANGELOG names Gemini CLI, but that is not an observed
		// result. Leave false until measured.
		Tokeniser: FamilyGemini,
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
	{
		// ZCode (Z.ai's desktop client, setup_zcode.go) is a CLI-style coding
		// agent — it gets its own skills directory (~/.zcode/skills) like Claude
		// Code, Codex, Junie, and Kimi Code, unlike the thin claude-desktop chat
		// client — so native file/search/shell are set true on the same evidence
		// basis as those clients. No --lean allowlist exists for it: setup_zcode.go
		// documents that an unknown key on ZCode's strict server schema causes the
		// server to be dropped entirely, so ClientSideAllowlist stays false. Its
		// SchemaDiscoveryOnly / ReliableDeferredToolDiscovery / SupportsMCPInstructions
		// / SupportsAlwaysLoadPin behaviour has not been measured — all left false,
		// so auto mode falls through to today's conservative default ("full",
		// unverified-deferred-discovery), exactly like Codex and Gemini before their
		// allowlist flag existed. Tokeniser: no GLM-specific ratio has been
		// measured, so FamilyGPT (tiktoken-lineage BPE) is the closest defensible
		// family, the same reasoning the Kimi Code entry above gives rather than
		// inventing a bespoke ratio.
		Name:           "zcode",
		Prefixes:       []string{"zcode"},
		NativeFileRead: true,
		NativeSearch:   true,
		NativeShell:    true,
		Tokeniser:      FamilyGPT,
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

// All returns every registered client's Capabilities, in registry order. It
// exists for callers — the PLAN-370 description-conformance check is the
// first — that must iterate every known client without duplicating the
// registry itself, so a client added here is covered automatically. The
// slice is a copy; mutating it does not affect the package's registry.
func All() []Capabilities {
	out := make([]Capabilities, len(registry))
	copy(out, registry)
	return out
}

// StrictestDescriptionCapRunes returns the smallest nonzero DescriptionCapRunes
// among all registered clients — the cap a conformance check should apply to a
// client whose own DescriptionCapRunes is unmeasured (0). An absent measurement
// is evidence of nothing, not evidence the client tolerates a longer
// description than the one client that has been measured, so "no data" must
// not read as "no limit". Returns 0 only if no client's cap has been measured
// yet, in which case a caller has nothing to enforce against.
func StrictestDescriptionCapRunes() int {
	strictest := 0
	for _, c := range registry {
		if c.DescriptionCapRunes == 0 {
			continue
		}
		if strictest == 0 || c.DescriptionCapRunes < strictest {
			strictest = c.DescriptionCapRunes
		}
	}
	return strictest
}
