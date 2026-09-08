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
