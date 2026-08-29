package tools

import (
	"strings"
	"testing"
)

// git_classify_fuzz_test.go — argument-invariance for classifyGit's flat arm,
// with the argument vectors GENERATED rather than listed.
//
// gitArgBattery (git_classify_invariance_test.go) samples the forms a person
// thought to write down. That is a good regression guard and a weak property.
// Its 64 vectors have an argument-length histogram of {0:2, 1:51, 2:10, 7:1} —
// lengths 3 to 6 and 8-and-up never occur at all — and its flag universe is a
// list someone wrote. Two mutants sat in exactly those holes and survived the
// entire internal/tools suite with no failing subtest:
//
//   - `if len(args) == 3 { return tierRead }` in the flat arm. Length three is
//     not exotic: `git cherry-pick -n -x <rev>` and `git clean -f -d .` are it.
//   - an arm keyed on `--rerere-autoupdate`, `--cleanup=verbatim` or
//     `--empty=drop` — three flags git-cherry-pick(1) documents and the battery
//     happens not to name.
//
// Both fail here. The seed corpus is BUILT, not written: every token in
// gitArgTokens at every length from 0 to gitArgSeedMaxLen, in two window
// families, so neither a length nor a vocabulary entry can go missing the way
// the battery's did. Adding a token covers it at every length for free, which is
// the part a hand-written table cannot do.
//
// What that guarantees is exact, and worth stating as a bound rather than as
// "every form": invariance across everything gitArgTokens can spell at those
// lengths under plain `make test`, plus whatever a `make fuzz` run reaches
// beyond it. The vocabulary is still a list — the fuzzer's freeform half is what
// escapes it, mutating raw NUL-separated tokens no list contains.
//
// The stake, and why this is the highest-value invariant in the file: tierRead
// is the tier that skips gateGit (so allow_destructive and confirm are never
// consulted), the write-rate limiter, beginSerialisedGit (the per-repo lock, the
// stale index.lock reaper, the write-drain gate) and armRefGuard (so
// expected_head and the cross-session ref-movement guard never arm). One
// arg-keyed arm would hand all of that to a verb that rewrites history.

// gitArgTokens is the vocabulary the seed corpus is assembled from: real
// arguments of the verbs classifyGit flat-classifies, plus the structural shapes
// an argument can take (=-joined flag, revision, range, `--` separator, bare
// `-`, empty string), plus tokens the ARG-DEPENDENT arms key on.
//
// The cross-verb nonsense at the end is deliberate. classifyGit parses no
// per-verb grammar, so the property has to hold for `git reset --staged list`
// exactly as firmly as for `git reset --hard`. A future arm that keys on one of
// those tokens is precisely what it catches.
var gitArgTokens = []string{
	// Sequencer state — cherry-pick, revert, rebase.
	"--continue", "--abort", "--skip", "--quit",
	// git-cherry-pick(1) and git-revert(1), in full.
	"-e", "--edit", "--no-edit", "-x", "-r", "-n", "--no-commit",
	"-s", "--signoff", "-S", "--gpg-sign", "-m", "--mainline", "1",
	"--ff", "--allow-empty", "--allow-empty-message", "--keep-redundant-commits",
	"--empty=drop", "--empty=keep", "--empty=stop",
	"--cleanup=verbatim", "--cleanup=scissors",
	"--rerere-autoupdate", "--no-rerere-autoupdate",
	"--strategy=recursive", "--strategy", "ours", "--strategy-option=theirs",
	"-X", "theirs", "--reference",
	// git-rebase(1).
	"-i", "--interactive", "--onto", "--root", "--autostash", "--no-autostash",
	"--autosquash", "--rebase-merges", "--update-refs", "--keep-base",
	"--fork-point", "--exec", "--committer-date-is-author-date",
	// git-reset(1).
	"--soft", "--mixed", "--hard", "--merge", "--keep", "-q",
	"--pathspec-from-file=paths.txt",
	// git-clean(1).
	"-f", "-fd", "-fdx", "-d", "--exclude=*.log", "--dry-run", "--quiet",
	// The network verbs.
	"--force", "--force-with-lease", "--tags", "--prune", "--set-upstream",
	"-u", "--depth=1", "--ff-only", "--rebase", "--all",
	// The read verbs.
	"--oneline", "-p", "--stat", "--name-only", "--graph", "-10",
	"--since=2026-01-01", "-w", "--porcelain", "-v",
	// add, commit, mv.
	"-A", "--amend", "--no-verify", "-k", "--message", "commit message",
	// Revisions, ranges, pathspecs, separators, and the empty string.
	"HEAD", "HEAD~1", "abc1234", "A..B", "abc1234^..def5678",
	"origin/main", "main", "v1.0.0", "--", "", "file.go", "path/to/dir",
	":/", "*.log", "-", "=", "--=", "-x=y",
	// Tokens the argument-DEPENDENT arms key on — nonsense for a flat verb, and
	// that is the point.
	"--staged", "--worktree", "-W", "-D", "--delete", "--list", "-l",
	"-b", "-B", "--copy", "--move", "--discard-changes",
	"list", "drop", "pop", "clear",
	"--show-current", "--merged", "--no-merged", "--contains", "-a",
	"--remotes", "-vv",
}

const (
	// gitArgVectorMax bounds a decoded vector so a fuzzer-supplied selector
	// cannot turn one case into an unbounded allocation.
	gitArgVectorMax = 16
	// gitArgSeedMaxLen is the longest vector the generated seed corpus builds.
	// Real git calls do not run long, and every length up to it is covered.
	gitArgSeedMaxLen = 10
)

// gitArgVector decodes a fuzz input into an argument vector. The selector bytes
// index gitArgTokens, which is what lets the fuzzer reach realistic argv quickly;
// raw contributes NUL-separated tokens verbatim, which is what lets it leave the
// vocabulary behind and try a flag nobody listed.
func gitArgVector(sel []byte, raw string) []string {
	args := make([]string, 0, len(sel))
	for _, b := range sel {
		if len(args) == gitArgVectorMax {
			return args
		}
		args = append(args, gitArgTokens[int(b)%len(gitArgTokens)])
	}
	if raw == "" {
		return args
	}
	for tok := range strings.SplitSeq(raw, "\x00") {
		if len(args) == gitArgVectorMax {
			break
		}
		args = append(args, tok)
	}
	return args
}

// seedGitArgVectors builds the seed corpus: every gitArgTokens entry at every
// length from 0 to gitArgSeedMaxLen.
//
// Two window families, both cyclic over the vocabulary. Stride 1 takes
// consecutive runs, which covers every (token, position, length) triple. Stride
// 7 skips, which pairs tokens the consecutive runs never place together — the
// consecutive family alone could miss an arm keyed on a CONJUNCTION of two
// far-apart tokens. Pairwise coverage is still partial by construction; going
// past it is the fuzzer's job, not the seed corpus's.
func seedGitArgVectors(f *testing.F) {
	f.Helper()
	f.Add([]byte(nil), "")
	n := len(gitArgTokens)
	for _, stride := range []int{1, 7} {
		for length := 1; length <= gitArgSeedMaxLen; length++ {
			if length == 1 && stride != 1 {
				continue // every stride agrees at length one
			}
			for offset := range n {
				sel := make([]byte, length)
				for i := range sel {
					sel[i] = byte((offset + i*stride) % n)
				}
				f.Add(sel, "")
			}
		}
	}
	// Freeform seeds, so the raw half is exercised from the first run rather
	// than only once the fuzzer discovers it.
	f.Add([]byte(nil), "--some-flag-nobody-listed")
	f.Add([]byte(nil), "\x00\x00")
	f.Add([]byte{0, 1, 2}, "--another\x00")
}

// FuzzClassifyGitArgInvariance asserts the property gitArgIndependent's name
// claims: for a subcommand classifyGit decides without consulting args, the tier
// it returns is the tier it returns for nil, whatever the arguments are.
//
// The seed corpus (seedGitArgVectors) is exhaustive over gitArgTokens x lengths
// 0..gitArgSeedMaxLen and runs under plain `make test`; `make fuzz` explores past
// the vocabulary via the raw half. Any input a fuzz run does surface belongs in
// testdata/fuzz/FuzzClassifyGitArgInvariance/ under a name that says what it is,
// where plain `make test` will keep running it.
//
// Two entries are already there, and they are mutant-regression pins rather than
// fuzz finds — no run has surfaced a failure, the classifier being correct. They
// carry the two vectors the surviving mutants died on, spelled as raw tokens
// rather than vocabulary indices, so an edit to gitArgTokens or to the seed
// generator cannot quietly stop covering them.
func FuzzClassifyGitArgInvariance(f *testing.F) {
	if n := len(gitArgTokens); n > 256 {
		f.Fatalf("gitArgTokens has %d entries; a seed selector byte can only address 256, so the "+
			"generated corpus would silently stop covering the tail", n)
	}
	seedGitArgVectors(f)

	f.Fuzz(func(t *testing.T, sel []byte, raw string) {
		args := gitArgVector(sel, raw)
		for _, sub := range gitArgIndependent {
			base := classifyGit(sub, nil)
			if got := classifyGit(sub, args); got != base {
				t.Errorf("classifyGit(%q, %q) = %s, but classifyGit(%q, nil) = %s — %q is pinned as "+
					"argument-INDEPENDENT, so an argument that moves its tier is a policy hole, not a "+
					"refinement (a demotion to read skips the gate, the rate limiter, the per-repo lock "+
					"and the ref guard)",
					sub, args, gitTierNames[got], sub, gitTierNames[base], sub)
			}
		}
	})
}
