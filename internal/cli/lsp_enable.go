package cli

import (
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"sort"
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
		return errors.New("no language given")
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

// languageOverrideErr validates a session_start `language` argument, returning
// nil when it names an ACTIVE language (enabled and installed) and an
// actionable refusal otherwise.
//
// This used to be a silent drop: an override that named an unknown, disabled or
// uninstalled language was discarded and detection's answer kept. The caller
// was told nothing, so an agent on a repo that had misdetected asked for its
// real language, was answered with the wrong one, and had no way to tell the
// override from a language plumb genuinely believed in. Reported from the field
// as "session_start(language=...) — ignored once the workspace is already
// pinned", which was the visible half of this.
//
// The three cases get three remedies because they are three different problems:
// a typo needs the valid keys, a disabled language needs a config edit, and an
// uninstalled server needs an install. lspActiveStatus already draws the second
// two apart for `plumb config show`, so the wording cannot drift from it.
func (s *connSession) languageOverrideErr(name string) error {
	if s.pool == nil {
		return nil // no pool wired (tests / degraded start): nothing to validate against
	}
	if s.pool.hasActiveLanguage(name) {
		return nil
	}
	cfg, known := s.store.Current().LSP[name]
	if !known {
		return fmt.Errorf("session_start: language %q has no [lsp.%s] adapter. Known languages: %s",
			name, name, strings.Join(knownLanguageNames(s.store.Current()), ", "))
	}
	// The pool's effective set and the config can disagree: the set is built at
	// pool construction and widened by enable-lsp, so a language enabled or
	// installed SINCE the daemon resolved it is configured-active but not yet
	// pool-active. Reporting lspActiveStatus here would print the flatly
	// self-contradicting "not active — yes (installed)", and send the caller to
	// edit config that is already correct. The remedy for this case is the live
	// enable that exists precisely for it.
	if lspActive(cfg) {
		return fmt.Errorf("session_start: language %q is enabled and %s is installed, but this daemon has not picked it up "+
			"(its language set was resolved before that became true). Run `plumb enable-lsp %s` to activate it without a restart, then retry",
			name, cfg.Command, name)
	}
	return fmt.Errorf("session_start: language %q is configured but not active — %s. "+
		"Set [lsp.%s] enabled = true and install %s, then retry; "+
		"or omit the language argument to use what detection resolves",
		name, lspActiveStatus(cfg), name, cfg.Command)
}

// knownLanguageNames lists every configured [lsp.<lang>] key, sorted, for the
// remedy half of an unknown-language refusal. Every key is listed, active or
// not: a caller who named a disabled language needs to see that the key exists
// before the enablement advice makes sense.
func knownLanguageNames(cfg config.Config) []string {
	names := make([]string, 0, len(cfg.LSP))
	for name := range cfg.LSP {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
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
	maps.Copy(newLSP, p.baseConfig.LSP)
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
	// Published last, so a connection that observes the new generation is
	// guaranteed to see the widened set when it re-detects. Bumped only on a real
	// widening — the already-enabled no-op above returns before here, so a repeat
	// `enable-lsp go` costs live sessions nothing.
	p.langsGen.Add(1)

	return false, nil
}

// langsGeneration returns the current effective-language-set generation. A live
// connection compares it against the generation it last resolved its primary
// language at; a difference means `enable-lsp` widened the set and the primary
// may now be resolvable. See connSession.refreshPrimaryIfStale.
func (p *workspacePool) langsGeneration() uint64 {
	return p.langsGen.Load()
}

// enableLanguageCtrl is the control-socket adapter over enableLanguage: it maps
// the (already, err) result to the single human-readable line the daemon writes
// back to `plumb enable-lsp`. Wired into ctrlHandlers.enableLSP in daemon.go.
func (p *workspacePool) enableLanguageCtrl(lang string) (string, error) {
	if lang == "" {
		return "", errors.New("no language given")
	}
	already, err := p.enableLanguage(lang)
	if err != nil {
		return "", err
	}
	if already {
		return lang + " is already enabled", nil
	}
	return fmt.Sprintf("enabled %s — its server attaches on the next matching file", lang), nil
}
