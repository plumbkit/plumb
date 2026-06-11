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

	Tokeniser Family
}

// registry holds one entry per known client. Claude Desktop is the thin client
// (no native filesystem, search, shell, or LSP); the CLI agents (Claude Code,
// Codex, Gemini) carry strong local file/search/shell access but no native LSP;
// unknownCaps is the conservative default for any unrecognised client — it
// assumes capable local tooling, which credits efficiency (small) rather than
// capability (large), keeping estimates defensibly low.
var registry = []Capabilities{
	{
		Name:     "claude-desktop",
		Prefixes: []string{"claude-desktop", "claude-ai", "claude"},
		// Thin client: no native filesystem, search, shell, or LSP.
		Tokeniser: FamilyClaude,
	},
	{
		Name:           "claude-code",
		Prefixes:       []string{"claude-code"},
		NativeFileRead: true,
		NativeSearch:   true,
		NativeShell:    true,
		Tokeniser:      FamilyClaude,
	},
	{
		Name:           "codex",
		Prefixes:       []string{"codex"},
		NativeFileRead: true,
		NativeSearch:   true,
		NativeShell:    true,
		Tokeniser:      FamilyGPT,
	},
	{
		Name:           "gemini",
		Prefixes:       []string{"gemini"},
		NativeFileRead: true,
		NativeSearch:   true,
		NativeShell:    true,
		Tokeniser:      FamilyGemini,
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
