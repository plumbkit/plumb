package stats

import "strings"

// Per-tool token-savings model.
//
// Each tool's "alternative cost" is the approximate token count the LLM
// would have spent to get the same answer without plumb (e.g. via
// Read + grep). The savings for one call are:
//
//   tokens_saved = alternative_tokens - plumb_tokens
//
// where plumb_tokens = output_bytes / charsPerToken. The numbers below
// come from the savings table at the top of site/index.html, measured
// against the plumb codebase. They are estimates, not exact accounting —
// good enough to convey "you saved roughly N tokens this week."
//
// Tools without an entry default to a 1:1 ratio (zero savings).

const charsPerToken = 4 // rough English/code average

// altCost is the approximate alternative token count for one call of a given
// tool. Negative tools (no benefit over filesystem) return 0.
var altCost = map[string]int{
	"find_symbol":       800,
	"workspace_symbols": 600,
	"get_definition":    250,
	"explain_symbol":    800,
	"list_symbols":      1600,
	"find_references":   400,
	"call_hierarchy":    1500,
	"type_hierarchy":    800,
	"diagnostics":       300,
	// Filesystem & VCS tools roughly match their fallback cost — no savings.
	"list_files":      0,
	"find_files":      0,
	"search_in_files": 0,
	"file_diff":       0,
	"git":             0,
	// Edit and memory tools have no clear "filesystem alternative" — savings 0.
}

// TokensSaved returns the estimated token savings for one tool invocation.
// Returns 0 if the tool has no model entry or output_bytes already exceeds
// the alternative.
func TokensSaved(tool string, outputBytes int) int {
	alt, ok := altCost[tool]
	if !ok || alt == 0 {
		return 0
	}
	plumbTokens := outputBytes / charsPerToken
	if plumbTokens >= alt {
		return 0
	}
	return alt - plumbTokens
}

// HasSavingsModel reports whether a tool participates in savings accounting.
func HasSavingsModel(tool string) bool {
	v, ok := altCost[tool]
	return ok && v > 0
}

// FormatSavings renders a token count as a short human string ("1.2k", "850").
func FormatSavings(tokens int) string {
	if tokens < 1000 {
		return itoa(tokens)
	}
	thousands := float64(tokens) / 1000
	s := strings.Builder{}
	if thousands < 10 {
		// one decimal
		whole := int(thousands)
		tenth := int(thousands*10) - whole*10
		s.WriteString(itoa(whole))
		s.WriteByte('.')
		s.WriteString(itoa(tenth))
	} else {
		s.WriteString(itoa(int(thousands)))
	}
	s.WriteByte('k')
	return s.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
