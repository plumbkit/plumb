package arch

// This file declares the shared primitives that must have exactly one
// implementation, so "don't hand-roll this again" is a failing test rather than
// a thing reviewers have to notice.
//
// The rules exist because the alternative was measured. A 2026-06 audit found
// an atomic temp→rename write reimplemented five times and a byte formatter
// three times; consolidating them was filed, partly done, and by the time the
// remaining items were picked up the atomic write had regrown to twelve copies
// and the byte formatter to five. Nothing had failed in between. Extraction on
// its own does not hold — the tree has several agents editing it, a helper that
// is easier to retype than to find will be retyped, and the copy is invisible
// in review because it compiles and passes.
//
// Each rule names the one package allowed to own the primitive, plus an
// explicit allowlist of exceptions with the reason each is genuinely different.
// Adding an entry is deliberately a code edit with a written justification, not
// a silent regrowth.
//
// Concurrency: read-only package-level data, safe for concurrent use.

// PrimitiveRule declares a family of helper functions that belongs in exactly
// one package.
//
// A function matches the rule when its (lowercased) name carries one of
// Prefixes or equals one of Exact. Matching declarations outside Home fail
// TestSharedPrimitives unless listed in Allowed.
type PrimitiveRule struct {
	// Primitive names what is being protected, for the failure message.
	Primitive string

	// Home is the package (relative to the module root) that owns the single
	// implementation.
	Home string

	// Prefixes match any function whose lowercased name starts with one of
	// these.
	Prefixes []string

	// Exact matches a function whose lowercased name equals one of these. Used
	// where a prefix would be too broad — "clamp" alone catches clampWidth,
	// clampPercent and a dozen other numeric clamps that have nothing to do
	// with string budgets.
	Exact []string

	// Allowed maps "<package>.<func>" to the reason that declaration is
	// legitimately not a duplicate. Every entry must say why, not just that.
	Allowed map[string]string
}

// PrimitiveRules is the full set. Keep the reasons current: an allowlist entry
// whose justification has stopped being true is worse than no rule at all.
var PrimitiveRules = []PrimitiveRule{
	{
		Primitive: "string truncation",
		Home:      "internal/textfmt",
		Prefixes:  []string{"truncate"},
		Exact:     []string{"clampbytes", "clamptobytes", "ellipsis"},
		Allowed: map[string]string{
			"internal/tools.truncateLines":          "caps output by LINE count with a caller-supplied suffix — a different budget from textfmt's rune and byte helpers, and it never splits a line",
			"internal/tui.contractPathTruncateLeft": "path abbreviation: keeps the RIGHTMOST runes and prefixes the ellipsis, the opposite end from textfmt.Ellipsis",
			"cmd/clientsmoke.truncate":              "operates on []byte for harness log output, in a build-tagged test-only package that ships in no binary",
		},
	},
	{
		Primitive: "pluralisation",
		Home:      "internal/textfmt",
		Prefixes:  []string{"plural"},
		Allowed: map[string]string{
			"internal/cli.pluralSessionCount": "formats a whole clause including the count and a verb (\"1 session is\" / \"3 sessions are\"), not a bare word choice",
			"internal/cli.pluralisedFiles":    "language-aware: picks the noun by the detected language as well as the count",
			"internal/tools.pluralMemories":   "interpolates the count and switches the surrounding sentence, not just the noun",
		},
	},
	{
		Primitive: "byte-size formatting",
		Home:      "internal/textfmt",
		Exact: []string{
			"formatbytes", "humanbytes", "humanbytescompact",
			"formatsize", "bytesize", "bytesizelabel",
		},
		Allowed: map[string]string{},
	},
}
