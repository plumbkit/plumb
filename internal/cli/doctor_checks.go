package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/paths"
	"github.com/plumbkit/plumb/internal/quality/golangcilint"
	"github.com/plumbkit/plumb/internal/render"
	"github.com/plumbkit/plumb/internal/stats"
	"github.com/plumbkit/plumb/internal/tools"
)

// checkDaemon verifies the daemon is reachable and its version matches.
func checkDaemon() []checkResult {
	socketPath := daemonSocketPath()
	conn, err := net.DialTimeout("unix", socketPath, time.Second)
	if err != nil {
		return []checkResult{{
			name:   "socket",
			ok:     false,
			detail: "cannot dial " + render.ContractPath(socketPath),
			fix:    "run `plumb serve` or let an MCP client start it automatically",
		}}
	}
	conn.Close()

	results := []checkResult{{
		name:   "socket",
		ok:     true,
		detail: render.ContractPath(socketPath),
	}}

	data, err := os.ReadFile(daemonVersionPath())
	if err != nil {
		results = append(results, checkResult{
			name:   "version",
			ok:     false,
			detail: "version file missing — daemon may be stale",
			fix:    "run `plumb stop` then reconnect to restart with the current binary",
		})
		return results
	}
	running := string(bytes.TrimSpace(data))
	if running == Version || running == "" {
		results = append(results, checkResult{
			name:   "version",
			ok:     true,
			detail: running,
		})
	} else {
		results = append(results, checkResult{
			name:   "version",
			ok:     false,
			detail: fmt.Sprintf("running %s, binary is %s", running, Version),
			fix:    "run `plumb stop` then reconnect to reload the current binary",
		})
	}
	return results
}

// checkMCPClients checks whether plumb is registered with each known MCP client
// and that the binary each one launches still exists and matches the running
// executable.
func checkMCPClients() []checkResult {
	selfPath, _ := os.Executable()
	results := make([]checkResult, 0, len(allSetupClients())+2)
	for _, c := range allSetupClients() {
		results = append(results, checkOneClient(c, selfPath))
	}
	if r, ok := checkLegacyAntigravityConfigs(selfPath); ok {
		results = append(results, r)
	}
	if r, ok := checkClaudeDesktopExtraProfiles(selfPath); ok {
		results = append(results, r)
	}
	if r, ok := checkKimiLeanHint(); ok {
		results = append(results, r)
	}
	return results
}

// checkKimiLeanHint surfaces what `plumb doctor` can usefully say about Kimi
// Code's tool surface beyond "registered": either plumb is advertising its whole
// tool registry and Kimi's own mcp.json could trim it with an enabledTools
// allowlist, or the allowlist that is there is degenerate and filters plumb down
// to nothing. ok is false when the config is absent, does not register plumb, or
// carries a working allowlist — there is nothing to say in those cases.
func checkKimiLeanHint() (checkResult, bool) {
	path, err := KimiCodeConfigPath()
	if err != nil {
		return checkResult{}, false
	}
	return kimiLeanHintAt(path)
}

// kimiLeanHintAt is checkKimiLeanHint's path-injectable body, so a test can
// drive an allowlist that is present, absent, degenerate, or in a config that
// does not register plumb at all.
//
// Only a NON-EMPTY list counts as a working allowlist. Presence of the key is
// not enough: `"enabledTools": []`, null, or a non-list value all mean Kimi
// registers zero plumb tools, and a presence-only check would call that server
// healthy while it is effectively dead — the one failure mode of this feature
// that a user cannot see from the outside.
//
// It reads the file directly rather than through readOrInitClaudeConfig, which
// creates the parent directory for an absent config — a doctor check must never
// write to the filesystem it is inspecting.
func kimiLeanHintAt(cfgPath string) (checkResult, bool) {
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return checkResult{}, false
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		return checkResult{}, false // a malformed config is checkOneClient's business
	}
	servers, ok := cfg["mcpServers"].(map[string]any)
	if !ok {
		return checkResult{}, false
	}
	entry, ok := servers["plumb"].(map[string]any)
	if !ok {
		return checkResult{}, false
	}
	raw, has := entry["enabledTools"]
	if !has {
		return kimiFullSurfaceHint(), true
	}
	if list, isList := raw.([]any); isList && len(list) > 0 {
		return checkResult{}, false
	}
	return kimiDegenerateAllowlistResult(raw), true
}

const kimiToolSurfaceCheck = "Kimi Code (tool surface)"

// kimiFullSurfaceHint is the no-allowlist result, and it is INFORMATIONAL:
// ok=true, warn=false, no fix line. A full registration is a perfectly valid
// default — it is what every other client gets — so flagging it as a warning
// would put a "!" against a healthy machine and inflate doctor's warning count
// for a preference. The suggestion goes in the detail, which prints on a clean
// pass; fix lines only render on attention.
func kimiFullSurfaceHint() checkResult {
	return checkResult{
		name: kimiToolSurfaceCheck,
		ok:   true,
		detail: fmt.Sprintf("no client-side allowlist, so Kimi loads whatever plumb advertises "+
			"(every tool under the default profile) — `plumb setup kimi-code --lean` writes an "+
			"enabledTools allowlist trimming it to the %d-tool lean set", len(tools.LeanToolNames())),
	}
}

// kimiDegenerateAllowlistResult grades an enabledTools key that cannot function
// as an allowlist — empty, null, or not a list at all.
//
// This is a WARNING (ok=true, warn=true, with a fix), not the informational line
// above and not a failure. It is a real misconfiguration, not a preference: the
// server still starts, but Kimi filters every plumb tool out of it, so the whole
// integration is silently inert — the same shape as the golangci-lint check,
// where a capability quietly disappears and only doctor can say why. It stays
// non-fatal because doctor's exit code is reserved for plumb itself being
// broken, and this is a hand-edited client config plumb can rewrite in one
// command.
func kimiDegenerateAllowlistResult(raw any) checkResult {
	return checkResult{
		name: kimiToolSurfaceCheck,
		ok:   true,
		warn: true,
		detail: "enabledTools is " + kimiAllowlistShape(raw) + " — Kimi loads NO plumb tools at all; " +
			"the server connects but nothing it offers is callable",
		fix: fmt.Sprintf("run `plumb setup kimi-code --lean` to write the %d-tool lean allowlist, "+
			"or delete the enabledTools key to restore the full tool surface", len(tools.LeanToolNames())),
	}
}

// kimiAllowlistShape names the degenerate value so the detail line says which
// hand-edit produced it.
func kimiAllowlistShape(raw any) string {
	switch v := raw.(type) {
	case nil:
		return "null"
	case []any:
		return "an empty list"
	default:
		return fmt.Sprintf("not a list (%T)", v)
	}
}

// checkClaudeDesktopExtraProfiles validates the plumb binary registered in any
// heuristically-discovered sibling Claude Desktop profile (see
// claudeDesktopExtraConfigPaths) — the unofficial multi-account convention of
// running Claude Desktop under a second Application Support directory.
// checkOneClient only ever sees the one canonical path Anthropic documents, so a
// stale entry in a sibling profile would otherwise pass unflagged. ok is false
// when no extra profile is found — the result is then omitted rather than shown
// as a spurious pass.
func checkClaudeDesktopExtraProfiles(selfPath string) (checkResult, bool) {
	extras, err := claudeDesktopExtraConfigPaths()
	if err != nil || len(extras) == 0 {
		return checkResult{}, false
	}

	var missing, mismatch []string
	present := 0
	for _, p := range extras {
		bin, ok, err := claudeDesktopCommandExtractor(p)
		if err != nil || !ok {
			continue
		}
		bin = expandRegisteredPath(bin)
		present++
		switch {
		case !binaryExists(bin):
			missing = append(missing, contractConfigPath(p))
		case selfPath != "" && !sameBinary(bin, selfPath):
			mismatch = append(mismatch, contractConfigPath(p))
		}
	}
	if present == 0 {
		return checkResult{}, false
	}
	return claudeDesktopExtraProfilesResult(present, missing, mismatch), true
}

// claudeDesktopExtraProfilesResult shapes the check from the scan tallies: a
// missing binary is a failure, a mismatch-but-present binary a non-fatal
// warning, all-current a clean pass — mirroring legacyAntigravityResult.
func claudeDesktopExtraProfilesResult(present int, missing, mismatch []string) checkResult {
	const name = "Claude Desktop (extra profiles)"
	const fix = "run `plumb setup claude-desktop` to repoint every detected profile"
	switch {
	case len(missing) > 0:
		return checkResult{name: name, ok: false, detail: "registered binary missing in: " + strings.Join(missing, ", "), fix: fix}
	case len(mismatch) > 0:
		return checkResult{name: name, ok: true, warn: true, detail: "stale plumb binary in: " + strings.Join(mismatch, ", "), fix: fix}
	default:
		return checkResult{name: name, ok: true, detail: fmt.Sprintf("%d extra profile(s) current (heuristic — not an Anthropic-documented path)", present)}
	}
}

// checkLegacyAntigravityConfigs validates the plumb binary in the flat
// mcp_config.json files Antigravity reads alongside the standalone mcp/plumb.json
// targets. The per-client checks above see only the standalone files, so a stale
// entry in a legacy file (the path Antigravity may actually launch) would slip
// past unflagged. ok is false when no legacy file registers plumb — the result is
// then omitted rather than shown as a spurious pass.
func checkLegacyAntigravityConfigs(selfPath string) (checkResult, bool) {
	cfgPath, err := AntigravityConfigPath()
	if err != nil {
		return checkResult{}, false
	}
	base := geminiBaseFromStandalone(cfgPath)
	var missing, mismatch []string
	present := 0
	for _, p := range legacyAntigravityConfigPaths(base) {
		bin, ok := readLegacyAntigravityCommand(p)
		if !ok {
			continue
		}
		bin = expandRegisteredPath(bin)
		present++
		switch {
		case !binaryExists(bin):
			missing = append(missing, contractConfigPath(p))
		case selfPath != "" && !sameBinary(bin, selfPath):
			mismatch = append(mismatch, contractConfigPath(p))
		}
	}
	if present == 0 {
		return checkResult{}, false
	}
	return legacyAntigravityResult(present, missing, mismatch), true
}

// legacyAntigravityResult shapes the check from the scan tallies: a missing binary
// is a failure (Antigravity cannot launch plumb), a mismatch-but-present binary a
// non-fatal warning, all-current a clean pass.
func legacyAntigravityResult(present int, missing, mismatch []string) checkResult {
	const name = "Antigravity (legacy)"
	const fix = "run `plumb setup antigravity` to repoint legacy configs"
	switch {
	case len(missing) > 0:
		return checkResult{name: name, ok: false, detail: "registered binary missing in: " + strings.Join(missing, ", "), fix: fix}
	case len(mismatch) > 0:
		return checkResult{name: name, ok: true, warn: true, detail: "stale plumb binary in: " + strings.Join(mismatch, ", "), fix: fix}
	default:
		return checkResult{name: name, ok: true, detail: fmt.Sprintf("%d legacy config(s) current", present)}
	}
}

// binaryExists reports whether a registered launch binary is present on disk.
func binaryExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// checkOneClient resolves one client's config and validates that the plumb server
// it registers points at an existing binary matching the running executable.
func checkOneClient(c setupTarget, selfPath string) checkResult {
	path, err := c.pathFn()
	if err != nil {
		return checkResult{name: c.name, ok: false, detail: "cannot locate config: " + err.Error()}
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if c.installedFn != nil && c.installedFn() {
			// The client is installed but has never materialised its MCP config
			// (Kimi Code's mcp.json only appears once a server is configured), so
			// an absent file is "not registered", not "not installed".
			return checkResult{
				name:   c.name,
				ok:     false,
				detail: "installed, but plumb is not registered (no config yet)",
				fix:    fmt.Sprintf("run `plumb setup %s`", c.use),
			}
		}
		return checkResult{
			name:   c.name,
			ok:     false,
			detail: "not installed or config not found",
			fix:    fmt.Sprintf("install %s, then run `plumb setup %s`", c.name, c.use),
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return checkResult{name: c.name, ok: false, detail: "cannot read config: " + err.Error()}
	}
	if !strings.Contains(string(data), "plumb") {
		return checkResult{
			name:   c.name,
			ok:     false,
			detail: "config exists but plumb is not registered",
			fix:    fmt.Sprintf("run `plumb setup %s`", c.use),
		}
	}
	res := classifyClientBinary(c, path, selfPath)
	res.name = c.name
	return res
}

// classifyClientBinary compares the binary a client launches for plumb against the
// running executable: a missing registered binary is a failure, a mismatch with an
// existing binary a non-fatal warning, an exact match a clean pass. When the launch
// command can't be extracted it falls back to a plain "registered" pass.
func classifyClientBinary(c setupTarget, cfgPath, selfPath string) checkResult {
	detail := contractConfigPath(cfgPath)
	if c.extractFn == nil {
		return checkResult{ok: true, detail: detail}
	}
	regPath, registered, err := c.extractFn(cfgPath)
	if err != nil || !registered {
		return checkResult{ok: true, detail: detail}
	}
	regPath = expandRegisteredPath(regPath)
	if _, err := os.Stat(regPath); err != nil {
		return checkResult{
			ok:     false,
			detail: detail + "\nregistered binary missing: " + render.ContractPath(regPath),
			fix:    fmt.Sprintf("run `plumb setup %s` to repoint at the current binary", c.use),
		}
	}
	if selfPath != "" && !sameBinary(regPath, selfPath) {
		return checkResult{
			ok:     true,
			warn:   true,
			detail: detail + "\nregistered: " + render.ContractPath(regPath) + "\ncurrent:    " + render.ContractPath(selfPath),
			fix:    fmt.Sprintf("run `plumb setup %s` to repoint at the current binary", c.use),
		}
	}
	return checkResult{ok: true, detail: detail}
}

// expandRegisteredPath expands a leading ~ and any $VAR in a registered launch
// path so the doctor doesn't misreport a valid-but-unexpanded path as a missing
// binary. plumb always writes an absolute path (os.Executable), so this only
// matters for a config edited by hand to use ~ or an environment variable.
func expandRegisteredPath(p string) string {
	return paths.ExpandHome(p)
}

// sameBinary reports whether two paths resolve to the same executable, comparing
// after symlink resolution so a symlinked install matches its target.
func sameBinary(a, b string) bool {
	return resolvePath(a) == resolvePath(b)
}

func resolvePath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// checkConfigs verifies global and project config files are parseable.
func checkConfigs(ws string) []checkResult {
	var results []checkResult

	globalPath := config.GlobalConfigPath()
	if _, err := os.Stat(globalPath); os.IsNotExist(err) {
		results = append(results, checkResult{
			name:   "global config",
			ok:     true,
			detail: "not present (using compiled defaults)",
		})
	} else if _, err := config.Load(); err != nil {
		results = append(results, checkResult{
			name:   "global config",
			ok:     false,
			detail: err.Error(),
			fix:    "fix TOML syntax in " + contractConfigPath(globalPath),
		})
	} else {
		results = append(results, checkResult{
			name:   "global config",
			ok:     true,
			detail: contractConfigPath(globalPath),
		})
	}

	if ws == "" {
		return results
	}
	projectPath := config.ProjectConfigPath(ws)
	if _, err := os.Stat(projectPath); os.IsNotExist(err) {
		results = append(results, checkResult{
			name:   "project config",
			ok:     true,
			detail: "not present (inheriting global)",
		})
	} else {
		base, _ := config.Load()
		if _, err := config.LoadProject(base, ws); err != nil {
			results = append(results, checkResult{
				name:   "project config",
				ok:     false,
				detail: err.Error(),
				fix:    "fix TOML syntax in " + contractConfigPath(projectPath),
			})
		} else {
			results = append(results, checkResult{
				name:   "project config",
				ok:     true,
				detail: contractConfigPath(projectPath),
			})
		}
	}
	return results
}

// checkStatsDB verifies the global stats DB is readable.
func checkStatsDB(ws string) []checkResult {
	dbPath := stats.DBPathFor()
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return []checkResult{{
			name:   "stats db",
			ok:     true,
			detail: "not present yet (created on first tool call)",
		}}
	}
	db, err := stats.OpenReadOnly()
	if err != nil {
		return []checkResult{{
			name:   "stats db",
			ok:     false,
			detail: err.Error(),
			fix:    "the DB may be corrupt — remove " + contractConfigPath(dbPath) + " to reset",
		}}
	}
	filter := stats.Filter{}
	if ws != "" {
		filter.Workspace = ws
	}
	total := db.TotalCalls(filter)
	db.Close()
	return []checkResult{{
		name:   "stats db",
		ok:     true,
		detail: fmt.Sprintf("%s  (%d calls recorded)", contractConfigPath(dbPath), total),
	}}
}

// checkDevTools reports the external developer tools plumb itself shells out
// to. Only golangci-lint qualifies today: the post-write [quality] analyser
// runs it on every Go write, and the repo's pre-commit hook depends on it.
//
// It exists because its absence used to be invisible. The analyser skips
// silently when the binary cannot be resolved, so on a machine where
// golangci-lint was installed in ~/go/bin but the daemon's PATH lacked that
// directory, the quality findings simply never appeared and nothing — not
// doctor, not the log — said why.
func checkDevTools() []checkResult {
	path, found := golangcilint.LookBinary()
	return []checkResult{golangciLintResult(path, found)}
}

// golangciLintResult is the pure decision half of checkDevTools, so the shape
// of the report is testable without depending on what the host has installed.
//
// A missing linter is a WARNING, never a failure: plumb works fine without it
// (writes still succeed, findings are simply absent), and doctor's exit code is
// reserved for things that are actually broken.
func golangciLintResult(path string, found bool) checkResult {
	if !found {
		return checkResult{
			name:   "golangci-lint",
			ok:     true,
			warn:   true,
			detail: "not found on PATH or in the Go tool bin dir — post-write [quality] Go findings are disabled",
			fix:    "install golangci-lint (golangci-lint.run), or put its directory on the PATH the daemon inherits",
		}
	}
	return checkResult{
		name:   "golangci-lint",
		ok:     true,
		detail: render.ContractPath(path),
	}
}

// checkRastro reports whether the Rastro integration is enabled and, if so,
// whether its executable resolves on PATH. It resolves the effective config and
// delegates the decision to rastroResults, which is pure and therefore testable
// without touching the user's real global config.
func checkRastro(ws string) []checkResult {
	cfg, err := config.Load()
	if err != nil {
		return rastroConfigErr(err)
	}
	// A project config that fails to load is deliberately ignored: the global
	// config is a valid fallback, and checkConfigs reports the project fault.
	if ws != "" {
		if c, err := config.LoadProject(cfg, ws); err == nil {
			cfg = c
		}
	}
	return rastroResults(cfg)
}

// rastroConfigErr reports an unloadable config as a WARNING, not a failure: the
// "Configuration" section already fails the run for an unloadable global config,
// and surfacing the same fault twice would double-count the exit code. Returning
// nil instead would make the whole Integrations section vanish with no
// explanation — the opposite of what doctor is for.
func rastroConfigErr(err error) []checkResult {
	return []checkResult{{
		name:   "rastro",
		ok:     true,
		warn:   true,
		detail: "cannot evaluate: " + err.Error(),
		fix:    "see the Configuration section above",
	}}
}

// rastroResults is the pure decision half of checkRastro.
func rastroResults(cfg config.Config) []checkResult {
	if !cfg.Rastro.Enabled {
		return []checkResult{{
			name:   "rastro",
			ok:     true,
			detail: "disabled in config",
		}}
	}

	bin := cfg.Rastro.Path
	if bin == "" {
		bin = "rastro" // defensive: defaults set this, but a config may blank it
	}

	path, err := exec.LookPath(bin)
	if err != nil {
		return []checkResult{{
			name:   "rastro",
			ok:     false,
			detail: fmt.Sprintf("executable %q not found", bin),
			fix:    "install rastro or update rastro.path in settings",
		}}
	}

	return []checkResult{{
		name:   "rastro",
		ok:     true,
		detail: render.ContractPath(path),
	}}
}
