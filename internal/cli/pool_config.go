package cli

import "github.com/plumbkit/plumb/internal/config"

func (p *workspacePool) cfgFor(language string) (config.LSPConfig, bool) {
	return cfgAmong(p.langsSnapshot(), language)
}

// cfgForWorkspace resolves the language-server configuration at the same
// project boundary the connection exposes to every other subsystem. The pool is
// daemon-global, but LSP settings are explicitly project-overridable; starting
// an entry from only the daemon's global snapshot would silently ignore knobs
// such as [lsp.<lang>] diagnostics.
func (p *workspacePool) cfgForWorkspace(root, language string) (config.LSPConfig, bool) {
	return cfgAmong(p.effectiveLanguages(root), language)
}

// entryConfig is e's effective config, whether its language is enabled, and
// whether the project config governing it fails to parse — all from one
// resolution (resolveLanguages).
func (p *workspacePool) entryConfig(e *poolEntry) (cfg config.LSPConfig, enabled, invalid bool) {
	langs, invalid := p.resolveLanguages(e.root)
	cfg, enabled = cfgAmong(langs, e.language)
	return cfg, enabled, invalid
}
