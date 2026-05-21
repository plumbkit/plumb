package config

import (
	"testing"
	"time"
)

func TestDefaults_LSPQueryTimeout(t *testing.T) {
	if got := defaults.LSPQuery.Timeout.Duration; got != 30*time.Second {
		t.Errorf("default lsp_query.timeout = %v, want 30s", got)
	}
}

func TestApplyEnv_LSPQueryTimeout(t *testing.T) {
	t.Setenv("PLUMB_LSP_QUERY_TIMEOUT", "5s")
	cfg := defaults
	applyEnv(&cfg)
	if got := cfg.LSPQuery.Timeout.Duration; got != 5*time.Second {
		t.Errorf("PLUMB_LSP_QUERY_TIMEOUT=5s not applied, got %v", got)
	}
}

func TestApplyEnv_LSPQueryTimeout_InvalidIgnored(t *testing.T) {
	t.Setenv("PLUMB_LSP_QUERY_TIMEOUT", "not-a-duration")
	cfg := defaults
	applyEnv(&cfg)
	if got := cfg.LSPQuery.Timeout.Duration; got != 30*time.Second {
		t.Errorf("invalid env should leave default intact, got %v", got)
	}
}

func TestValidate_LSPQueryTimeout_NegativeRejected(t *testing.T) {
	cfg := defaults
	cfg.LSPQuery.Timeout = Duration{-1}
	if err := validate(cfg); err == nil {
		t.Fatal("expected validation error for negative lsp_query.timeout")
	}
}

func TestValidate_LSPQueryTimeout_ZeroAllowed(t *testing.T) {
	cfg := defaults
	cfg.LSPQuery.Timeout = Duration{0}
	if err := validate(cfg); err != nil {
		t.Fatalf("zero lsp_query.timeout should be valid (disables the cap): %v", err)
	}
}
