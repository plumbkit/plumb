package arch

// This file declares the shared primitives that must have exactly one
// implementation, so "don't hand-roll this again" is a failing test rather than
// a thing reviewers have to notice.
//
// The rules exist because the alternative was measured. A 2026-06 audit found
// an atomic temp→rename write reimplemented five times and a byte formatter
// three times; consolidating them was filed, partly done, and by the time the
// remaining items were picked up the atomic write had regrown to twelve copies
// and the byte formatter to five. Nothing had failed in between. Extraction on
// its own does not hold — the tree has several agents editing it, a helper that
// is easier to retype than to find will be retyped, and the copy is invisible
// in review because it compiles and passes.
//
// Each rule names the one package allowed to own the primitive, plus an
// explicit allowlist of exceptions with the reason each is genuinely different.
// Adding an entry is deliberately a code edit with a written justification, not
// a silent regrowth.
//
// Concurrency: read-only package-level data, safe for concurrent use.

// PrimitiveRule declares a family of helper functions that belongs in exactly
// one package.
//
// A function matches the rule when its (lowercased) name carries one of
// Prefixes or equals one of Exact. Matching declarations outside Home fail
// TestSharedPrimitives unless listed in Allowed.
type PrimitiveRule struct {
	// Primitive names what is being protected, for the failure message.
	Primitive string

	// Home is the package (relative to the module root) that owns the single
	// implementation.
	Home string

	// Prefixes match any function whose lowercased name starts with one of
	// these.
	Prefixes []string

	// Exact matches a function whose lowercased name equals one of these. Used
	// where a prefix would be too broad — "clamp" alone catches clampWidth,
	// clampPercent and a dozen other numeric clamps that have nothing to do
	// with string budgets.
	Exact []string

	// Allowed maps "<package>.<func>" to the reason that declaration is
	// legitimately not a duplicate. Every entry must say why, not just that.
	Allowed map[string]string
}

// PrimitiveRules is the full set. Keep the reasons current: an allowlist entry
// whose justification has stopped being true is worse than no rule at all.
var PrimitiveRules = []PrimitiveRule{
	{
		Primitive: "string truncation",
		Home:      "internal/textfmt",
		Prefixes:  []string{"truncate"},
		Exact:     []string{"clampbytes", "clamptobytes", "ellipsis"},
		Allowed: map[string]string{
			"internal/tools.truncateLines":          "caps output by LINE count with a caller-supplied suffix — a different budget from textfmt's rune and byte helpers, and it never splits a line",
			"internal/tui.contractPathTruncateLeft": "path abbreviation: keeps the RIGHTMOST runes and prefixes the ellipsis, the opposite end from textfmt.Ellipsis",
			"cmd/clientsmoke.truncate":              "operates on []byte for harness log output, in a build-tagged test-only package that ships in no binary",
		},
	},
	{
		Primitive: "pluralisation",
		Home:      "internal/textfmt",
		Prefixes:  []string{"plural"},
		Allowed: map[string]string{
			"internal/cli.pluralSessionCount": "formats a whole clause including the count and a verb (\"1 session is\" / \"3 sessions are\"), not a bare word choice",
			"internal/cli.pluralisedFiles":    "language-aware: picks the noun by the detected language as well as the count",
			"internal/tools.pluralMemories":   "interpolates the count and switches the surrounding sentence, not just the noun",
		},
	},
	{
		// Added for issue #273, after the tree grew a SECOND path canonicaliser
		// that disagreed with the first on the two cases each was written for:
		// one anchored a relative path to the daemon's working directory (the
		// silent cross-repository write of #181) and Cleaned ".." lexically
		// before resolving symlinks (the check-and-syscall divergence of #264),
		// while the other refuses both. Neither said it was one of two, so the
		// next caller would pick whichever it found first. The sweep that came
		// with the rule found FIVE declarations matching, not the two the issue
		// named — which is the argument for the rule rather than for another
		// round of manual consolidation.
		//
		// "Same place?" is one question and gets one answer: paths.Canonical.
		// Anchoring a relative path is a SEPARATE decision each caller must make
		// visibly, above the canonicaliser — and so is refusing an unresolved
		// "..", which is an authorisation check, not an identity function.
		//
		// Every entry below was written against the code on main at the commit that
		// adds it, and that ordering was ENFORCED rather than promised. While the two
		// duplicates this rule exists to remove were still present
		// (internal/tools.canonicalPathForBoundary, folded by #284, and
		// internal/cli.canonicalXcodeRoot, folded by #282) neither was allowlisted,
		// so TestSharedPrimitives stayed RED and named them.
		//
		// An earlier draft did allowlist the second one with a reason describing its
		// post-fold body. The tests passed on a tree where the function was still
		// Abs+Clean, while the entry asserted in the past tense that a live defect
		// had been fixed. Nothing in this file's machinery can detect that: a failing
		// test can be read, a green lie cannot. Never write an entry for code that is
		// not on main yet, however certain the fold is.
		Primitive: "path canonicalisation",
		Home:      "internal/paths",
		Prefixes:  []string{"canonical"},
		Allowed: map[string]string{
			"internal/tools.canonicalRoot":        "the boundary seam's adapter: decodes a file:// URI, a spelling paths.Canonical deliberately does not know because it is also called with plain paths from inside the daemon. Every resolution step is delegated",
			"internal/tools.canonicalAddPaths":    "not a canonicaliser: it builds the git ls-files pathspecs and the absolute forms to match them against, and calls canonicalRoot for the resolution itself",
			"internal/fsguard.canonical":          "anchors a relative walk root to the process cwd BEFORE delegating. RefuseWalk matches protected directories by exact path, so an unanchored \".\" would match none and fail OPEN exactly when the cwd IS $HOME — the opposite direction from the boundary policy, where anchoring is the hazard. Resolution is delegated",
			"internal/cli.canonicalXcodeRoot":     "the same anchor-then-delegate division as internal/fsguard.canonical: filepath.Abs anchors a possibly-relative root so the \"one build-server flow per root\" singleflight key is stable, then paths.Canonical resolves it. Before that fold it was Abs+Clean with NO symlink resolution, so two spellings of one project each started their own concurrent xcodebuild — four runner calls where one flow makes two",
			"internal/config.canonicalTaskHash":   "canonicalises a []TaskCommandSpec into a stable byte sequence for hashing — a struct-encoding question with no filesystem in it",
			"internal/config.canonicalPolicyHash": "as canonicalTaskHash, for ProjectPolicySpec",
			"internal/mcp.canonicalFor":           "resolves an argument ALIAS to its canonical JSON key; no path ever enters it",
		},
	},
	{
		Primitive: "byte-size formatting",
		Home:      "internal/textfmt",
		Exact: []string{
			"formatbytes", "humanbytes", "humanbytescompact",
			"formatsize", "bytesize", "bytesizelabel",
		},
		Allowed: map[string]string{},
	},
}

// CallRule pins a standard-library call that must not be scattered across the
// tree, because the correct way to use it is several lines long and getting it
// subtly wrong is invisible.
//
// Where PrimitiveRule catches a helper being re-declared, this catches the
// pattern being open-coded without a helper at all — which is how the atomic
// write spread in the first place: nobody wrote a second AtomicWrite, they
// wrote a fifth CreateTemp/Write/Rename sequence inline.
type CallRule struct {
	// Call is the qualified selector as written in source, e.g. "os.Rename".
	Call string

	// Why explains, in the failure message, what to use instead.
	Why string

	// Allowed maps "<package>.<func>" to the reason that function may make the
	// call directly. Methods are keyed by their name alone ("Execute"), since
	// that is what the AST gives without type resolution.
	Allowed map[string]string
}

// CallRules is the full set.
//
// These are checked against production files only. A test that hand-builds a
// deliberately old-schema database, or stages a torn file to prove a recovery
// path, is doing the forbidden thing on purpose.
var CallRules = []CallRule{
	{
		Call: "sql.Open",
		Why:  "a SQLite handle belongs to sqlitex.Open / sqlitex.OpenReadOnly, which build the DSN as a file: URI with the `_pragma=` spelling — the modernc driver silently ignores both the mattn-style `_busy_timeout=` form AND `mode=ro` on a bare path, so a hand-written DSN can leave busy_timeout at 0 and a 'read-only' handle writable",
		Allowed: map[string]string{
			"internal/sqlitex.Open":         "the shared implementation itself",
			"internal/sqlitex.OpenReadOnly": "the shared read-only implementation itself",
		},
	},
	{
		Call: "os.Rename",
		Why:  "a staged temp→rename write belongs in fsync.AtomicWrite, which fsyncs the staging file before the rename and the directory after it, preserves an existing file's mode, and cleans up on every failure path",
		Allowed: map[string]string{
			"internal/fsync.AtomicWriteFunc":  "the shared implementation itself",
			"internal/tools.safeWrite":        "a separate primitive: stages in os.TempDir with a memoised cross-device verdict, resolves symlinks so the rename writes THROUGH a link, and returns the mtime/timing that edit_file's concurrent-write detection needs",
			"internal/tools.safeWriteSibling": "safeWrite's EXDEV fallback, staging next to the target when os.TempDir is on another filesystem",
			"internal/tools.Execute":          "rename_file: a user-facing move of a real file, not a staged write",
		},
	},
}

// DelegationRule pins a resource named by a string literal to a single
// implementation: any production function whose body contains a string
// literal exactly equal to Literal, outside Home, must also call Delegate —
// or carry an Allowed entry saying why it legitimately does not (for
// instance, it only ever reads the resource).
//
// Where PrimitiveRule catches a helper re-declared under a family of names,
// and CallRule catches a stdlib call open-coded with no helper at all, this
// catches a file-format owner being bypassed: a hand-rolled copy of the
// .gitignore appender shares no name and no distinctive stdlib call with the
// original, but it cannot avoid naming the file. A literal matches when it
// equals Literal after unquoting, or ends in "/"+Literal (the whole relative
// path spelled as one literal) — never on a bare substring, because
// ".gitignore" appears inside prose in tool descriptions, error wraps and
// log messages, none of which touch the file.
type DelegationRule struct {
	// Resource names what is being protected, for the failure message.
	Resource string

	// Literal is the exact (unquoted) string-literal value that marks a
	// function as touching the resource.
	Literal string

	// Home is the package (relative to the module root) that owns the single
	// implementation. Functions in Home are exempt.
	Home string

	// Delegate is the required call in display form, e.g.
	// "paths.EnsureGitignoreEntries". The checker matches the function name
	// after the last dot, qualified by the file's actual local import name
	// for Home — an aliased or dot import still counts as delegation.
	Delegate string

	// Why explains, in the failure message, what to use instead.
	Why string

	// Allowed maps "<package>.<func>" to the reason that site legitimately
	// touches the literal without delegating. Methods are keyed by their name
	// alone, as elsewhere; a package-level const or var carrying the literal
	// — including a func-literal var, which has no enclosing function to
	// delegate from — is keyed by its identifier and can only be excused
	// here. Every entry must say why, not just that.
	Allowed map[string]string
}

// DelegationRules is the full set.
//
// Like CallRules, these are checked against production files only: a test
// that stages a .gitignore fixture is touching the file on purpose.
var DelegationRules = []DelegationRule{
	{
		Resource: ".gitignore writing",
		Literal:  ".gitignore",
		Home:     "internal/paths",
		Delegate: "paths.EnsureGitignoreEntries",
		Why:      "appending to a .gitignore belongs in paths.EnsureGitignoreEntries, which matches entries by exact trimmed line — one hand-rolled copy used a substring match instead, so an entry appearing inside any other line (a comment, a longer path) was silently treated as already present and never appended",
		Allowed: map[string]string{
			"internal/arch.DelegationRules": "the rule's own declaration: a DelegationRule must spell the literal it pins, which the package-level-literal check then sees",
			"internal/tools.load":           "ignoreStack.load READS .gitignore and .ignore to honour ignore rules during directory walks; it never writes the file, so there is nothing to delegate",
		},
	},
}
