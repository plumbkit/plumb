package session

// health_sanitize.go — sanitising HealthMessage before it is persisted.
//
// HealthMessage embeds client-supplied text (a boundary-violation message
// quotes the offending path), and ESC is a legal byte in a POSIX path. A
// prior attempt (issue #358) stripped escapes inside one TUI renderer only,
// so the raw text still reached the terminal through a second renderer that
// read the same field without going through the first one's defence.
// writeSessionFileAtomic (session.go) applies sanitizeHealthMessage once, at
// the single choke point every writer of HealthMessage passes through
// (Register directly, Patch — and so markBoundaryViolation — via its own
// call), so every reader gets already-clean text instead of each needing to
// defend itself.

import "strings"

// sanitizeHealthMessage strips control characters from a HealthMessage before
// it is persisted: C0 controls (including ESC and DEL) and C1 controls are
// dropped, newline/carriage-return/tab collapse to a single space, and the
// result is trimmed. Every reader — the dashboard alert, the session detail
// pane, and the web API's /sessions endpoint — then renders the same
// already-clean text, rather than each needing to defend itself.
func sanitizeHealthMessage(s string) string {
	if s == "" {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteRune(' ')
		case r < 0x20 || r == 0x7f: // C0 controls + DEL
		case r >= 0x80 && r <= 0x9f: // C1 controls
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}
