package cli

import (
	"fmt"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/quality"
	"github.com/plumbkit/plumb/internal/render"
	"github.com/plumbkit/plumb/internal/tui"
)

// checkDevTools reports the external developer tools plumb itself shells out
// to: every analyser the [quality] block configures, and the clipboard helper
// the TUI's "c" key pipes into.
//
// It exists because their absence used to be invisible. The analyser skips
// silently when the binary cannot be resolved, so on a machine where
// golangci-lint was installed in ~/go/bin but the daemon's PATH lacked that
// directory, the quality findings simply never appeared and nothing — not
// doctor, not the log — said why. The clipboard was worse: it reported success
// either way.
//
// It reports the CONFIGURED list rather than a hardcoded golangci-lint row,
// because the silent-skip has a second shape the hardcoded row could not see: an
// entry plumb does not recognise at all (a typo, or the absolute path of a tool
// that would work under its name) vanished inside buildAnalysers with nothing
// reported anywhere.
func checkDevTools() []checkResult {
	clipName, clipPath, clipHint := tui.ClipboardTool()

	cfg, err := config.Load()
	if err != nil {
		// The "Configuration" section already fails the run for an unloadable
		// global config; reporting the same fault twice would double-count the
		// exit code. Keep the clipboard row so the section does not vanish.
		return []checkResult{clipboardToolResult(clipName, clipPath, clipHint)}
	}

	// [quality] is global-only by construction (see config.QualityConfig), so
	// unlike checkRastro this deliberately does NOT merge the project config: a
	// doctor row built from a value the runner never reads would be exactly the
	// misreport this change exists to remove.
	states := quality.ClassifyEntries(cfg.Quality.Analysers, cfg.Quality.Bin)
	out := make([]checkResult, 0, len(states)+1)
	for _, st := range states {
		out = append(out, analyserResult(st, cfg.Quality.Enabled))
	}
	return append(out, clipboardToolResult(clipName, clipPath, clipHint))
}

// analyserResult is the pure decision half of an analyser row, so the shape of
// the report is testable without depending on what the host has installed.
//
// No analyser problem is ever a FAILURE: plumb works fine without one (writes
// still succeed, findings are simply absent), and doctor's exit code is reserved
// for things that are actually broken. Nor is one a warning while [quality] is
// switched off — the whole feature is opt-in and off by default, so warning
// about an unusable entry in a block that is not running would put a yellow row
// in front of every user who has never touched this config.
func analyserResult(st quality.EntryState, enabled bool) checkResult {
	name := "analyser " + st.Entry
	if st.Status == quality.EntryOK {
		detail := render.ContractPath(st.Binary)
		if lang := st.Language(); lang != "" {
			detail = lang + "  " + detail
		}
		return checkResult{name: name, ok: true, detail: detail}
	}
	return checkResult{
		name:   name,
		ok:     true,
		warn:   enabled,
		detail: render.ContractHome(st.Reason) + " — it will not run",
		fix:    analyserFix(st),
	}
}

// analyserFix is the remedy for a non-OK entry, and it differs per status
// because pointing all three at "install it" would send someone to install a
// tool plumb still would not run.
func analyserFix(st quality.EntryState) string {
	switch st.Status {
	case quality.EntryBinaryMissing:
		return fmt.Sprintf("install %s, set [quality.bin] %s = \"<path>\", "+
			"or put its directory on the PATH the daemon inherits", st.Entry, st.Entry)
	case quality.EntryUnsupported, quality.EntryUnknown:
		return "remove it from [quality] analysers; `plumb doctor` lists the names plumb runs"
	default:
		return ""
	}
}

// clipboardToolResult is the pure decision half of the clipboard row, so the
// report's shape is testable without depending on what the host has installed
// or which session type the test happens to run under. The inputs come from
// tui.ClipboardTool, so doctor names the helper the TUI would actually use
// rather than resolving its own — a check that disagreed with the thing it
// checks would be worse than none.
//
// The row never fails doctor's exit code, for analyserResult's reason: the
// TUI still copies via OSC 52 without a helper. It warns only when installing
// something would help — on a headless box (no DISPLAY, no WAYLAND_DISPLAY)
// there is nothing to install, and a warning on every SSH session is how a
// warning gets ignored.
func clipboardToolResult(name, path, hint string) checkResult {
	if name != "" {
		return checkResult{
			name:   "clipboard",
			ok:     true,
			detail: name + "  " + render.ContractPath(path),
		}
	}
	if hint == "" {
		return checkResult{
			name:   "clipboard",
			ok:     true,
			detail: "no local helper applies (no DISPLAY/WAYLAND_DISPLAY) — the TUI's `c` copy uses OSC 52 via the terminal",
		}
	}
	return checkResult{
		name:   "clipboard",
		ok:     true,
		warn:   true,
		detail: "no clipboard helper found — the TUI's `c` copy falls back to OSC 52, which many terminals ignore",
		fix:    hint,
	}
}
