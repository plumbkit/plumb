package tools

import "strings"

// git_options.go reads a git subcommand's arguments the way git's own option
// parser (parse-options) does, as far as tiering needs to.
//
// The tier classifiers and merge's refused-flag check used to match options by
// exact spelling, one token at a time. git reads them differently in three
// ways, and each difference was a way to run an operation at a LOWER tier than
// git would actually perform it (#530, #540 review):
//
//   - git expands any unambiguous prefix of a long option: `switch --disc` IS
//     --discard-changes, `branch --del` IS --delete.
//   - git unpacks bundled short flags: `branch -dr` is -d -r.
//   - an option with a required value takes the NEXT argument whatever it
//     spells: in `merge --message -m --no-verify`, "-m" is the message and
//     --no-verify is live; in `merge --message -- --no-verify`, "--" is the
//     message and does not end the options.
//
// A long name is matched by prefix, down to one character. That is safe in both
// directions: whenever git resolves an abbreviation to option X, the name is a
// prefix of X, so it matches X here; and git never resolves a prefix of X to a
// different option Y unless the name is exactly Y (otherwise it is ambiguous and
// git refuses the command) — which no grammar below allows for an option that
// changes a tier.

// gitOptionGrammar is the part of one subcommand's option grammar that decides
// how its arguments are read: which options consume a value. A long option
// whose value is optional (`--track[=x]`) takes it only joined, so it is not
// listed; a short one (`-n[<n>]`, `-S[<key>]`) takes the rest of its token.
type gitOptionGrammar struct {
	valueLong     []string // long options with a required value
	valueShort    string   // short options with a required value
	optionalShort string   // short options whose optional value is the rest of the token
}

// gitOption is one option occurrence as git's parser reads it: a long option's
// name as written (possibly abbreviated, without "--" or "=value"), or a short
// option's letter.
type gitOption struct {
	long  string
	short rune
}

// is reports whether o is the short option in shorts or an abbreviation of one
// of longs.
func (o gitOption) is(shorts string, longs ...string) bool {
	if o.short != 0 {
		return strings.ContainsRune(shorts, o.short)
	}
	for _, l := range longs {
		if isLongPrefix(o.long, l, 1) {
			return true
		}
	}
	return false
}

// scan calls fn with each option in args, in order, as git reads them: a
// required value is consumed (joined, or the next argument), bundled short
// flags are unpacked up to the first that takes a value, and a lone "-" or any
// non-option is skipped. With untilDashDash, a "--" (or "--end-of-options")
// that is not a value ends the scan — for a check that must only count real options, such as the one
// that LOWERS restore to the write tier. Without it the scan runs to the end,
// which is the safe choice for any check that raises a tier or refuses: a path
// after "--" that spells an option only over-classifies. fn returns false to
// stop.
func (g gitOptionGrammar) scan(args []string, untilDashDash bool, fn func(gitOption) bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if isEndOfOptions(a) {
			if untilDashDash {
				return
			}
			continue
		}
		var consumesNext, more bool
		if name, ok := strings.CutPrefix(a, "--"); ok {
			consumesNext, more = g.scanLong(name, fn)
		} else if len(a) > 1 && a[0] == '-' {
			consumesNext, more = g.scanShorts(a[1:], fn)
		} else {
			continue
		}
		if !more {
			return
		}
		if consumesNext {
			i++
		}
	}
}

func (g gitOptionGrammar) scanLong(name string, fn func(gitOption) bool) (consumesNext, more bool) {
	name, _, joined := strings.Cut(name, "=")
	if !fn(gitOption{long: name}) {
		return false, false
	}
	if joined {
		return false, true
	}
	for _, v := range g.valueLong {
		if isLongPrefix(name, v, 1) {
			return true, true
		}
	}
	return false, true
}

func (g gitOptionGrammar) scanShorts(bundle string, fn func(gitOption) bool) (consumesNext, more bool) {
	for i, c := range bundle {
		if !fn(gitOption{short: c}) {
			return false, false
		}
		if strings.ContainsRune(g.valueShort, c) {
			// The rest of the token is the value; with none, the next argument is.
			return i == len(bundle)-1, true
		}
		if strings.ContainsRune(g.optionalShort, c) {
			return false, true // the rest of the token, if any, is the value
		}
	}
	return false, true
}

// has reports whether args carry any of the given options anywhere — past
// "--" and ignoring negations, so it can only over-report. It is for checks
// that RAISE a tier or refuse; a check that lowers one uses final.
func (g gitOptionGrammar) has(args []string, shorts string, longs ...string) bool {
	found := false
	g.scan(args, false, func(o gitOption) bool {
		found = o.is(shorts, longs...)
		return !found
	})
	return found
}

// count returns how many times args carry any of the given options, however
// each is spelled: a bundled short flag, a joined value (`-Bname`) and an
// abbreviated long name all count as git counts them. Like has it runs past
// "--" and ignores negations, so it can only over-count. A check that lowers a
// tier because an option appears EXACTLY once pairs it with a token-level read
// of the same option (git_ref_reset.go): when the two disagree, the call is not
// one it understands.
func (g gitOptionGrammar) count(args []string, shorts string, longs ...string) int {
	n := 0
	g.scan(args, false, func(o gitOption) bool {
		if o.is(shorts, longs...) {
			n++
		}
		return true
	})
	return n
}

// positionals returns the arguments git reads as positional: neither an
// option nor an option's value, plus everything after the end of options.
func (g gitOptionGrammar) positionals(args []string) []string {
	var out []string
	ignore := func(gitOption) bool { return true }
	for i := 0; i < len(args); i++ {
		a := args[i]
		var consumesNext bool
		switch {
		case isEndOfOptions(a):
			return append(out, args[i+1:]...)
		case strings.HasPrefix(a, "--"):
			consumesNext, _ = g.scanLong(a[2:], ignore)
		case len(a) > 1 && a[0] == '-':
			consumesNext, _ = g.scanShorts(a[1:], ignore)
		default:
			out = append(out, a)
		}
		if consumesNext {
			i++
		}
	}
	return out
}

// final reports whether any of opts is in force once all of args are read: like
// has, but a later --no-<opt> cancels an earlier --<opt> (or -<o>), as git's
// parser does. That is the reading a check that LOWERS a tier needs —
// `branch --list --no-list x` is not in list mode — while a check that raises
// one can ignore negations and so over-classify, which is the safe error. A
// name that is itself one of opts (`--no-contains`) sets that option rather
// than cancelling another.
func (g gitOptionGrammar) final(args []string, untilDashDash bool, opts ...gitOptName) bool {
	on := map[string]bool{}
	g.scan(args, untilDashDash, func(o gitOption) bool {
		for _, n := range opts {
			if o.is(string(n.short), n.long) {
				on[n.long] = true
				return true
			}
		}
		if rest, ok := strings.CutPrefix(o.long, "no-"); ok {
			for _, n := range opts {
				if isLongPrefix(rest, n.long, 1) {
					on[n.long] = false
				}
			}
		}
		return true
	})
	for _, v := range on {
		if v {
			return true
		}
	}
	return false
}

// gitOptName names one option by its short letter (0 for none) and long name.
type gitOptName struct {
	short rune
	long  string
}

// isEndOfOptions reports whether a ends git's options: "--", or
// "--end-of-options", which git treats the same way.
func isEndOfOptions(a string) bool {
	return a == "--" || a == "--end-of-options"
}

// isLongPrefix reports whether name is an abbreviation git would expand to
// full: a prefix of it at least minLen characters long.
func isLongPrefix(name, full string, minLen int) bool {
	return len(name) >= minLen && strings.HasPrefix(full, name)
}

// The grammars of the subcommands whose tier depends on their arguments, from
// `git <subcommand> -h` (git 2.55).
var (
	switchGrammar = gitOptionGrammar{
		valueLong:  []string{"create", "force-create", "conflict", "orphan"},
		valueShort: "cC",
	}
	restoreGrammar = gitOptionGrammar{
		valueLong:  []string{"source", "conflict", "unified", "inter-hunk-context", "pathspec-from-file"},
		valueShort: "sU",
	}
	checkoutGrammar = gitOptionGrammar{
		valueLong:  []string{"conflict", "orphan", "unified", "inter-hunk-context", "pathspec-from-file"},
		valueShort: "bBU",
	}
	branchGrammar = gitOptionGrammar{
		valueLong: []string{
			"set-upstream-to", "contains", "no-contains", "merged", "no-merged",
			"sort", "points-at", "format",
		},
		valueShort: "u",
	}
	tagGrammar = gitOptionGrammar{
		valueLong: []string{
			"message", "file", "trailer", "cleanup", "local-user", "contains", "no-contains",
			"merged", "no-merged", "sort", "points-at", "format",
		},
		valueShort:    "mFu",
		optionalShort: "n",
	}
	// mergeGrammar: --log and --gpg-sign/-S take an optional value, joined only.
	mergeGrammar = gitOptionGrammar{
		valueLong:     []string{"cleanup", "strategy", "strategy-option", "message", "file", "into-name"},
		valueShort:    "msXF",
		optionalShort: "S",
	}
)
