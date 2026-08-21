package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// clipboardCopiedMsg is the status shown after a copy we can prove landed.
// Declared once so the renderer and its tests agree on the exact string.
const clipboardCopiedMsg = "Copied to the clipboard"

// clipboardKind distinguishes the two ways the TUI can put text on the
// clipboard: shelling out to a platform helper, or writing an OSC 52 escape
// sequence and hoping the terminal honours it.
type clipboardKind int

const (
	clipExec clipboardKind = iota
	clipOSC52
)

// clipboardMethod is how a copy will be attempted on this host. name/path/args
// are set only for clipExec; installHint names the package that would give us a
// real helper, and is empty when installing something would not help.
type clipboardMethod struct {
	kind        clipboardKind
	name        string
	path        string
	args        []string
	installHint string
}

// clipboardStatus is the transient line shown after a copy attempt. verified
// means we have positive evidence the text landed — a helper that exited 0. An
// OSC 52 write can never set it: the escape sequence is write-only, and a
// terminal that silently discards it is indistinguishable from one that
// honoured it. The TUI used to report success unconditionally, which on a
// Wayland desktop meant "Copied to the clipboard" over a clipboard that was
// never touched.
type clipboardStatus struct {
	text     string
	verified bool
}

type clipboardResultMsg struct{ status clipboardStatus }

type copyStatusResetMsg struct{ id int }

// selectClipboardMethod picks the copy helper for this host. goos, getenv and
// look are injected so the whole table is testable without a display server.
//
// Wayland is detected by WAYLAND_DISPLAY rather than XDG_SESSION_TYPE. The
// session type is set by the login manager, so it reads "tty" or is absent
// whenever the compositor is started from a getty, and it is a label rather
// than an address: wl-copy needs the socket that WAYLAND_DISPLAY names.
// DISPLAY is used the same way for X11.
//
// Every exec arm is look-guarded. The previous implementation fell back to
// xsel unconditionally, so on a box with neither helper it ran a command that
// does not exist and discarded the error.
func selectClipboardMethod(goos string, getenv func(string) string, look func(string) (string, error)) clipboardMethod {
	lookup := func(name string, args ...string) (clipboardMethod, bool) {
		path, err := look(name)
		if err != nil {
			return clipboardMethod{}, false
		}
		return clipboardMethod{kind: clipExec, name: name, path: path, args: args}, true
	}

	switch goos {
	case "darwin":
		if m, ok := lookup("pbcopy"); ok {
			return m
		}
		// pbcopy ships with macOS, so there is nothing to install — but an
		// absent one still means a broken PATH the user can repair, which is
		// why this carries a hint at all.
		return clipboardMethod{kind: clipOSC52, installHint: "put pbcopy back on PATH (it ships with macOS, at /usr/bin/pbcopy)"}
	case "windows":
		// clip.exe writes through the console codepage and mangles anything
		// outside it. Windows Terminal supports OSC 52, so the escape sequence
		// is both simpler and more correct until someone can test the native
		// path on real hardware.
		return clipboardMethod{kind: clipOSC52}
	}

	wayland := getenv("WAYLAND_DISPLAY") != ""
	x11 := getenv("DISPLAY") != ""

	if wayland {
		if m, ok := lookup("wl-copy"); ok {
			return m
		}
	}
	// Falling through to the X11 helpers under Wayland is deliberate: XWayland
	// bridges the X11 CLIPBOARD selection into the compositor's clipboard, so a
	// paste into a native Wayland window still works. It is gated on DISPLAY
	// because a compositor started without XWayland leaves it unset, and xclip
	// there dies with "Can't open display" — the blind failure this replaces.
	if x11 {
		if m, ok := lookup("xclip", "-selection", "clipboard"); ok {
			return m
		}
		if m, ok := lookup("xsel", "--clipboard", "--input"); ok {
			return m
		}
	}

	switch {
	case wayland:
		// Name the native package even when DISPLAY is set: a Wayland user
		// should not be talked into the compatibility path.
		return clipboardMethod{kind: clipOSC52, installHint: "install wl-clipboard"}
	case x11:
		return clipboardMethod{kind: clipOSC52, installHint: "install xclip"}
	default:
		// No display server at all — an SSH session or a bare TTY. OSC 52 is
		// the right answer here rather than a shortfall, so there is no hint:
		// installing xclip on a headless box would change nothing.
		return clipboardMethod{kind: clipOSC52}
	}
}

// ClipboardTool reports the copy helper the TUI would actually shell out to on
// this host, so `plumb doctor` and the TUI can never disagree about it. name
// and path are empty when the copy falls back to OSC 52; hint names what to
// install when installing something would help.
func ClipboardTool() (name, path, hint string) {
	m := selectClipboardMethod(runtime.GOOS, os.Getenv, exec.LookPath)
	return m.name, m.path, m.installHint
}

func copyToClipboard(ij, ot string) tea.Cmd {
	return copyTextToClipboard(formatCallDetailForClipboard(ij, ot))
}

func formatCallDetailForClipboard(ij, ot string) string {
	var buf strings.Builder
	if ij != "" {
		buf.WriteString("=== Args ===\n")
		var pb bytes.Buffer
		if err := json.Indent(&pb, []byte(ij), "", "  "); err == nil {
			buf.WriteString(pb.String())
		} else {
			buf.WriteString(ij)
		}
		buf.WriteString("\n")
	}
	if ot != "" {
		buf.WriteString("=== Output ===\n")
		buf.WriteString(ot)
		buf.WriteString("\n")
	}
	return buf.String()
}

// copyTextToClipboard attempts the copy and always reports what happened as a
// clipboardResultMsg, so the status line describes the outcome instead of the
// key press.
//
// An empty payload is refused here rather than at the call sites. Every helper
// accepts empty stdin and exits 0, so running one would REPLACE the user's
// clipboard with nothing and then truthfully report a verified copy — the
// worst of both. It is refused at this choke point because the two call sites
// used to guard it differently: the log detail checked its text was non-empty
// and the call-detail popup did not, and currentDetail has four exits that
// return empty strings (no sessions, no stats DB, no stored detail for the
// row, and rows written before input_json/output_text existed).
func copyTextToClipboard(txt string) tea.Cmd {
	if txt == "" {
		return func() tea.Msg {
			return clipboardResultMsg{status: clipboardStatus{text: "Nothing to copy"}}
		}
	}
	// Selection runs here rather than inside the returned closure because the
	// OSC 52 leg has to hand bubbletea its own command; it costs two getenvs
	// and at most three PATH stats.
	method := selectClipboardMethod(runtime.GOOS, os.Getenv, exec.LookPath)
	if method.kind == clipOSC52 {
		status := clipboardStatus{text: "Sent via OSC 52 — the terminal may ignore it"}
		if method.installHint != "" {
			status.text = "Sent via OSC 52 (unverified) — " + method.installHint
		}
		// Batched rather than chained: tea.SetClipboard's command yields
		// bubbletea's own internal message, which the runtime consumes, so the
		// status has to travel as a message of its own.
		return tea.Batch(tea.SetClipboard(txt), func() tea.Msg {
			return clipboardResultMsg{status: status}
		})
	}
	return func() tea.Msg {
		return clipboardResultMsg{status: runClipboardExec(method, txt)}
	}
}

// clipboardExecTimeout bounds a wedged helper. The healthy case returns in
// milliseconds — wl-copy, xclip and xsel all drain stdin and fork before the
// parent exits — so this only fires when the helper cannot make progress at
// all, e.g. xclip blocking in XOpenDisplay against an unresponsive X server.
// Without it that goroutine never returns, no result message is ever sent, and
// the status line stays blank forever: silence, which is the failure mode this
// whole path exists to remove.
const clipboardExecTimeout = 5 * time.Second

// runClipboardExec pipes txt into the helper and reports whether it exited
// cleanly.
func runClipboardExec(m clipboardMethod, txt string) clipboardStatus {
	return runClipboardExecWithTimeout(m, txt, clipboardExecTimeout)
}

// runClipboardExecWithTimeout is the timeout-injectable half, so the wedged
// case is testable without a test that sits for the real budget.
func runClipboardExecWithTimeout(m clipboardMethod, txt string, timeout time.Duration) clipboardStatus {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, m.path, m.args...) //nolint:gosec // G204: the path comes from exec.LookPath over a closed set of literal helper names, never from user input
	cmd.Stdin = strings.NewReader(txt)
	// Stdout and Stderr are left nil on purpose. wl-copy, xclip and xsel all
	// fork and leave a child resident to serve the selection; assigning a
	// non-*os.File writer makes os/exec create a pipe and wait for every writer
	// to close it — including that resident child — so Run would block for as
	// long as the clipboard offer stands. A nil writer gets /dev/null and no
	// pipe, so Wait returns as soon as the parent does. The exit status and the
	// helper's name are enough to say something actionable.
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return clipboardStatus{text: fmt.Sprintf("Copy failed: %s did not respond within %s", m.name, timeout)}
		}
		return clipboardStatus{text: "Copy failed: " + m.name + ": " + err.Error()}
	}
	return clipboardStatus{text: clipboardCopiedMsg, verified: true}
}
