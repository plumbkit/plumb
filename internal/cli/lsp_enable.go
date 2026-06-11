package cli

import (
	"os/exec"

	"github.com/plumbkit/plumb/internal/config"
)

// lspInstalled reports whether a language server command resolves to an
// executable. It delegates to exec.LookPath, the standard library's
// cross-platform PATH resolver: on Windows it honours PATHEXT (so a bare
// "gopls" matches gopls.exe), and on macOS/Linux it walks PATH. An absolute or
// relative command path is validated directly. An empty command is never
// installed.
func lspInstalled(command string) bool {
	if command == "" {
		return false
	}
	_, err := exec.LookPath(command)
	return err == nil
}

// lspActive reports the effective enablement of a language server: the user's
// intent (LSPConfig.Enabled, which defaults to true) gated on the binary
// actually being present. This is the "automatic mode" — install the server and
// it activates; set enabled = false to exclude a language even when installed;
// an enabled language whose server is absent stays dormant at zero cost rather
// than failing to spawn. Evaluated wherever the set of active languages is
// resolved, so a freshly-installed server is picked up without code changes.
func lspActive(cfg config.LSPConfig) bool {
	return cfg.Enabled && lspInstalled(cfg.Command)
}

// lspActiveStatus is a human-readable explanation of the effective state, for
// `plumb config show` and diagnostics.
func lspActiveStatus(cfg config.LSPConfig) string {
	switch {
	case !cfg.Enabled:
		return "no (disabled in config)"
	case !lspInstalled(cfg.Command):
		return "no (" + cfg.Command + " not installed)"
	default:
		return "yes (installed)"
	}
}
