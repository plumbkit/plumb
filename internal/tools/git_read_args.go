package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/plumbkit/plumb/internal/paths"
)

// git_read_args.go refuses the options that would make a READ-tier command do
// something other than read the repository. The read tier is always on — no
// [git] grant, no confirm — so everything it passes through to git must be a
// read of this repository, and a handful of options on the read verbs are not:
//
//   - `--output=<file>` (diff, log, show, shortlog, blame, stash show) writes
//     the command's output to any path the caller names, creating or truncating
//     it;
//   - `grep -O` / `--open-files-in-pager` runs a program;
//   - `--no-index` (diff, grep) and `blame --contents` read files OUTSIDE the
//     repository and print them; `blame -S` and `--ignore-revs-file` read a file
//     outside it as revisions and echo its lines in git's own error messages;
//     `grep -f` reads one as patterns and echoes a line that fails to compile.
//
// A list of options cannot be the whole answer, because git also leaves the
// repository WITHOUT any option: `git diff <outside-path> <file>` switches to
// no-index mode by itself when one of two paths is outside the working tree. So
// checkReadArgPaths is the structural net behind the list: no argument of a read
// may name an existing path outside the repository.
//
// Options are read through gitOptionGrammar, so an abbreviation (`--open` for
// --open-files-in-pager), a bundle (`-nO`) or a value given as the next argument
// counts as git counts it. The match is by prefix, so it also refuses some
// spellings git itself would reject as unknown or ambiguous; the scan runs past
// "--" and ignores negations. Both can only over-refuse, which is the safe
// error.

// readRefusal is one refused option on one read verb.
type readRefusal struct {
	shorts string   // short spellings, as gitOption.is takes them
	longs  []string // long names, matched by unambiguous prefix
	// exact are long names git matches EXACTLY to a different option, so a
	// prefix of a refused name that equals one of them is not that name:
	// blame's --ignore-rev (a revision) is a prefix of --ignore-revs-file.
	exact []string
	does  string // what the option would do, for the refusal
}

// diffFamilyGrammar is the value-taking shorts of the diff and log options the
// read verbs share: -S, -G, -l, -n, -L, -U and -O take a value (joined or the
// next argument), and -M, -C and -B an optional one as the rest of their token.
// Without it a pickaxe such as `-SOpen` would read as -O.
var diffFamilyGrammar = gitOptionGrammar{valueShort: "SGlnLUO", optionalShort: "MCB"}

// readArgGrammars give each policed verb the options that take a value, so a
// value is not mistaken for an option (`grep -e -O` searches for "-O"; it opens
// no pager). They must match git EXACTLY in one direction: an option listed as
// value-taking that git does not treat so would swallow the next token here
// while git reads it as a live option — a bypass. blame's -C and -M take an
// OPTIONAL score as the rest of their token, so `-CL -S <file>` is -C with score
// "L" and a live -S, never a -L swallowing it. A verb not listed reads every
// token as a possible option, which can only over-refuse.
var readArgGrammars = map[string]gitOptionGrammar{
	"grep":     {valueShort: "efABCm", valueLong: []string{"after-context", "before-context", "context", "max-count", "max-depth", "threads"}, optionalShort: "O"},
	"blame":    {valueShort: "LS", optionalShort: "CM"},
	"diff":     diffFamilyGrammar,
	"log":      diffFamilyGrammar,
	"show":     diffFamilyGrammar,
	"stash":    diffFamilyGrammar,
	"ls-files": {valueShort: "xX"},
}

// orderFile refuses diff's -O<orderfile>: it reads the named file whole, with no
// bound — `-O/dev/zero` grows the git child by gigabytes a second — and only the
// joined spelling escapes checkReadArgPaths, which sees separate arguments.
var orderFile = readRefusal{shorts: "O", does: "reads a file of any size from anywhere as a path order"}

var readRefusals = map[string][]readRefusal{
	"diff": {
		{longs: []string{"output"}, does: "writes the output to a file"},
		{longs: []string{"no-index"}, does: "reads files outside the repository"},
		orderFile,
	},
	"log":      {{longs: []string{"output"}, does: "writes the output to a file"}, orderFile},
	"show":     {{longs: []string{"output"}, does: "writes the output to a file"}, orderFile},
	"shortlog": {{longs: []string{"output"}, does: "writes the output to a file"}},
	// Only `stash show` and `stash list` reach the read tier, and both take diff
	// and log options.
	"stash": {{longs: []string{"output"}, does: "writes the output to a file"}, orderFile},
	"blame": {
		{longs: []string{"output"}, does: "writes the output to a file"},
		{longs: []string{"contents"}, does: "reads a file outside the repository"},
		{shorts: "S", longs: []string{"ignore-revs-file"}, exact: []string{"ignore-rev"}, does: "reads a file outside the repository"},
	},
	"ls-files": {
		{shorts: "X", longs: []string{"exclude-from"}, does: "reads patterns from a file, which then tell what that file holds"},
	},
	"grep": {
		{shorts: "O", longs: []string{"open-files-in-pager"}, does: "runs a program"},
		{longs: []string{"no-index"}, does: "searches outside the repository"},
		{shorts: "f", does: "reads patterns from a file, echoing a line that fails to compile"},
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
		if refusedIn(grammar, args, r) {
			return fmt.Errorf("git %s: %s is not permitted — it %s, and the read tier only reads this repository",
				sub, readRefusalName(r), r.does)
		}
	}
	return nil
}

// refusedIn reports whether args carry r's option, as grammar reads them.
func refusedIn(grammar gitOptionGrammar, args []string, r readRefusal) bool {
	found := false
	grammar.scan(args, false, func(o gitOption) bool {
		found = o.is(r.shorts, r.longs...) && (o.long == "" || !slices.Contains(r.exact, o.long))
		return !found
	})
	return found
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

// checkReadArgPaths refuses a read whose arguments name an existing path outside
// the repository at root — the net behind readRefusals for the cases no option
// marks, such as diff's implicit no-index mode. It looks at every argument that
// does not start with "-", at every long option's joined value, and at EVERY
// argument after "--" or "--end-of-options" — git reads those as paths whatever
// they spell, a leading "-" included. It resolves a relative one against root (git's working directory), and refuses it when it
// exists and its real location is not inside root. A revision, a pathspec that
// matches nothing on disk, or a path inside the repository passes untouched.
func checkReadArgPaths(root string, args []string) error {
	canonRoot := paths.Canonical(root)
	endOfOptions := false
	for _, a := range args {
		candidate := a
		if !endOfOptions && isEndOfOptions(a) {
			endOfOptions = true
			continue
		}
		if !endOfOptions && strings.HasPrefix(a, "-") {
			_, value, joined := strings.Cut(a, "=")
			if !joined || !strings.HasPrefix(a, "--") {
				continue
			}
			candidate = value
		}
		if candidate == "" {
			continue
		}
		p := candidate
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, p)
		}
		//nolint:gosec // G703: only stat-ed to decide a refusal; never opened or written.
		if _, err := os.Lstat(p); err != nil {
			continue
		}
		if !dirWithinRoot(paths.Canonical(p), canonRoot) {
			return fmt.Errorf("git: argument %q names a path outside the repository, and the read tier only reads this "+
				"repository. Any argument that resolves to an existing file or directory outside it is refused, even "+
				"a search pattern such as \"/usr\"; anchor such a pattern so it is not a path (e.g. \"^/usr\")", a)
		}
	}
	return nil
}

// readArgsNamePaths reports whether any argument could name a path, so the
// repository root — a git subprocess away — is resolved only when it matters.
func readArgsNamePaths(args []string) bool {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") || isEndOfOptions(a) || (strings.HasPrefix(a, "--") && strings.Contains(a, "=")) {
			return true
		}
	}
	return false
}

// checkReadCallPaths applies checkReadArgPaths to a read-tier call, against the
// repository root git will run in (runGit's cmd.Dir).
func checkReadCallPaths(a gitToolArgs) error {
	if classifyGit(a.Subcommand, a.Args) != tierRead || !readArgsNamePaths(a.Args) {
		return nil
	}
	root, err := findGitRoot(a.Repo)
	if err != nil {
		return nil // git refuses a call with no repository itself; nothing is read
	}
	return checkReadArgPaths(root, a.Args)
}
