package cli

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/config"
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
			detail: fmt.Sprintf("cannot dial %s", render.ContractPath(socketPath)),
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
	p = os.ExpandEnv(p)
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			if p == "~" {
				return home
			}
			return filepath.Join(home, p[2:])
		}
	}
	return p
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
