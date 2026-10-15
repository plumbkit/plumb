package tui

import "github.com/plumbkit/plumb/internal/session"

// defaultSessions lists the live sessions of the default namespace: the TUI is
// the user's view, and a namespaced end-to-end run (PLAN-520) is not shown.
func defaultSessions() ([]session.Info, error) {
	all, err := session.List()
	return session.InNamespace(all, ""), err
}
