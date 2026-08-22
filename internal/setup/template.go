package setup

import "github.com/plumbkit/plumb/internal/clienttemplates"

// DefaultVersion, DefaultTemplate, MaxTemplateLines, TemplateLineCount, and
// TemplateWithinBudget re-export internal/clienttemplates under setup's
// established names — kept for API stability (internal/cli and this
// package's own tests reference them). PLAN-366 relocated the underlying
// body and budget logic to internal/clienttemplates (a Foundation-layer
// package) so internal/mcp can render the SAME client-agnostic fallback into
// the MCP initialize `instructions` field for an unrecognised client, without
// importing this Domain-layer package (internal/arch's layer rule forbids
// Transport importing Domain). See clienttemplates.DefaultTemplate for the
// full rationale this constant's body follows.
const (
	DefaultVersion   = clienttemplates.DefaultVersion
	DefaultTemplate  = clienttemplates.DefaultTemplate
	MaxTemplateLines = clienttemplates.MaxLines
)

// TemplateLineCount returns the number of lines in body — the same measure
// TemplateWithinBudget applies, and the one an author bumping DefaultTemplate
// should check before committing. Delegates to clienttemplates.LineCount.
func TemplateLineCount(body string) int {
	return clienttemplates.LineCount(body)
}

// TemplateWithinBudget reports whether body fits inside MaxTemplateLines.
// Delegates to clienttemplates.WithinBudget.
func TemplateWithinBudget(body string) bool {
	return clienttemplates.WithinBudget(body)
}
