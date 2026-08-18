package cli

import (
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/plumbkit/plumb/internal/tui"
)

// statusStyle maps a short status word to its theme style. Callers must have
// run tui.RebuildStyles() (the returned style is the shared package var, not a copy).
func statusStyle(status string) lipgloss.Style {
	// Prefix match rather than equality because skill statuses carry suffixes,
	// e.g. "stale (installed by 0.15.1)". The negated keys are listed first so
	// the most specific key wins: should the matching ever broaden or a key be
	// added that overlaps another, "not registered" must keep its muted styling
	// rather than fall through to the success colour of "registered".
	switch {
	case strings.HasPrefix(status, "not installed"),
		strings.HasPrefix(status, "not registered"),
		strings.HasPrefix(status, "already current"),
		strings.HasPrefix(status, "skipped"),
		strings.HasPrefix(status, "current"):
		return tui.MutedStyle
	case strings.HasPrefix(status, "registered"),
		strings.HasPrefix(status, "updated"),
		strings.HasPrefix(status, "installed"):
		return tui.OkStyle
	case strings.HasPrefix(status, "missing"),
		strings.HasPrefix(status, "stale"),
		strings.HasPrefix(status, "error"),
		strings.HasPrefix(status, "uninstall"),
		strings.HasPrefix(status, "unregistered"):
		return tui.WarnStyle
	default:
		return lipgloss.NewStyle()
	}
}
