package cli

import (
	"context"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
)

func TestLspInstalled(t *testing.T) {
	// `go` is guaranteed on PATH while running the test suite.
	if !lspInstalled("go") {
		t.Error("expected 'go' to be installed on PATH in the test environment")
	}
	if lspInstalled("plumb-definitely-not-a-real-binary-xyz") {
		t.Error("a bogus command must not be reported as installed")
	}
	if lspInstalled("") {
		t.Error("an empty command must not be reported as installed")
	}
}

func TestLspActive(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.LSPConfig
		want bool
	}{
		{"enabled+installed", config.LSPConfig{Command: "go", Enabled: true}, true},
		{"enabled+missing", config.LSPConfig{Command: "plumb-no-such-binary-xyz", Enabled: true}, false},
		{"disabled+installed", config.LSPConfig{Command: "go", Enabled: false}, false},
		{"enabled+nocommand", config.LSPConfig{Command: "", Enabled: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := lspActive(tc.cfg); got != tc.want {
				t.Errorf("lspActive=%v want %v", got, tc.want)
			}
		})
	}
}

func TestHasActiveLanguage(t *testing.T) {
	p := detectTestPool() // go + python
	if !p.hasActiveLanguage("go") {
		t.Error("go should be reported active (in the pool's langs)")
	}
	if p.hasActiveLanguage("swift") {
		t.Error("swift is not in the pool, so it must not be active")
	}
}

// TestNewWorkspacePool_InstallGatesLangs verifies the automatic-mode contract:
// an enabled language whose server is not installed is excluded from the pool's
// active set, so its root markers never enter workspace detection; a language
// explicitly disabled is excluded even though its server is installed.
func TestNewWorkspacePool_InstallGatesLangs(t *testing.T) {
	cfg := config.Defaults()
	cfg.LSP = map[string]config.LSPConfig{
		"go":    {Command: "go", RootMarkers: []string{"go.mod"}, Enabled: true},                           // installed + enabled
		"ghost": {Command: "plumb-no-such-binary-xyz", RootMarkers: []string{"ghost.toml"}, Enabled: true}, // enabled but missing
		"off":   {Command: "go", RootMarkers: []string{"off.toml"}, Enabled: false},                        // installed but excluded
	}
	p := newWorkspacePool(context.Background(), cfg)
	active := map[string]bool{}
	for _, l := range p.langs {
		active[l.name] = true
	}
	if !active["go"] {
		t.Error("installed + enabled 'go' should be active")
	}
	if active["ghost"] {
		t.Error("enabled-but-uninstalled 'ghost' must be excluded so its markers don't pollute detection")
	}
	if active["off"] {
		t.Error("explicitly disabled 'off' must be excluded even though its command is installed")
	}
}
