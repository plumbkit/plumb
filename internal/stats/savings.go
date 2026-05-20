package stats

import "strings"

// charsPerToken is a rough English/code average used to estimate how many
// tokens a given byte count occupies. Basis: GPT-3 tokeniser average.
const charsPerToken = 4

// Normalised client name constants for profile lookup.
const (
	clientClaudeDesktop = "claude-desktop"
	clientClaudeCode    = "claude-code"
	clientCodex         = "codex"
	clientGemini        = "gemini"
	clientUnknown       = "unknown"
)

// profiles maps normalised client name → tool → estimated alternative token
// cost (the tokens the client would spend to obtain the same answer without
// plumb). Zero means plumb provides no advantage over that client's native
// capabilities for that tool. Values are conservative estimates — a lower,
// defensible number is better than an inflated one users cannot trust.
//
//   - claude-desktop: weak filesystem/shell access; every plumb query saves context.
//   - claude-code: strong local file/shell access; savings come from LSP semantics.
//   - codex: same profile as claude-code (strong local file/shell access).
//   - gemini: conservative fallback; profile data pending (same as claude-desktop).
//   - unknown: conservative medium; assumes moderate local capabilities.
var profiles = map[string]map[string]int{
	clientClaudeDesktop: {
		"find_symbol":       800,
		"workspace_symbols": 600,
		"get_definition":    250,
		"explain_symbol":    800,
		"list_symbols":      1600,
		"find_references":   400,
		"call_hierarchy":    1500,
		"type_hierarchy":    800,
		"diagnostics":       300,
	},
	clientClaudeCode: {
		// LSP semantic tools are still high-value: the alternative is multiple
		// Bash (grep/find) calls plus in-context reasoning over raw text.
		"list_symbols":      800,
		"find_symbol":       400,
		"workspace_symbols": 800,
		"get_definition":    250,
		"explain_symbol":    400,
		"find_references":   800,
		"call_hierarchy":    1500,
		"type_hierarchy":    800,
		// Filesystem/shell tools: CC has native equivalents, so savings are low.
		"diagnostics":     50,
		"search_in_files": 100,
	},
	clientCodex: {
		// Same profile as claude-code: Codex CLI has strong local file/shell access.
		"list_symbols":      800,
		"find_symbol":       400,
		"workspace_symbols": 800,
		"get_definition":    250,
		"explain_symbol":    400,
		"find_references":   800,
		"call_hierarchy":    1500,
		"type_hierarchy":    800,
		"diagnostics":       50,
		"search_in_files":   100,
	},
	clientGemini: {
		// Conservative fallback; pending real profile data.
		"find_symbol":       800,
		"workspace_symbols": 600,
		"get_definition":    250,
		"explain_symbol":    800,
		"list_symbols":      1600,
		"find_references":   400,
		"call_hierarchy":    1500,
		"type_hierarchy":    800,
		"diagnostics":       300,
	},
	clientUnknown: {
		// Conservative medium: unknown clients may have varying local capabilities.
		"list_symbols":      600,
		"find_symbol":       300,
		"workspace_symbols": 500,
		"get_definition":    200,
		"explain_symbol":    350,
		"find_references":   500,
		"call_hierarchy":    1000,
		"type_hierarchy":    500,
		"diagnostics":       150,
		"search_in_files":   50,
	},
}

// normaliseClient maps a raw MCP clientInfo.name to a canonical profile key.
// Matching is case-insensitive on the name prefix so versioned identifiers
// (e.g. "claude-code/1.2.3") and minor naming variants are handled correctly.
func normaliseClient(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.HasPrefix(n, "claude-code"):
		return clientClaudeCode
	case strings.HasPrefix(n, "claude"):
		return clientClaudeDesktop
	case strings.HasPrefix(n, "codex"):
		return clientCodex
	case strings.HasPrefix(n, "gemini"):
		return clientGemini
	default:
		return clientUnknown
	}
}

// TokensSaved returns estimated savings using the conservative unknown profile.
// For client-aware accounting use TokensSavedForClient.
func TokensSaved(tool string, outputBytes int) int {
	return TokensSavedForClient(tool, clientUnknown, outputBytes)
}

// TokensSavedForClient returns the estimated token savings for one tool
// invocation by a named MCP client. clientName is normalised via
// normaliseClient before lookup. Returns 0 when no model exists for the
// client+tool pair or when output bytes already exceed the alternative cost.
func TokensSavedForClient(tool, clientName string, outputBytes int) int {
	profile := profiles[normaliseClient(clientName)]
	if profile == nil {
		profile = profiles[clientUnknown]
	}
	alt, ok := profile[tool]
	if !ok || alt == 0 {
		return 0
	}
	plumbTokens := outputBytes / charsPerToken
	if plumbTokens >= alt {
		return 0
	}
	return alt - plumbTokens
}

// SavingsLabel returns the user-facing label for a savings total given a
// client name. Claude Desktop has no native filesystem or shell access, so
// plumb's value is expressed as capabilities enabled rather than tokens saved
// compared to a native alternative.
func SavingsLabel(clientName string) string {
	if normaliseClient(clientName) == clientClaudeDesktop {
		return "capabilities enabled"
	}
	return "tokens saved"
}

// HasSavingsModel reports whether tool participates in savings accounting for
// at least one client profile. Used as a fast skip in DB aggregation loops.
func HasSavingsModel(tool string) bool {
	for _, p := range profiles {
		if v, ok := p[tool]; ok && v > 0 {
			return true
		}
	}
	return false
}

// FormatSavings renders a token count as a short human string ("1.2k", "850").
func FormatSavings(tokens int) string {
	if tokens < 1000 {
		return itoa(tokens)
	}
	thousands := float64(tokens) / 1000
	s := strings.Builder{}
	if thousands < 10 {
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
