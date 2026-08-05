package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
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
	skillArgRe    = regexp.MustCompile(`\b([a-zA-Z_][a-zA-Z0-9_]*)\s*=`)
	skillQuotedRe = regexp.MustCompile(`"[^"]*"`)
)

// minCallsPerSkill pins how many tool calls each shipped skill expresses in
// call form. Without it the only emptiness check is "did every skill go dark at
// once", which is useless: two of these skills contribute nothing today, so a
// regression that silenced the extractor for plumb-testing alone would reduce
// real coverage to a quarter and still pass. Raise a number when a skill gains
// examples; lowering one is a deliberate statement that the skill genuinely
// stopped using call form, not a way to make a red test green.
var minCallsPerSkill = map[string]int{
	"plumb-explore":        0,
	"plumb-minimal-change": 0,
	"plumb-refactor":       5,
	"plumb-testing":        4,
}

// TestSkillExamplesUseRealToolArguments ties worked examples in a shipped skill
// to the real tool schemas.
//
// Both review rounds on the skills work found the same defect class — a skill
// teaching a call the code refuses (await_diagnostics named on tools that do not
// declare it; a run_task target no shipped command can take) — and both were
// caught by a human reading the file, not by a test. This catches one shape of
// it: an argument the tool does not declare. Many schemas set
// "additionalProperties": false and argument validation hard-rejects extras, so
// an undeclared argument in a skill is not a style problem, it is a guaranteed
// failed call for whichever agent follows the example.
//
// Two limits, both real, stated because a guard that looks broader than it is
// costs more than no guard at all:
//
//   - SYNTACTIC. It only sees arguments written inside `tool(arg=…)`. The
//     await_diagnostics defect that motivated this test was written as prose
//     with an inline backticked `arg=value` and is NOT caught here; nor is a
//     misspelt tool name, nor an argument in a JSON-shaped example. Keeping
//     argument facts in call form is what makes them checkable.
//   - SEMANTIC. A declared argument refused for the state it is used in
//     (run_task's target against a command with no {target} placeholder) is a
//     runtime precondition, not a schema fact, and stays a review obligation.
func TestSkillExamplesUseRealToolArguments(t *testing.T) {
	declared := map[string]map[string]bool{}
	for _, tl := range append(leanToolSet(), nonLeanToolSet()...) {
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(tl.InputSchema(), &schema); err != nil {
			t.Fatalf("%s: InputSchema is not valid JSON: %v", tl.Name(), err)
		}
		set := make(map[string]bool, len(schema.Properties))
		for name := range schema.Properties {
			set[name] = true
		}
		declared[tl.Name()] = set
	}

	entries, err := os.ReadDir(skillsSourceDir)
	if err != nil {
		t.Fatalf("reading %s: %v", skillsSourceDir, err)
	}

	seen := map[string]int{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(skillsSourceDir, e.Name(), "SKILL.md")
		body, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if _, pinned := minCallsPerSkill[e.Name()]; !pinned {
			t.Errorf("%s: new skill has no entry in minCallsPerSkill — add one stating how many "+
				"call-form examples it carries, so its coverage cannot silently be zero", e.Name())
		}
		// Strip quoted values from the WHOLE body before matching calls: a ")"
		// inside a string literal would otherwise end the argument list early and
		// hide every argument after it.
		stripped := skillQuotedRe.ReplaceAllString(string(body), "")
		for _, call := range skillCallRe.FindAllStringSubmatch(stripped, -1) {
			tool, args := call[1], call[2]
			props, isTool := declared[tool]
			if !isTool {
				continue // prose, a shell command, or a non-plumb code sample
			}
			seen[e.Name()]++
			for _, arg := range skillArgRe.FindAllStringSubmatch(args, -1) {
				if !props[arg[1]] {
					t.Errorf("%s: %s(%s) passes %q, which %s does not declare in its InputSchema — "+
						"argument validation rejects undeclared parameters, so this example cannot work",
						path, tool, args, arg[1], tool)
				}
			}
		}
	}
	for skill, want := range minCallsPerSkill {
		if got := seen[skill]; got < want {
			t.Errorf("%s: found %d call-form tool examples, expected at least %d — either the "+
				"extractor regressed or the skill moved argument facts out of call form, where "+
				"they stop being checkable", skill, got, want)
		}
	}
}

// TestToolDescriptionsAreNotStubs is the backstop for a wide mechanical prose
// edit across the tool surface. It pins nothing semantic: the property that a
// DRY pass removes only comparative routing and never a contract fact is still
// enforced by review, not by code. What it does is convert a gutted description
// from a silent pass into a loud failure, which is the failure mode a sweeping
// rewrite actually risks.
func TestToolDescriptionsAreNotStubs(t *testing.T) {
	const floor = 120
	for _, tl := range append(leanToolSet(), nonLeanToolSet()...) {
		if n := len(tl.Description()); n < floor {
			t.Errorf("%s: Description() is %d bytes, under the %d-byte floor — a tool description "+
				"that short cannot state what the tool does and when to reach for it", tl.Name(), n, floor)
		}
	}
}
