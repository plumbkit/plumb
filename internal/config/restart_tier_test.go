package config

import (
	"slices"
	"testing"
	"time"
)

// TestRestartTier_MatchesTheComparison is the one-source-of-truth check
// (PLAN-539): the registry's ReloadRestart keys are exactly the fields
// RestartSensitiveEqual compares — less log_file, which is registered
// restart-tier but has no reader at all (project_classification.go), so no
// restart applies it either and warning about it would mislead — each of them
// flips the comparison, and an
// [lsp.<lang>] change — now applied live, by restarting that language's servers
// — does not.
func TestRestartTier_MatchesTheComparison(t *testing.T) {
	var restart []string
	for _, f := range Registry() {
		if f.ReloadTier == ReloadRestart {
			restart = append(restart, f.Key)
		}
	}
	slices.Sort(restart)
	want := []string{"cache.max_size", "cache.ttl", "daemon.account_authority", "log_file", "log_format"}
	if !slices.Equal(restart, want) {
		t.Fatalf("restart-tier keys = %v, want %v", restart, want)
	}
	inert := cloneConfig(Defaults())
	inert.LogFile = "/tmp/elsewhere.log"
	if !RestartSensitiveEqual(Defaults(), inert) {
		t.Error("log_file has no reader; a change to it must not warn that a restart is needed")
	}

	base := Defaults()
	for key, mutate := range map[string]func(*Config){
		"cache.max_size":           func(c *Config) { c.Cache.MaxSize++ },
		"cache.ttl":                func(c *Config) { c.Cache.TTL = Duration{Duration: c.Cache.TTL.Duration + time.Minute} },
		"daemon.account_authority": func(c *Config) { c.Daemon.AccountAuthority = !c.Daemon.AccountAuthority },
		"log_format":               func(c *Config) { c.LogFormat = "json-" + c.LogFormat },
	} {
		changed := cloneConfig(base)
		mutate(&changed)
		if RestartSensitiveEqual(base, changed) {
			t.Errorf("a change to %s is not restart-sensitive, but the registry says it needs a restart", key)
		}
	}

	lsp := cloneConfig(base)
	if lsp.LSP == nil {
		lsp.LSP = map[string]LSPConfig{}
	}
	g := lsp.LSP["go"]
	g.Command = "some-other-gopls"
	lsp.LSP["go"] = g
	if !RestartSensitiveEqual(base, lsp) {
		t.Error("an [lsp.go] change still demands a daemon restart; it restarts only the Go servers")
	}
	for _, key := range []string{"lsp.go.command", "lsp.go.diagnostics", "lsp.go.idle_timeout", "lsp.go.max_workspaces"} {
		f, ok := Lookup(key)
		if !ok || f.ReloadTier != ReloadLive {
			t.Errorf("%s: registered %v (tier %v), want a live per-language field", key, ok, f.ReloadTier)
		}
	}
}
