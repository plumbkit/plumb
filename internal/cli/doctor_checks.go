package cli

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/paths"
	"github.com/plumbkit/plumb/internal/quality/golangcilint"
	"github.com/plumbkit/plumb/internal/render"
	"github.com/plumbkit/plumb/internal/stats"
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
	if r, ok := checkClaudeDesktopExtraProfiles(selfPath); ok {
		results = append(results, r)
	}
	results = append(results, checkLeanAllowlists()...)
	results = append(results, checkSkillFreshness()...)
	return results
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
// warning, all-current a clean pass.
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
			// (Kimi Code's mcp.json, or DeepSeek Harness's home patch — both only
			// appear once an entry is configured), so an absent file is
			// "not registered", not "not installed".
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
// existing binary a non-fatal warning, an exact match a clean pass. A config the
// extractor cannot parse is a FAILURE — the client cannot load it either, so plumb
// is not running there whatever the file says. When the config parses but holds no
// recognisable plumb entry, it falls back to a plain "registered" pass (the
// caller already found the word "plumb" in the file).
func classifyClientBinary(c setupTarget, cfgPath, selfPath string) checkResult {
	detail := contractConfigPath(cfgPath)
	if c.extractFn == nil {
		return checkResult{ok: true, detail: detail}
	}
	regPath, registered, err := c.extractFn(cfgPath)
	if err != nil {
		return checkResult{
			ok:     false,
			detail: detail + "\nconfig cannot be parsed: " + err.Error(),
			fix: fmt.Sprintf("fix the syntax in %s — %s cannot load it either, so plumb is not registered there",
				detail, c.name),
		}
	}
	if !registered {
		return checkResult{ok: true, detail: detail}
	}
	regPath = expandRegisteredPath(regPath)
	if _, err := os.Stat(regPath); err != nil {
		return checkResult{
			ok:     false,
			detail: detail + "\nregistered binary missing: " + render.ContractPath(regPath),
			fix:    repointFix(c, cfgPath),
		}
	}
	if selfPath != "" && !sameBinary(regPath, selfPath) {
		return checkResult{
			ok:     true,
			warn:   true,
			detail: detail + "\nregistered: " + render.ContractPath(regPath) + "\ncurrent:    " + render.ContractPath(selfPath),
			fix:    repointFix(c, cfgPath),
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
// after symlink resolution so a symlinked install matches its target. Delegates
// to paths.Canonical — the tree's one "same place?" answer. All three call
// sites stat the path first, so only Canonical's existing-path branch
// (identical to this helper's old EvalSymlinks) is ever reached.
func sameBinary(a, b string) bool {
	return paths.Canonical(a) == paths.Canonical(b)
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
		return results
	}
	base, _ := config.Load()
	if _, err := config.LoadProject(base, ws); err != nil {
		results = append(results, checkResult{
			name:   "project config",
			ok:     false,
			detail: err.Error(),
			fix:    "fix TOML syntax in " + contractConfigPath(projectPath),
		})
		return results
	}
	results = append(results, checkResult{
		name:   "project config",
		ok:     true,
		detail: contractConfigPath(projectPath),
	})
	if r, ok := checkProjectPolicyTrust(ws); ok {
		results = append(results, r)
	}
	return results
}

// checkProjectPolicyTrust reports whether this workspace's project config asks
// for capability-granting settings ([git], the exec-deciding [lsp.<lang>]
// fields) and whether they are actually in effect.
//
// It exists because the alternative is silence. plumb ignores an untrusted
// project config's [git] and [lsp.*] request, which is the correct default and a
// terrible thing to do quietly: a user who wrote `[lsp.html] root_markers` and
// saw nothing happen has no way to tell a typo from a trust boundary. ok is
// false when the project asks for nothing — the row is then omitted rather than
// shown as a pass with nothing to say.
func checkProjectPolicyTrust(ws string) (checkResult, bool) {
	st, err := config.ProjectPolicyStatusFor(ws)
	if err != nil || st.Spec.IsEmpty() {
		return checkResult{}, false
	}
	return projectPolicyTrustResult(ws, st), true
}

// projectPolicyTrustResult is the pure decision half of checkProjectPolicyTrust.
//
// An untrusted request is a WARNING, not a failure: the settings being ignored is
// the safe state, not a broken one, and doctor's exit code is reserved for things
// that are actually broken. It still gets the attention colour and a fix line,
// because it is the answer to "why did my project config do nothing".
func projectPolicyTrustResult(ws string, st config.ProjectPolicyStatus) checkResult {
	const name = "capability trust"
	keys := strings.Join(st.Spec.Keys(), "\n")
	if st.Trusted {
		return checkResult{
			name:   name,
			ok:     true,
			detail: fmt.Sprintf("trusted — %d key(s) in effect\n%s", len(st.Spec), keys),
		}
	}
	return checkResult{
		name: name,
		ok:   true,
		warn: true,
		detail: fmt.Sprintf("NOT in effect — this project's config sets %d capability-granting key(s) plumb is ignoring:\n%s\n"+
			"the global config's values are in force instead", len(st.Spec), keys),
		fix: "review them with `plumb config show --workspace " + ws + "`, then run `plumb trust " + ws + "` to honour them",
	}
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
