package tools

import (
	"fmt"
	"os"
	"strings"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
)

// resolveShowDiff resolves a per-session show_write_diff toggle. A nil resolver
// means the tool was constructed without WithShowWriteDiff (e.g. in tests), in
// which case it defaults to on — matching the config default.
func resolveShowDiff(fn func() bool) bool {
	if fn == nil {
		return true
	}
	return fn()
}

// applySingleEdit runs the standard apply-or-preview flow used by every
// symbol-edit tool. summary is the human-readable verb ("inserted before",
// "replaced", etc.) used in the dry-run / applied output. When showDiff is true
// the response carries a unified diff of the change — a preview in dry-run, the
// applied change otherwise.
func applySingleEdit(uri string, edit protocol.TextEdit, dryRun, showDiff bool, summary string, sym *protocol.DocumentSymbol, viaFallback bool) (string, error) {
	path := paths.URIToPath(uri)
	diff := ""
	if showDiff {
		diff = symbolEditDiff(path, edit)
	}
	var sb strings.Builder
	if viaFallback {
		sb.WriteString("[topology fallback — LSP unavailable; symbol located by tree-sitter, range is line-granular]\n\n")
	}
	if dryRun {
		sb.WriteString("DRY RUN — file not modified.\n\n")
		fmt.Fprintf(&sb, "Would %s symbol %q in %s\n", summary, sym.Name, path)
		fmt.Fprintf(&sb, "  Range: line %d char %d → line %d char %d\n",
			edit.Range.Start.Line, edit.Range.Start.Character,
			edit.Range.End.Line, edit.Range.End.Character)
		if diff != "" {
			sb.WriteString("\n")
			sb.WriteString(diff)
		}
		sb.WriteString("\nTo apply, re-run with dry_run=false.")
		return sb.String(), nil
	}
	if err := applyTextEditsToFile(path, []protocol.TextEdit{edit}); err != nil {
		return "", fmt.Errorf("applying edit: %w", err)
	}
	fmt.Fprintf(&sb, "%s symbol %q in %s\n", capitalise(summary), sym.Name, path)
	if diff != "" {
		sb.WriteString("\n")
		sb.WriteString(diff)
	}
	return sb.String(), nil
}

// symbolEditDiff renders the unified diff a single TextEdit would produce
// against the current on-disk content. Returns "" if the file can't be read or
// the edit can't be applied in-memory — the diff is best-effort presentation,
// never a hard failure of the edit itself.
func symbolEditDiff(path string, edit protocol.TextEdit) string {
	old, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	out, err := applyTextEdits(old, []protocol.TextEdit{edit})
	if err != nil {
		return ""
	}
	return unifiedDiff(path, string(old), string(out))
}

func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
