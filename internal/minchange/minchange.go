// Package minchange is a deterministic, advisory diff-review engine. Given a
// parsed unified diff (and a few injected, read-only topology queries) it flags
// places where a change may have been over-built: a single-use abstraction, a
// thin forwarding wrapper, a new dependency with a well-known stdlib
// equivalent, a possible duplicate helper, or a logic change with no
// accompanying test change.
//
// Design commitments:
//   - Advisory only. Findings never block a write; they are hints for a human
//     or an agent to weigh, not gates.
//   - Asymmetric evidence. A noisy reviewer is ignored, so a check stays silent
//     unless it can point at concrete evidence and, where defensible, a smaller
//     alternative. When the evidence is approximate (topology-derived) the
//     finding says so via its Confidence label rather than overstating.
//   - Pure and injectable. This package imports nothing project-local beyond the
//     leaf tokenise-free path: the topology lookups it needs are passed in as
//     function values (Deps), so the whole engine is trivially unit-testable
//     with fakes and carries no dependency on the running daemon.
//
// The engine is language-aware only where it must be: the abstraction checks
// (single-use, thin wrapper, duplicate helper) understand Go declaration
// syntax; the dependency and verification-gap checks are language-agnostic over
// the diff's file set.
package minchange

import (
	"context"
	"sort"
)

// Severity is the advisory weight of a finding. Only two levels exist on
// purpose: info ("worth a glance") and warning ("worth a second look"). Neither
// blocks anything.
type Severity string

const (
	// Info is the softer level — a suggestion the author may already have a
	// reason for.
	Info Severity = "info"
	// Warning is the stronger level, reserved for findings proven from the diff
	// text itself (a passthrough wrapper, a missing test change).
	Warning Severity = "warning"
)

// Confidence is an honesty label separating findings proven from the diff text
// (High) from findings that rest on the approximate topology index (Low). It is
// distinct from Severity: a Warning is always High, but an Info may be either.
type Confidence string

const (
	// High means the finding is provable from the diff text alone.
	High Confidence = "high"
	// Low means the finding leans on the topology index, which is approximate
	// (its call graph is intra-file, and it may be a few edits stale).
	Low Confidence = "low"
)

// Finding kinds. Stable identifiers so a consumer can filter or suppress by
// kind.
const (
	KindSingleUse       = "single-use-abstraction"
	KindThinWrapper     = "thin-wrapper"
	KindStdlibCandidate = "stdlib-candidate"
	KindDuplicateHelper = "duplicate-helper"
	KindVerificationGap = "verification-gap"
)

// Finding is one advisory observation about the reviewed diff.
type Finding struct {
	Severity   Severity
	Kind       string
	Confidence Confidence
	// File is the new-side path the finding is about; empty for a whole-diff
	// finding (e.g. the aggregate verification gap).
	File string
	// Line is the 1-based new-side line, or 0 when not tied to a single line.
	Line int
	// Rationale is a one-line explanation of why this may be over-building.
	Rationale string
	// Evidence is the concrete proof: the wrapper signature, the added require
	// line, the resembling symbol's location, etc. Never empty.
	Evidence string
	// Alternative is a concrete smaller option, when one is defensible; empty
	// otherwise. Suppressed when Options.IncludeSuggestions is false.
	Alternative string
}

// SymbolRef locates a symbol the injected topology queries returned.
type SymbolRef struct {
	Name string
	Path string
	Line int
	Kind string
}

// Deps are the injected, read-only topology queries used by the approximate
// checks. Any field may be nil; a nil field disables its check and is recorded
// in the report's NotChecked list. Keeping these as function values is what
// lets this package stay pure (no topology import) and unit-test with fakes.
type Deps struct {
	// CallerCount returns an APPROXIMATE caller count for a defined symbol and,
	// when the count is exactly one, that single call site. found is false when
	// the symbol is absent from the index. The count is intra-file only (the
	// topology call graph does not cross files), so it is a lower bound —
	// callers use it for a Low-confidence hint, never a claim.
	CallerCount func(ctx context.Context, name string) (count int, site SymbolRef, found bool)

	// SimilarSymbols returns indexed free functions whose tokenised name closely
	// matches name, excluding excludeFile (the symbol's own file). Used to flag a
	// possible duplicate helper. Returns nil when there is no close match.
	SimilarSymbols func(ctx context.Context, name, excludeFile string) []SymbolRef
}

// Options tune the analysis and its output bound.
type Options struct {
	// MaxFindings caps the returned findings; excess is dropped and Truncated is
	// set. Zero means the package default.
	MaxFindings int
	// IncludeSuggestions controls whether Finding.Alternative is populated.
	IncludeSuggestions bool
	// ScopedToFiles is true when the caller restricted the diff to an explicit
	// file set; when false the report notes it reviewed the whole working-tree
	// diff (which may include unrelated peer-agent edits).
	ScopedToFiles bool
}

// Report is the structured result of Analyze.
type Report struct {
	// Findings, ordered warnings-first then by file and line.
	Findings []Finding
	// NotChecked lists the analyses that did not run and why, so a consumer
	// knows the review's blind spots rather than reading silence as a clean
	// bill of health.
	NotChecked []string
	// Truncated is true when MaxFindings dropped some findings.
	Truncated bool
	// FilesReviewed is the number of file diffs considered.
	FilesReviewed int
}

const (
	defaultMaxFindings = 20
	maxMaxFindings     = 100
)

// Analyze runs every check over diff and returns a bounded, ordered report. It
// is a thin orchestrator: each check is an independently-testable function.
func Analyze(ctx context.Context, diff *Diff, deps Deps, opts Options) Report {
	if opts.MaxFindings <= 0 {
		opts.MaxFindings = defaultMaxFindings
	}
	if opts.MaxFindings > maxMaxFindings {
		opts.MaxFindings = maxMaxFindings
	}

	r := Report{}
	if diff == nil {
		r.NotChecked = notCheckedList(nil, deps, opts)
		return r
	}
	r.FilesReviewed = len(diff.Files)

	added := collectAddedSymbols(diff)

	var findings []Finding
	findings = append(findings, thinWrapperFindings(added, opts)...)
	findings = append(findings, singleUseFindings(ctx, added, deps, opts)...)
	findings = append(findings, duplicateHelperFindings(ctx, added, deps, opts)...)
	findings = append(findings, dependencyFindings(diff, opts)...)
	findings = append(findings, verificationGapFindings(diff, opts)...)

	if !opts.IncludeSuggestions {
		for i := range findings {
			findings[i].Alternative = ""
		}
	}

	sortFindings(findings)
	if len(findings) > opts.MaxFindings {
		findings = findings[:opts.MaxFindings]
		r.Truncated = true
	}
	r.Findings = findings
	r.NotChecked = notCheckedList(diff, deps, opts)
	return r
}

// sortFindings orders warnings before infos, then by file, then by line, then
// by kind — a stable order so identical input yields identical output.
func sortFindings(f []Finding) {
	sort.SliceStable(f, func(i, j int) bool {
		if f[i].Severity != f[j].Severity {
			return f[i].Severity == Warning // Warning sorts first
		}
		if f[i].File != f[j].File {
			return f[i].File < f[j].File
		}
		if f[i].Line != f[j].Line {
			return f[i].Line < f[j].Line
		}
		return f[i].Kind < f[j].Kind
	})
}

// notCheckedList records the analyses that could not run, so the review's blind
// spots are explicit.
func notCheckedList(diff *Diff, deps Deps, opts Options) []string {
	var out []string
	if deps.CallerCount == nil {
		out = append(out, "single-use-abstraction: topology index unavailable — call-site counts not checked")
	} else {
		out = append(out, "single-use-abstraction: uses the topology call graph, which is intra-file and may be a few edits stale — cross-file/cross-package callers and dynamic dispatch are not counted (verify with find_references)")
	}
	if deps.SimilarSymbols == nil {
		out = append(out, "duplicate-helper: topology index unavailable — name-similarity not checked")
	}
	out = append(out, "stdlib-candidate: flags only a curated set of modules with a well-known stdlib equivalent; it does not analyse how the dependency is actually used")
	if !opts.ScopedToFiles {
		out = append(out, "scope: reviewed the entire working-tree diff — pass `files` to scope the review to your change and exclude unrelated peer-agent edits")
	}
	if diff != nil {
		for _, f := range diff.Files {
			if f.IsBinary {
				out = append(out, "binary files were skipped (no textual diff to analyse)")
				break
			}
		}
	}
	return out
}
