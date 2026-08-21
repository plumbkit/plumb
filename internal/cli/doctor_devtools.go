package cli

import (
	"github.com/plumbkit/plumb/internal/quality/golangcilint"
	"github.com/plumbkit/plumb/internal/render"
	"github.com/plumbkit/plumb/internal/tui"
)

// checkDevTools reports the external developer tools plumb itself shells out
// to: golangci-lint, which the post-write [quality] analyser runs on every Go
// write, and the clipboard helper the TUI's "c" key pipes into.
//
// It exists because their absence used to be invisible. The analyser skips
// silently when the binary cannot be resolved, so on a machine where
// golangci-lint was installed in ~/go/bin but the daemon's PATH lacked that
// directory, the quality findings simply never appeared and nothing — not
// doctor, not the log — said why. The clipboard was worse: it reported success
// either way.
func checkDevTools() []checkResult {
	path, found := golangcilint.LookBinary()
	clipName, clipPath, clipHint := tui.ClipboardTool()
	return []checkResult{
		golangciLintResult(path, found),
		clipboardToolResult(clipName, clipPath, clipHint),
	}
}

// golangciLintResult is the pure decision half of checkDevTools, so the shape
// of the report is testable without depending on what the host has installed.
//
// A missing linter is a WARNING, never a failure: plumb works fine without it
// (writes still succeed, findings are simply absent), and doctor's exit code is
// reserved for things that are actually broken.
func golangciLintResult(path string, found bool) checkResult {
	if !found {
		return checkResult{
			name:   "golangci-lint",
			ok:     true,
			warn:   true,
			detail: "not found on PATH or in the Go tool bin dir — post-write [quality] Go findings are disabled",
			fix:    "install golangci-lint (golangci-lint.run), or put its directory on the PATH the daemon inherits",
		}
	}
	return checkResult{
		name:   "golangci-lint",
		ok:     true,
		detail: render.ContractPath(path),
	}
}

// clipboardToolResult is the pure decision half of the clipboard row, so the
// report's shape is testable without depending on what the host has installed
// or which session type the test happens to run under. The inputs come from
// tui.ClipboardTool, so doctor names the helper the TUI would actually use
// rather than resolving its own — a check that disagreed with the thing it
// checks would be worse than none.
//
// The row never fails doctor's exit code, for golangciLintResult's reason: the
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
