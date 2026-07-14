package config

import "testing"

// The [lsp.<lang>] diagnostics knob accepts exactly {"", "auto", "push", "pull"}.
// Empty is treated as auto so user configs stay minimal.
func TestValidate_LSPDiagnostics_AcceptsKnownModes(t *testing.T) {
	for _, mode := range []string{"", "auto", "push", "pull"} {
		cfg := cloneConfig(defaults)
		lspCfg := cfg.LSP["go"]
		lspCfg.Diagnostics = mode
		cfg.LSP["go"] = lspCfg
		if err := validate(cfg); err != nil {
			t.Errorf("diagnostics=%q should be valid: %v", mode, err)
		}
	}
}

func TestValidate_LSPDiagnostics_RejectsUnknownMode(t *testing.T) {
	cfg := cloneConfig(defaults)
	lspCfg := cfg.LSP["go"]
	lspCfg.Diagnostics = "hybrid" // a resolved-mode word, never a valid config value
	cfg.LSP["go"] = lspCfg
	if err := validate(cfg); err == nil {
		t.Fatal("diagnostics=hybrid must be rejected (config enum is auto|push|pull)")
	}
}

// DiagnosticsModes is the registry enum source of truth: exactly auto, push,
// pull (empty is the implicit auto and is not an explicit choice).
func TestDiagnosticsModes_Values(t *testing.T) {
	want := []string{"auto", "push", "pull"}
	if len(DiagnosticsModes) != len(want) {
		t.Fatalf("DiagnosticsModes = %v, want %v", DiagnosticsModes, want)
	}
	for i, v := range want {
		if DiagnosticsModes[i] != v {
			t.Errorf("DiagnosticsModes[%d] = %q, want %q", i, DiagnosticsModes[i], v)
		}
	}
}
