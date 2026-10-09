package cli

// doctor_kimi.go — warn when Kimi Code's experimental tool-select flag would
// leave plumb unreachable (PLAN-413 phase 2).
//
// With `[experimental] tool-select = true` and a model that declares
// dynamically_loaded_tools, Kimi Code keeps MCP schemas out of the model's
// tools[] and is meant to load them through a select_tools tool. In Kimi Code
// 0.38.0's headless mode (`kimi -p`) it removes every MCP schema but never
// registers select_tools, so the model cannot call a single plumb tool
// (MoonshotAI/kimi-code#2381; TestKimiWireCapture measures it). plumb never
// turns the flag on, but a user may have, and the failure is silent: plumb
// just never gets called. So doctor says so. Whether the active model declares
// the capability is not checked: the flag is the user's choice, and naming the
// condition in the warning is enough.

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

// kimiDisclosureIssue is the upstream defect the warning cites.
const kimiDisclosureIssue = "https://github.com/MoonshotAI/kimi-code/issues/2381"

// checkKimiToolSelect returns the warning row when Kimi Code's config turns
// tool-select on, and nothing otherwise (no Kimi, no config, or flag off).
func checkKimiToolSelect() []checkResult {
	mcpPath, err := KimiCodeConfigPath()
	if err != nil {
		return nil
	}
	if r, ok := kimiToolSelectResultAt(filepath.Join(filepath.Dir(mcpPath), "config.toml")); ok {
		return []checkResult{r}
	}
	return nil
}

// kimiToolSelectResultAt is checkKimiToolSelect's path-injectable body. A
// missing or unparsable config is not doctor's business here (Kimi's own
// `kimi doctor` validates it), so it yields no row.
func kimiToolSelectResultAt(path string) (checkResult, bool) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: reads the Kimi Code config of the invoking user ($KIMI_CODE_HOME or ~/.kimi-code), as setup already does for mcp.json
	if err != nil {
		return checkResult{}, false
	}
	var cfg struct {
		Experimental map[string]any `toml:"experimental"`
	}
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return checkResult{}, false
	}
	if on, _ := cfg.Experimental["tool-select"].(bool); !on {
		return checkResult{}, false
	}
	return checkResult{
		name: "Kimi Code tool-select",
		ok:   true,
		warn: true,
		detail: "[experimental] tool-select is on: with a model declaring dynamically_loaded_tools, " +
			"Kimi Code 0.38.0 run headless (kimi -p) hides every MCP tool without offering select_tools, " +
			"so plumb cannot be called (" + kimiDisclosureIssue + ")",
		fix: fmt.Sprintf("set tool-select = false under [experimental] in %s until that issue is fixed", path),
	}, true
}
