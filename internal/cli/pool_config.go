package cli

import (
	"log/slog"

	"github.com/plumbkit/plumb/internal/config"
)

func (p *workspacePool) cfgFor(language string) (config.LSPConfig, bool) {
	for _, l := range p.langsSnapshot() {
		if l.name == language {
			return l.cfg, true
		}
	}
	return config.LSPConfig{}, false
}

// cfgForWorkspace resolves the language-server configuration at the same
// project boundary the connection exposes to every other subsystem. The pool is
// daemon-global, but LSP settings are explicitly project-overridable; starting
// an entry from only the daemon's global snapshot would silently ignore knobs
// such as [lsp.<lang>] diagnostics.
func (p *workspacePool) cfgForWorkspace(root, language string) (config.LSPConfig, bool) {
	global, ok := p.cfgFor(language)
	if !ok {
		return config.LSPConfig{}, false
	}
	// Narrow test pools and detection-only fixtures may be constructed without a
	// full config snapshot. Their language entry is already the resolved source
	// of truth; feeding a zero Config into LoadProject would fail validation.
	if p.baseConfig.LogLevel == "" {
		return global, true
	}
	project, err := config.LoadProject(p.baseConfig, root)
	if err != nil {
		slog.Warn("pool: project config invalid; using global LSP config", "root", root, "language", language, "err", err)
		return global, true
	}
	resolved, ok := project.LSP[language]
	if !ok || !lspActive(resolved) {
		return config.LSPConfig{}, false
	}
	return resolved, true
}
