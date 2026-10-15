package tui

import (
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
)

// TestLSPKnobRows_RoundTrip pins the per-language idle timeout and max
// workspaces rows (PLAN-539): every option renders back as itself, and applying
// it writes the typed value into [lsp.<lang>].
func TestLSPKnobRows_RoundTrip(t *testing.T) {
	for _, opt := range lspIdleTimeoutOptions {
		var c config.Config
		applyLSPField(&c, "go", skLSPIdleTimeout, opt)
		if got := idleTimeoutValue(c.LSP["go"].IdleTimeout.Duration); got != opt {
			t.Errorf("idle timeout %q round-trips as %q", opt, got)
		}
	}
	for d, want := range map[time.Duration]string{
		30 * time.Second: "30s", 90 * time.Second: "1m30s", 45 * time.Minute: "45m",
		time.Hour: "1h", 90 * time.Minute: "1h30m", 0: "0",
	} {
		if got := idleTimeoutValue(d); got != want {
			t.Errorf("idleTimeoutValue(%s) = %q, want %q", d, got, want)
		}
	}
	var c config.Config
	applyLSPField(&c, "java", skLSPIdleTimeout, "20m")
	applyLSPField(&c, "java", skLSPMaxWorkspaces, lspCycleValue(skLSPMaxWorkspaces, "3"))
	if j := c.LSP["java"]; j.IdleTimeout.Duration != 20*time.Minute || j.MaxWorkspaces != 3 {
		t.Fatalf("[lsp.java] = %+v, want idle 20m and max 3", j)
	}
	for key, want := range map[settingKey]string{skLSPIdleTimeout: "idle_timeout", skLSPMaxWorkspaces: "max_workspaces"} {
		if got, ok := lspFieldName(key); !ok || got != want {
			t.Errorf("lspFieldName(%v) = %q, %v; want %q", key, got, ok, want)
		}
	}
}
