package tui

import (
	"strings"
	"time"
)

// lspIdleTimeoutOptions and lspMaxWorkspacesOptions are the steps the per-
// language idle timeout and server cap cycle through; "0" disables each.
var (
	lspIdleTimeoutOptions   = []string{"0", "5m", "10m", "20m", "30m", "1h", "2h", "4h"}
	lspMaxWorkspacesOptions = []string{"0", "1", "2", "3", "4", "6", "8"}
)

// idleTimeoutValue renders an idle timeout the way the cycle options spell it.
func idleTimeoutValue(d time.Duration) string {
	if d <= 0 {
		return "0"
	}
	s := d.String() // "20m0s", "1h0m0s", "30s", "1m30s"
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
