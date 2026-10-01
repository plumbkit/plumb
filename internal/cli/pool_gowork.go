package cli

import (
	"log/slog"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/tools"
)

// pool_gowork.go — the per-root GOWORK decision for a Go language server (#521).
//
// A git worktree of a Go module usually sits inside the directory tree of the
// main checkout, under a go.work that lists the main checkout's directory for
// that module. gopls started for the worktree resolves that go.work, in which the
// worktree is not a module: workspace/symbol answers from the main checkout (or
// not at all) and the worktree's own files are never type-checked, so a
// post-write diagnostics pass reports "clean" about code that does not build.
//
// The decision is the one the git child, run_task and mutation_test already make
// for the go commands they run (internal/tools/git_gowork.go, #506): GOWORK=off
// exactly when the enclosing go.work lists ANOTHER directory for this module and
// none for this one, and never when GOWORK is already somebody's choice. That
// covers an inherited GOWORK and an [lsp.go] env entry (both are in the env the
// decision reads), the user's go env file and $GOROOT/go.env, and — specific to
// gopls — an `env` table in [lsp.go] initialization_options, which gopls applies
// over its own process environment for every go command it runs.

// goLSPEnv returns the environment for the language server of (language, root),
// built from the base env envFor returns, and the go.work it switched off with
// GOWORK=off ("" when the environment was left alone). Only the Go server is
// considered: GOWORK means nothing to a server that does not run the go command.
func goLSPEnv(language, root string, lspCfg config.LSPConfig, env []string) ([]string, string) {
	workFile := goLSPGoWorkOff(language, root, lspCfg, env)
	if workFile == "" {
		return env, ""
	}
	slog.Info("pool: starting the Go language server with GOWORK=off: the enclosing go.work lists another directory for this module, "+
		"so workspace mode would answer from that copy (set GOWORK in [lsp.go] env to keep workspace mode)",
		"root", root, "go_work", workFile)
	return setEnvVar(env, "GOWORK", "off"), workFile
}

// goLSPGoWorkOff is goLSPEnv's decision alone: the go.work the server of
// (language, root) is started with GOWORK=off against, or "". Pure apart from
// reading go.work and go.mod, so the orientation can ask it about a server that
// has not started (plannedGoWorkOff) without logging a start that never happened.
func goLSPGoWorkOff(language, root string, lspCfg config.LSPConfig, env []string) string {
	if language != "go" || goplsEnvSetsGoWork(lspCfg.InitializationOptions) {
		return ""
	}
	return tools.GoWorkBypass(root, env)
}

// plannedGoWorkOff is the GOWORK decision a server for (root, language) would
// start with now, from the same config and environment startOrReuse would give
// it — for orienting an agent whose workspace has no server yet. "" when the
// language is not enabled for root.
func (p *workspacePool) plannedGoWorkOff(root, language string) string {
	lspCfg, ok := p.cfgForWorkspace(root, language)
	if !ok {
		return ""
	}
	return goLSPGoWorkOff(language, root, lspCfg, envFor(lspCfg))
}

// hasEntry reports whether the pool holds a server for (root, language), in
// any lifecycle state. Resolution-only.
func (p *workspacePool) hasEntry(root, language string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.entries[poolKey{root, language}]
	return ok
}

// goplsEnvSetsGoWork reports whether gopls's `env` setting, passed through
// [lsp.go] initialization_options, names GOWORK. gopls gives that setting
// precedence over its process environment, so it is an explicit choice this
// decision must not contradict — nor claim, in the orientation, to have
// overridden.
func goplsEnvSetsGoWork(opts map[string]any) bool {
	env, ok := opts["env"].(map[string]any)
	if !ok {
		return false
	}
	_, set := env["GOWORK"]
	return set
}

// goWorkOffFor returns the go.work the pooled (root, language) server was
// started with GOWORK=off against, or "" when it runs with the environment it
// inherited or no such entry exists. Resolution-only, like diagModeFor.
func (p *workspacePool) goWorkOffFor(root, language string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[poolKey{root, language}]
	if !ok {
		return ""
	}
	return e.goWorkOff
}
