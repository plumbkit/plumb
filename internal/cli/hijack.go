package cli

import (
	"fmt"
	"log/slog"

	"github.com/plumbkit/plumb/internal/paths"
	"github.com/plumbkit/plumb/internal/render"
	"github.com/plumbkit/plumb/internal/tui"
)

// hijackUsing renders the recovered target of a hijack for display: a contracted
// path, or a plain label when plumb fell back to the OS-native default.
func hijackUsing(h paths.Hijack) string {
	if h.To == "" {
		return "OS-native default"
	}
	return render.ContractPath(h.To)
}

// logRecoveredHijacks writes one WARN line per recovered XDG hijack to the
// daemon log, so a daemon that landed on a recovered path leaves a trace.
// A no-op in the common (un-hijacked) case.
func logRecoveredHijacks() {
	for _, h := range paths.RecoveredHijacks() {
		slog.Warn("recovered hijacked XDG base directory",
			"env", h.EnvVar,
			"hijacked_to", h.From,
			"using", hijackUsing(h),
			"hint", "a shell session manager (e.g. tsm) rewrote "+h.EnvVar+" to a temp dir")
	}
}

// printRecoveredHijacks renders any recovered XDG hijacks as a WarnStyle banner
// for interactive commands, followed by a blank line so it stands apart from the
// table that follows. A no-op in the common (un-hijacked) case.
func printRecoveredHijacks() {
	hijacks := paths.RecoveredHijacks()
	if len(hijacks) == 0 {
		return
	}
	for _, h := range hijacks {
		fmt.Println(tui.WarnStyle.Render(fmt.Sprintf(
			"! %s was hijacked to a temp dir by a shell session manager (e.g. tsm) — using %s",
			h.EnvVar, hijackUsing(h))))
	}
	fmt.Println()
}
