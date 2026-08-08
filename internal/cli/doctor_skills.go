package cli

import (
	"fmt"
	"strings"

	"github.com/plumbkit/plumb/internal/render"
)

// checkSkillFreshness is the doctor grade on skill drift: one INFORMATIONAL
// line per registered skill-capable client whose installed skills are missing
// or stale. Never a warning — stale skills are not a broken integration, they
// are content one `plumb skills sync` away, so a "!" would inflate doctor's
// warning count for a repair. A client with current skills, or one whose
// config does not register plumb, produces no line at all: the suggestion in
// the detail goes out on a clean pass, the same contract as the Kimi
// no-allowlist hint (fix lines only render on attention).
func checkSkillFreshness() []checkResult {
	var results []checkResult
	for _, t := range skillCapableClients() {
		if r, ok := skillFreshnessResult(t); ok {
			results = append(results, r)
		}
	}
	return results
}

// skillFreshnessResult is checkSkillFreshness's per-client body, split out so a
// test can drive a target pointed at temp config and skills directories.
func skillFreshnessResult(t setupTarget) (checkResult, bool) {
	if !plumbRegisteredIn(t) {
		return checkResult{}, false
	}
	dir, drifted := skillsDrift(t)
	if !drifted {
		return checkResult{}, false
	}
	missing, stale := skillDriftCounts(dir)
	detail := describeSkillDrift(missing, stale)
	if info := skillStaleDetails(dir); len(info) > 0 {
		detail += " (" + strings.Join(info, ", ") + ")"
	}
	return checkResult{
		name: t.name + " (skills)",
		ok:   true,
		detail: fmt.Sprintf("%s in %s — run `plumb skills sync %s`",
			detail, render.ContractPath(dir), t.use),
	}, true
}

// describeSkillDrift renders the tallies, e.g. "2 skill(s) missing, 1 stale".
// Both counts always print: "0 stale" is the useful half of the message when
// everything is simply absent.
func describeSkillDrift(missing, stale int) string {
	return fmt.Sprintf("%d skill(s) missing, %d stale", missing, stale)
}
