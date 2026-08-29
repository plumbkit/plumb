package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// skillsSourceDir is the directory internal/cli embeds as the shipped skills.
// The guard below lives here rather than in internal/cli because this is where
// the real tool schemas are constructible (leanToolSet + nonLeanToolSet is the
// whole registration, pinned by TestFullToolSet_Count); the embed reads the same
// files from disk.
const skillsSourceDir = "../cli/skills"

var (
	skillCallRe   = regexp.MustCompile(`\b([a-z][a-z0-9_]*)\(([^)]*)\)`)
	skillArgRe    = regexp.MustCompile(`\b([a-zA-Z_][a-zA-Z0-9_]*)\s*=\s*("[^"]*"|[^,\s]*)`)
	skillQuotedRe = regexp.MustCompile(`"[^"]*"`)
	// A roster bullet: a list item whose first element is a bolded, backticked
	// identifier. That is the skills' own convention for "this names a tool", and
	// anchoring to it is what keeps the guard free of false positives — a bare
	// backticked `expected_mtime` in prose is a parameter, not a tool, and is not
	// matched here.
	skillBulletRe = regexp.MustCompile("(?m)^\\s*-\\s+\\*\\*`([a-z][a-z0-9_]*)`\\*\\*")
	// An enum value is always a plain word. Anything else in an example is a
	// placeholder (`…`, `<slot>`, a list, an expression) and is not a claim about
	// a concrete value, so it is not checked against the enum.
	skillLiteralRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
)

// skillCounts pins how much checkable content each shipped skill carries, per
// form. Without it the only emptiness check is "did every skill go dark at
// once", which is useless: some of these skills contribute nothing in one form,
// so a regression that silenced an extractor for a single skill would gut real
// coverage and still pass.
//
// The counts are EXACT (compared with !=), not minima. A minimum only catches
// the extractor breaking; it says nothing when a skill gains examples, which is
// precisely when the numbers should be re-read — under a minimum, five new
// unchecked call examples and zero new ones are indistinguishable. Updating a
// number is a one-line statement that the new content was looked at.
type skillCounts struct {
	calls   int // worked examples in `tool(arg=value)` form
	bullets int // roster bullets of the form "- **`tool_name`** — …"
}

var skillContentCounts = map[string]skillCounts{
	"plumb-chat": {calls: 5, bullets: 3},
	// 7, not 6: PLAN-376 added a `diagnostics()` call-form example teaching the
	// plain diagnostics() tool's separate INCOMPLETE label, alongside the
	// existing await_diagnostics pair.
	"plumb-diagnose": {calls: 7, bullets: 2},
	// 1, not 0: PLAN-376 added a session_start({detail:"brief"}) worked example
	// for cheap subagent re-orientation, on top of the existing 14 discovery-
	// ladder roster bullets.
	"plumb-explore": {calls: 1, bullets: 14},
	"plumb-git":     {calls: 10, bullets: 3},
	"plumb-memory":  {calls: 8, bullets: 0},
	// 1, not 0: PLAN-376 added a minimal_diff_review(mode="changed") worked
	// example alongside the Low-confidence-cap doctrine it now teaches.
	"plumb-minimal-change": {calls: 1, bullets: 0},
	// 7, not 5: PLAN-376 added a quick-reference row for read_multiple_files
	// (batch reads before a multi-file edit) and one for edit_file's
	// fail_on_new_errors (the refuse-to-break-the-build default verify move).
	"plumb-refactor": {calls: 7, bullets: 0},
	// 6, not 4: the skill gained two worked examples when the shipped test
	// defaults grew {target} placeholders and topology_affected started emitting
	// a target run_task can take (PLAN-378) — the very composition this file's
	// doc comment names as the defect class it exists to catch.
	"plumb-testing": {calls: 6, bullets: 0},
}

// TestSkillExamplesUseRealToolArguments ties worked examples in a shipped skill
// to the real tool schemas.
//
// Both review rounds on the skills work found the same defect class — a skill
// teaching a call the code refuses (await_diagnostics named on tools that do not
// declare it; a run_task target no shipped command can take) — and both were
// caught by a human reading the file, not by a test. Many schemas set
// "additionalProperties": false and argument validation hard-rejects extras, so
// a wrong argument in a skill is not a style problem, it is a guaranteed failed
// call for whichever agent follows the example.
//
// Four facts are checked, three of them added after a mutation pass found the
// name-only version passing an out-of-enum value, a call missing a required
// argument, and a misspelt tool name in backticked prose:
//
//   - UNDECLARED ARGUMENT — the argument is not in the schema's properties.
//   - OUT-OF-ENUM VALUE — the property declares an enum and the example's
//     concrete value is not one of its members.
//   - MISSING REQUIRED ARGUMENT — see the fragment rule below.
//   - UNREGISTERED TOOL NAME — a roster bullet naming a tool plumb does not
//     register, which is how a rename or a typo produces a skill pointing at
//     nothing. This is the hole that mattered most: plumb-explore names its
//     tools in bullets rather than call form, so before this it had no coverage
//     at all beyond "the file is non-empty".
//
// THE FRAGMENT RULE. A call naming none of its tool's required parameters is
// prose illustrating one argument (`edit_file(expected_mtime=…)`), not a worked
// example, and demanding the rest would only push authors out of call form —
// where the arguments stop being checkable at all. A call that names at least
// one required parameter is presenting itself as a call, and must name them all.
//
// The remaining limit is stated because a guard that looks broader than it is
// costs more than no guard: a tool named in ordinary backticked prose, outside
// both call form and a roster bullet, is still unchecked. Keeping tool facts in
// one of the two checkable forms is what makes them checkable.
func TestSkillExamplesUseRealToolArguments(t *testing.T) {
	declared, required, enums := toolSchemaFacts(t)

	entries, err := os.ReadDir(skillsSourceDir)
	if err != nil {
		t.Fatalf("reading %s: %v", skillsSourceDir, err)
	}

	seen := map[string]skillCounts{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(skillsSourceDir, e.Name(), "SKILL.md")
		body, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if _, pinned := skillContentCounts[e.Name()]; !pinned {
			t.Errorf("%s: new skill has no entry in skillContentCounts — add one stating how many "+
				"call-form examples and roster bullets it carries, so its coverage cannot silently be zero", e.Name())
		}
		got := seen[e.Name()]

		for _, b := range skillBulletRe.FindAllStringSubmatch(string(body), -1) {
			got.bullets++
			if _, isTool := declared[b[1]]; !isTool {
				t.Errorf("%s: roster bullet names %q, which plumb does not register — a renamed or "+
					"misspelt tool leaves the skill steering an agent at nothing", path, b[1])
			}
		}

		// Neutralise parentheses INSIDE string literals before matching calls: a
		// ")" in a value would otherwise end the argument list early and hide
		// every argument after it. The literal's TEXT is preserved — the previous
		// version deleted whole literals, which is exactly why an out-of-enum
		// value could never have been caught here.
		stripped := skillQuotedRe.ReplaceAllStringFunc(string(body), func(lit string) string {
			return strings.NewReplacer("(", "", ")", "").Replace(lit)
		})
		for _, call := range skillCallRe.FindAllStringSubmatch(stripped, -1) {
			tool, args := call[1], call[2]
			props, isTool := declared[tool]
			if !isTool {
				continue // prose, a shell command, or a non-plumb code sample
			}
			got.calls++
			supplied := map[string]bool{}
			for _, arg := range skillArgRe.FindAllStringSubmatch(args, -1) {
				name, value := arg[1], strings.Trim(arg[2], `"`)
				supplied[name] = true
				if !props[name] {
					t.Errorf("%s: %s(%s) passes %q, which %s does not declare in its InputSchema — "+
						"argument validation rejects undeclared parameters, so this example cannot work",
						path, tool, args, name, tool)
					continue
				}
				if allowed := enums[tool][name]; len(allowed) > 0 && skillLiteralRe.MatchString(value) && !allowed[value] {
					t.Errorf("%s: %s(%s) passes %s=%q, which is not in that parameter's enum (%s) — "+
						"the call is rejected before it reaches the tool", path, tool, args, name, value,
						strings.Join(sortedKeys(allowed), ", "))
				}
			}
			assertRequiredSatisfied(t, path, tool, args, required[tool], supplied)
		}
		seen[e.Name()] = got
	}

	for skill, want := range skillContentCounts {
		if got := seen[skill]; got != want {
			t.Errorf("%s: found %d call-form examples and %d roster bullets, pinned at %d and %d — if the "+
				"skill gained or lost content, re-read the new lines and update the pin; if it did not, the "+
				"extractor regressed and the checks below it are running on less than they appear to",
				skill, got.calls, got.bullets, want.calls, want.bullets)
		}
	}
}

// assertRequiredSatisfied applies the fragment rule: a call that names no
// required parameter is illustrating one argument, not teaching a call.
func assertRequiredSatisfied(t *testing.T, path, tool, args string, req []string, supplied map[string]bool) {
	t.Helper()
	var namesOne bool
	for _, r := range req {
		if supplied[r] {
			namesOne = true
			break
		}
	}
	if !namesOne {
		return
	}
	for _, r := range req {
		if !supplied[r] {
			t.Errorf("%s: %s(%s) omits the required parameter %q — the example reads as a complete call "+
				"(it names other required parameters), and this one is rejected before the tool runs",
				path, tool, args, r)
		}
	}
}

// toolSchemaFacts decodes the three schema facts the guard needs from every
// registered tool: declared property names, the required list, and the enum
// members of any property that constrains its values.
func toolSchemaFacts(t *testing.T) (declared map[string]map[string]bool, required map[string][]string, enums map[string]map[string]map[string]bool) {
	t.Helper()
	declared = map[string]map[string]bool{}
	required = map[string][]string{}
	enums = map[string]map[string]map[string]bool{}

	for _, tl := range append(leanToolSet(), nonLeanToolSet()...) {
		var schema struct {
			Required   []string                   `json:"required"`
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(tl.InputSchema(), &schema); err != nil {
			t.Fatalf("%s: InputSchema is not valid JSON: %v", tl.Name(), err)
		}
		set := make(map[string]bool, len(schema.Properties))
		perTool := map[string]map[string]bool{}
		for name, raw := range schema.Properties {
			set[name] = true
			var prop struct {
				Enum []any `json:"enum"`
			}
			if err := json.Unmarshal(raw, &prop); err != nil || len(prop.Enum) == 0 {
				continue
			}
			members := make(map[string]bool, len(prop.Enum))
			for _, v := range prop.Enum {
				if s, ok := v.(string); ok {
					members[s] = true
				}
			}
			if len(members) > 0 {
				perTool[name] = members
			}
		}
		declared[tl.Name()] = set
		required[tl.Name()] = schema.Required
		if len(perTool) > 0 {
			enums[tl.Name()] = perTool
		}
	}
	return declared, required, enums
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestToolDescriptionsAreNotStubs is the backstop for a wide mechanical prose
// edit across the tool surface. It pins nothing semantic: the property that a
// DRY pass removes only comparative routing and never a contract fact is still
// enforced by review, not by code. What it does is convert a gutted description
// from a silent pass into a loud failure, which is the failure mode a sweeping
// rewrite actually risks.
//
// The floor was 120 bytes, which no shipped description came within 75 bytes of
// — a backstop that far below the real distribution catches only a description
// deleted outright, not one hollowed to a single clause. It is 170 now, measured
// rather than guessed: the three shortest descriptions today are read_memory
// (195), type_hierarchy (197) and delete_memory (198), so 170 leaves the tightest
// of them 25 bytes of headroom while ruling out anything under two sentences.
// Raise it if the short tail moves up; do not lower it to make a red build green
// — a description that short is the bug the test names.
func TestToolDescriptionsAreNotStubs(t *testing.T) {
	const floor = 170
	for _, tl := range append(leanToolSet(), nonLeanToolSet()...) {
		if n := len(tl.Description()); n < floor {
			t.Errorf("%s: Description() is %d bytes, under the %d-byte floor — a tool description "+
				"that short cannot state what the tool does and when to reach for it", tl.Name(), n, floor)
		}
	}
}
