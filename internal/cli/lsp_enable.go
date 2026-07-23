package cli

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/config"
)

var enableLSPCmd = &cobra.Command{
	Use:   "enable-lsp <language>",
	Short: "Enable a language server in the running daemon without a restart",
	Long: `Turn on a configured language ([lsp.<language>]) in the running plumb daemon,
without restarting it.

  plumb enable-lsp rust     — enable the Rust server now

Enabling a language normally requires a daemon restart. This command flips the
language on live: the daemon adds it to its effective language set, and its
server attaches lazily on the next file of that language a session opens (no
process is spawned eagerly). It is honest about failure — an unknown language,
or a server binary that is not installed (it names the binary to install).

The change is daemon-lifetime only, like ` + "`plumb log-level`" + `. To make it
permanent, set enabled = true under [lsp.<language>] in the config file
(installing the server is usually enough — an installed, enabled language
activates automatically at startup).`,
	Args: cobra.ExactArgs(1),
	RunE: runEnableLSP,
}

func runEnableLSP(_ *cobra.Command, args []string) error {
	lang := strings.TrimSpace(args[0])
	if lang == "" {
		return fmt.Errorf("no language given")
	}
	resp, err := dialDaemonCtrl("enable-lsp " + lang)
	if err != nil {
		return err
	}
	if msg, ok := strings.CutPrefix(resp, "error:"); ok {
		return fmt.Errorf("%s", strings.TrimSpace(msg))
	}
	fmt.Println(resp)
	return nil
}

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

// langsSnapshot returns the current effective-language slice under the langs
// read lock. enableLanguage replaces the slice wholesale (copy-on-write) and
// never mutates a published backing array, so a caller may range the returned
// value after releasing the lock without risk of a torn read. This is the one
// accessor every langs reader (Detect helpers, cfgFor, hasActiveLanguage) goes
// through so a live `enable-lsp` cannot race the hot detection/routing paths.
func (p *workspacePool) langsSnapshot() []langConfig {
	p.langsMu.RLock()
	defer p.langsMu.RUnlock()
	return p.langs
}

// enableLanguage live-enables the language server for name in the running
// daemon, WITHOUT a restart — the restart-tier operation this whole feature
// exists to eliminate. On success the language joins the pool's effective set,
// so the next matching file routes to it and lazily starts its server (the
// multi-LSP on-demand attach); enableLanguage never eagerly spawns a process.
//
// It returns already=true (nil error) when the language is already active — a
// no-op the caller reports rather than failing. Errors are honest and
// actionable: an unknown language (no [lsp.<name>] block), or an enabled
// language whose server binary is not on PATH (named, so the user knows what to
// install).
//
// Concurrency: p.mu is held for the whole read-modify-write. It guards the only
// other reader of baseConfig (cfgForWorkspace, under startOrReuse's p.mu) and
// serialises concurrent enables. The langs slice is published under langsMu via
// copy-on-write so lock-free hot-path readers are never disturbed. Existing pool
// entries (running servers), pinned workspaces, and read-tracking are untouched:
// this only widens the set of languages a NEW acquire may start.
func (p *workspacePool) enableLanguage(name string) (already bool, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, l := range p.langs {
		if l.name == name {
			return true, nil
		}
	}

	cfg, ok := p.baseConfig.LSP[name]
	if !ok {
		return false, fmt.Errorf("unknown language %q: no [lsp.%s] block in the resolved config", name, name)
	}
	if cfg.Command == "" {
		return false, fmt.Errorf("language %q has no server command configured (set [lsp.%s].command)", name, name)
	}
	if !lspInstalled(cfg.Command) {
		return false, fmt.Errorf("language server %q for %q is not installed — install it (put %q on PATH), then run `plumb enable-lsp %s` again", cfg.Command, name, cfg.Command, name)
	}
	cfg.Enabled = true

	// Copy-on-write the baseConfig LSP map. The daemon's original map may still be
	// shared with config.Store readers (Current()), which the store contract
	// forbids mutating in place; replacing the pool's own map pointer flips
	// enablement for cfgForWorkspace (LoadProject over baseConfig) without touching
	// the store's map. The store's LSP block stays restart-tier by design — this
	// live change is deliberately pool-local (see the CLI command's help).
	newLSP := make(map[string]config.LSPConfig, len(p.baseConfig.LSP))
	for k, v := range p.baseConfig.LSP {
		newLSP[k] = v
	}
	newLSP[name] = cfg
	p.baseConfig.LSP = newLSP

	// Copy-on-write the effective language set: build a fresh sorted slice and
	// swap it in under langsMu, so a reader ranging a previously-published slice
	// is unaffected.
	next := make([]langConfig, len(p.langs), len(p.langs)+1)
	copy(next, p.langs)
	next = append(next, langConfig{name: name, cfg: cfg})
	sortLangs(next)
	p.langsMu.Lock()
	p.langs = next
	p.langsMu.Unlock()

	return false, nil
}

// enableLanguageCtrl is the control-socket adapter over enableLanguage: it maps
// the (already, err) result to the single human-readable line the daemon writes
// back to `plumb enable-lsp`. Wired into ctrlHandlers.enableLSP in daemon.go.
func (p *workspacePool) enableLanguageCtrl(lang string) (string, error) {
	if lang == "" {
		return "", fmt.Errorf("no language given")
	}
	already, err := p.enableLanguage(lang)
	if err != nil {
		return "", err
	}
	if already {
		return fmt.Sprintf("%s is already enabled", lang), nil
	}
	return fmt.Sprintf("enabled %s — its server attaches on the next matching file", lang), nil
}
