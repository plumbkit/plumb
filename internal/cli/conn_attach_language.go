package cli

// conn_attach_language.go — pure language-to-adapter mapping and best-effort
// language detection for display. Split from conn_attach.go by responsibility:
// nothing here touches session state.

import (
	"path/filepath"
	"sort"

	"github.com/plumbkit/plumb/internal/config"
)

func adapterForLanguage(language string) string {
	switch language {
	case "go":
		return "gopls"
	case "python":
		return "pyright"
	case "java":
		return "jdtls"
	case "rust":
		return "rust-analyzer"
	case "swift":
		return "sourcekit-lsp"
	case "zig":
		return "zls"
	case "typescript", "javascript":
		return "typescript-language-server"
	case "kotlin":
		return "kotlin-language-server"
	case "html":
		return "vscode-html-language-server"
	default:
		return ""
	}
}

func detectAnyLanguageAt(dir string, cfg config.Config) string {
	langs := make([]string, 0, len(cfg.LSP))
	for name, lspCfg := range cfg.LSP {
		if len(lspCfg.RootMarkers) > 0 {
			langs = append(langs, name)
		}
	}
	sort.Slice(langs, func(i, j int) bool {
		if langs[i] == "go" {
			return true
		}
		if langs[j] == "go" {
			return false
		}
		return langs[i] < langs[j]
	})
	homeInfo := homeFileInfo()
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		// Stop at $HOME, mirroring the pool's Detect/detectLanguageAt walks: a stray
		// marker in the home directory (e.g. a global ~/package.json) must not be
		// reported as the detected language for a workspace beneath it.
		if sameDirAs(d, homeInfo) {
			return ""
		}
		for _, name := range langs {
			for _, marker := range cfg.LSP[name].RootMarkers {
				if markerPresent(d, marker) {
					return name
				}
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
	}
}
