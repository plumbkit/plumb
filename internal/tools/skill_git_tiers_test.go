package tools

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// gitSubcommandUniverse is every subcommand `classifyGit` names, split by how it
// decides. The list is pinned rather than derived because classifyGit's switch
// has a `default: tierReject` arm, so there is nothing to enumerate from — a
// subcommand plumb does not know is indistinguishable from one it rejects.
//
// What the split buys: the TIER of every argIndependent name is computed by
// calling classifyGit, never restated here, so this test cannot itself go stale
// about a tier. Only membership is pinned, and adding a subcommand to
// classifyGit means adding it here — which is the moment to notice the skill
// needs a line too.
var (
	gitArgIndependent = []string{
		"status", "log", "diff", "show", "blame", "shortlog", "check-ignore",
		"add", "commit", "mv",
		"reset", "clean", "rebase", "revert",
		"push", "fetch", "pull",
	}
	// Classified by inspecting their args, so they belong in the skill's
	// argument-dependent bullets rather than any single tier row.
	gitArgDependent = []string{"switch", "restore", "branch", "tag", "stash", "checkout"}
	// Refused at every tier.
	gitRejected = []string{"rm"}
)

// gitTierNames maps the classifier's tiers onto the words the skill's table uses.
// Kept here rather than as a String() method on gitTier: production code has no
// use for one, and a test is not a reason to widen a production type's surface.
var gitTierNames = map[gitTier]string{
	tierRead:        "read",
	tierWrite:       "write",
	tierDestructive: "destructive",
	tierNetwork:     "network",
	tierReject:      "reject",
}

// skillTierRowRe matches one row of the plumb-git tier table, capturing the tier
// name and the cell listing its subcommands.
var skillTierRowRe = regexp.MustCompile(`(?m)^\| (read|write|destructive|network) \| (.+?) \|`)

// skillBacktickedRe pulls the backticked tokens out of a table cell.
var skillBacktickedRe = regexp.MustCompile("`([a-z][a-z-]*)`")

// TestPlumbGitSkillTierTableMatchesClassifier ties the plumb-git skill's tier
// table to classifyGit, which is the only thing that actually decides a tier.
//
// This exists because the table shipped wrong in six separate ways and every one
// of them was caught by a human reading the classifier next to the prose, not by
// a test: `restore --staged` filed as destructive when it is a write, four
// subcommands (`shortlog`, `check-ignore`, `mv`, `revert`) missing entirely, and
// `branch`/`tag`/`stash` presented as flatly "write" when each spans three
// tiers depending on its arguments. A skill that misstates a tier is worse than
// one that omits it: an agent reads "destructive" and asks the user to widen
// `[git] allow_destructive` for an operation that only ever needed
// `allow_writes`.
//
// The check is exact in both directions for the arg-independent subcommands:
// every one must appear in the row whose tier classifyGit gives it, and no row
// may name anything else. Arg-dependent subcommands are deliberately barred from
// the rows — a single cell cannot state a tier that depends on the call — and
// are required in the prose below instead.
//
// The limit, stated because a guard that looks broader than it is costs more
// than no guard: this checks WHICH tier a subcommand is in, not the prose
// describing what the tier gates or how the arg-dependent ones split. Those stay
// review obligations.
func TestPlumbGitSkillTierTableMatchesClassifier(t *testing.T) {
	body := readSkill(t, "plumb-git")

	wantByTier := map[string][]string{}
	for _, sub := range gitArgIndependent {
		tier := gitTierNames[classifyGit(sub, nil)]
		wantByTier[tier] = append(wantByTier[tier], sub)
	}

	gotByTier := map[string][]string{}
	rows := skillTierRowRe.FindAllStringSubmatch(body, -1)
	if len(rows) != 4 {
		t.Fatalf("found %d tier rows in the plumb-git table, want 4 — the table shape changed and this "+
			"guard is no longer reading it", len(rows))
	}
	for _, row := range rows {
		tier, cell := row[1], row[2]
		for _, m := range skillBacktickedRe.FindAllStringSubmatch(cell, -1) {
			gotByTier[tier] = append(gotByTier[tier], m[1])
		}
	}

	for _, tier := range []string{"read", "write", "destructive", "network"} {
		want, got := wantByTier[tier], gotByTier[tier]
		sort.Strings(want)
		sort.Strings(got)
		if strings.Join(want, ",") != strings.Join(got, ",") {
			t.Errorf("plumb-git %q row lists %v, but classifyGit puts exactly %v at that tier — "+
				"a skill that misstates a tier sends the agent to widen the wrong policy knob",
				tier, got, want)
		}
	}

	// An arg-dependent subcommand in a tier row would be a claim the classifier
	// cannot honour, whichever row it landed in.
	for _, tier := range gotByTier {
		for _, sub := range tier {
			for _, dep := range gitArgDependent {
				if sub == dep {
					t.Errorf("plumb-git's tier table names %q, whose tier depends on its arguments — "+
						"it belongs in the argument-dependent bullets, not a fixed row", sub)
				}
			}
		}
	}
}

// TestPlumbGitSkillNamesEveryArgDependentSubcommand pins the other half: the six
// subcommands excluded from the table above must still be explained somewhere,
// or excluding them would just be a way to omit them. Same for the one plumb
// refuses outright — an agent that does not know `rm` is refused will keep
// reaching for it.
func TestPlumbGitSkillNamesEveryArgDependentSubcommand(t *testing.T) {
	body := readSkill(t, "plumb-git")
	for _, sub := range append(append([]string{}, gitArgDependent...), gitRejected...) {
		if !strings.Contains(body, "`"+sub+"`") {
			t.Errorf("plumb-git never mentions %q, which plumb classifies specially — an agent following "+
				"the skill will meet its behaviour only by being refused", sub)
		}
	}
}

func readSkill(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(skillsSourceDir, name, "SKILL.md"))
	if err != nil {
		t.Fatalf("reading the %s skill: %v", name, err)
	}
	return string(body)
}
