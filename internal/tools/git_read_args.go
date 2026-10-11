package tools

import (
	"fmt"
	"strings"
)

// git_read_args.go refuses the options that would make a READ-tier command do
// something other than read the repository. The read tier is always on — no
// [git] grant, no confirm — so everything it passes through to git must be a
// read of this repository, and a handful of options on the read verbs are not:
//
//   - `--output=<file>` (diff, log, show, shortlog, stash show) writes the
//     command's output to any path the caller names, creating or truncating it;
//   - `--no-index` (diff) and `blame --contents` read files OUTSIDE the
//     repository and print them; `blame -S` and `--ignore-revs-file` read a file
//     outside it as revisions and echo its lines in git's own error messages.
//
// Options are read through gitOptionGrammar, so an abbreviation (`--outp`), a
// bundle (`-nO`) or a joined value counts as git counts it. The scan runs past
// "--" and ignores negations, so it can only over-refuse — a path after "--"
// that spells one of these options is refused too, which is the safe error.

// readRefusal is one refused option on one read verb.
type readRefusal struct {
	shorts string   // short spellings, as gitOption.is takes them
	longs  []string // long names, matched by unambiguous prefix
	does   string   // what the option would do, for the refusal
}

// readArgGrammars give each policed verb the options that take a REQUIRED
// value, so a value is not mistaken for an option (`blame -L/S/,+2` is a line
// range; it names no revisions file). A verb not listed reads every token as a
// possible option, which can only over-refuse.
var readArgGrammars = map[string]gitOptionGrammar{
	"blame": {valueShort: "LS"},
}

var readRefusals = map[string][]readRefusal{
	"diff": {
		{longs: []string{"output"}, does: "writes the output to a file"},
		{longs: []string{"no-index"}, does: "reads files outside the repository"},
	},
	"log":      {{longs: []string{"output"}, does: "writes the output to a file"}},
	"show":     {{longs: []string{"output"}, does: "writes the output to a file"}},
	"shortlog": {{longs: []string{"output"}, does: "writes the output to a file"}},
	// Only `stash show` and `stash list` reach the read tier, and both take diff
	// and log options.
	"stash": {{longs: []string{"output"}, does: "writes the output to a file"}},
	"blame": {
		{longs: []string{"contents"}, does: "reads a file outside the repository"},
		{shorts: "S", longs: []string{"ignore-revs-file"}, does: "reads a file outside the repository"},
	},
}

// checkReadArgs refuses a read-tier call that carries one of its verb's refused
// options.
func checkReadArgs(sub string, args []string) error {
	refusals := readRefusals[sub]
	if len(refusals) == 0 {
		return nil
	}
	grammar := readArgGrammars[sub]
	for _, r := range refusals {
		if grammar.has(args, r.shorts, r.longs...) {
			return fmt.Errorf("git %s: %s is not permitted — it %s, and the read tier only reads this repository",
				sub, readRefusalName(r), r.does)
		}
	}
	return nil
}

// readRefusalName spells a refusal's option the way a caller would look for it.
func readRefusalName(r readRefusal) string {
	names := make([]string, 0, len(r.shorts)+len(r.longs))
	for _, c := range r.shorts {
		names = append(names, "-"+string(c))
	}
	for _, l := range r.longs {
		names = append(names, "--"+l)
	}
	return strings.Join(names, "/")
}
