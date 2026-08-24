// Package topology maintains a persistent, disk-based semantic index of the
// workspace codebase using SQLite + FTS5.
package topology

import "time"

// NodeKind is the type of a semantic node in the topology graph.
type NodeKind string

const (
	KindFile     NodeKind = "file"
	KindPackage  NodeKind = "package"
	KindFunction NodeKind = "function"
	KindMethod   NodeKind = "method"
	KindType     NodeKind = "type"
	KindConstant NodeKind = "constant"
	KindVariable NodeKind = "variable"
	KindImport   NodeKind = "import"
	KindClass    NodeKind = "class"
	KindTest     NodeKind = "test"
	// KindField is a key/column of a data-format file: a SQL column, a TOML or
	// YAML key. Used by the config/markup tree-sitter extractors. NOTE: a member
	// field/property of a *code* type (struct field, class property) is NOT a
	// KindField — it is a KindConstant (when declared immutable) or KindVariable,
	// per the documented extractor conventions.
	KindField NodeKind = "field"
	// KindSection is a document heading (a Markdown section). Used by the
	// markup tree-sitter extractors for navigable document outlines.
	KindSection NodeKind = "section"
)

// EdgeKind is the type of a relationship between two nodes.
type EdgeKind string

const (
	EdgeCalls      EdgeKind = "calls"
	EdgeImports    EdgeKind = "imports"
	EdgeContains   EdgeKind = "contains"
	EdgeDefines    EdgeKind = "defines"
	EdgeInherits   EdgeKind = "inherits"
	EdgeImplements EdgeKind = "implements"
)

// Node is a semantic entity in the topology graph.
//
// Span fields and the absent-span convention: StartLine/EndLine are the 1-based
// line range (as they have always been). StartByte/EndByte and StartCol/EndCol
// are the byte-precise (0-based offset) and column (0-based) span of the same
// declaration. Because byte 0 is a legitimate offset, presence is signalled by
// HasBytes — NOT by a zero sentinel: when HasBytes is false the byte/column
// fields are meaningless and consumers must fall back to the line range. An
// extractor that knows the byte span sets HasBytes=true and the four offset
// fields together.
//
// DocStartByte/DocEndByte are the optional byte span of the declaration's doc
// comment. Their convention is self-describing: a doc span is present only when
// DocEndByte > DocStartByte; DocStartByte==DocEndByte (the 0/0 zero value
// included) means "no doc span". This holds regardless of HasBytes.
type Node struct {
	ID        int64
	FileID    int64
	Kind      NodeKind
	Name      string
	Qualified string
	Signature string
	StartLine int
	EndLine   int
	Docstring string
	Language  string
	Path      string // workspace-relative

	// Byte-precise declaration span. Valid only when HasBytes is true.
	HasBytes  bool
	StartByte int
	EndByte   int
	StartCol  int // 0-based column of StartByte
	EndCol    int // 0-based column of EndByte

	// Optional doc-comment byte span. Present only when DocEndByte > DocStartByte.
	DocStartByte int
	DocEndByte   int
}

// HasDocSpan reports whether the node carries a byte-precise doc-comment span.
func (n Node) HasDocSpan() bool { return n.DocEndByte > n.DocStartByte }

// Edge is a directed relationship between two nodes.
type Edge struct {
	ID         int64
	FromID     int64
	ToID       int64
	Kind       EdgeKind
	Confidence float64
	Source     string
}

// SearchResult is one ranked hit from a topology FTS5 search.
type SearchResult struct {
	Node    Node
	Score   float64
	Field   string
	Snippet string
}

// SearchOpts controls the behaviour of a topology search query.
type SearchOpts struct {
	Kinds    []string
	Language string
	Limit    int
	Snippets bool
}

// Direction controls which edges a directed BFS traversal follows.
type Direction int

const (
	// DirectionBoth follows edges in both directions (default, undirected).
	DirectionBoth Direction = 0
	// DirectionOutward follows edges from the frontier (from_id → to_id).
	DirectionOutward Direction = 1
	// DirectionInward follows edges toward the frontier (to_id → from_id).
	DirectionInward Direction = 2
)

// ExploreOpts controls the bounded BFS neighbourhood expansion.
type ExploreOpts struct {
	Depth         int
	MaxNodes      int
	MaxBytes      int
	IncludeSource string // none | signatures | snippets | full
	EdgeKinds     []string
	Direction     Direction // defaults to DirectionBoth
	// IncludeDerivedCalls admits the cross-file `calls` edges the call resolver
	// derives (`source = "call-resolver"`). It defaults to FALSE, so a caller
	// asking for `calls` edges gets the extractor's intra-file edges and nothing
	// else — consumers remain excluded by default for the deliberate step-6
	// rollout, which is enforced in code rather than left to prose.
	//
	// The exclusion is by SOURCE, not by kind: the derived edges carry
	// kind = "calls" exactly like the extractor's, so a kind filter cannot
	// separate them and every consumer asking for `calls` would receive them
	// silently. Their lifecycle is durable across incremental re-indexes, but each
	// consumer remains excluded until it is onboarded deliberately with a measured
	// before/after in step 6. This flag is the switch that rollout flips, one
	// consumer at a time.
	IncludeDerivedCalls bool
}

// ImpactOpts controls the bidirectional BFS used by topology_impact.
type ImpactOpts struct {
	Depth     int
	MaxNodes  int
	MaxBytes  int
	EdgeKinds []string
}

// ImpactResult is the result of a bidirectional BFS around a centre node.
type ImpactResult struct {
	Centre       Node
	DependsOn    *Neighbourhood // outward: what centre depends on
	DependedOnBy *Neighbourhood // inward: what depends on centre
}

// Neighbourhood is the result of a BFS exploration around a centre node.
type Neighbourhood struct {
	Centre    Node
	Nodes     []Node
	Edges     []Edge
	Truncated bool
}

// FileError is one file that failed to index and the reason recorded at the
// time (parse timeout, extractor panic, unreadable file, …). Status carries a
// bounded sample so a skipped file says WHY it was skipped rather than only
// incrementing a counter.
type FileError struct {
	Path    string
	Message string
}

// Status is a snapshot of the topology index health.
//
// The three file counts are disjoint by construction: IndexedFiles were parsed,
// SkippedFiles failed, and the remainder were never parsed at all — split into
// UncoveredFiles (a language plumb recognises but has no extractor for) and
// UnrecognisedFiles (binaries, lockfiles, anything the registry does not name).
// Keeping the last group visible is the point: an agent reading an empty Map for
// a Rails app needs to see "ruby (683), not covered" rather than infer absence.
type Status struct {
	IndexedFiles int
	SkippedFiles int
	EmptyFiles   int // parsed successfully but yielded zero nodes (comment-only, or a genuinely empty file)
	// UncoveredFiles counts recognised-but-unextractable files per language —
	// the coverage gap, and the roadmap for which extractor to write next.
	UncoveredFiles map[string]int
	// UnrecognisedFiles counts files the registry does not name at all.
	UnrecognisedFiles int
	TotalNodes        int
	TotalEdges        int
	DBSizeBytes       int64
	LastSync          time.Time
	IndexerState      string
	Languages         []string
	LastError         string
	// FileErrors is a bounded sample (most recently touched first) of the
	// files counted by SkippedFiles, capped at maxStatusFileErrors.
	FileErrors []FileError
	// CallGraph reports what the cross-file call resolver reached.
	CallGraph CallGraphStatus
}

type opKind int

const (
	opUpsert opKind = iota
	opDelete
	opResync
)

type indexOp struct {
	kind opKind
	path string // workspace-relative
}

// CallSiteKind distinguishes what a recorded site actually is. Both kinds live
// in one table because both are "a name used at a position, with arguments" and
// both are resolved by the same later pass; keeping them apart as separate
// tables would duplicate the file/enclosing/position columns for no gain.
type CallSiteKind string

const (
	// CallSiteCall is a call expression: `pkg.Do(x)`, `helper()`, `mux.HandleFunc("/x", h)`.
	CallSiteCall CallSiteKind = "call"
	// CallSiteField is a composite-literal field value: the `Use: "serve"` of
	// `&cobra.Command{Use: "serve"}`. It is not a call, but it is where a Go
	// project's registration strings live, so a call-sites table that omits it
	// cannot answer the questions call sites are being captured for.
	CallSiteField CallSiteKind = "field"
)

// MaxCallSiteArgIdents caps how many identifier-shaped arguments a CallSite
// records. The cap exists so one pathological call cannot dominate the table;
// ArgCount records the true argument count so a consumer can tell a short call
// from a truncated one instead of silently reading the cap as the whole list.
const MaxCallSiteArgIdents = 8

// CallSite is one syntactic call expression (or composite-literal field value)
// recorded verbatim, BEFORE any resolution.
//
// Everything here is TEXT as it appeared in the source. That is the point: an
// extractor sees one file and cannot address a node in another, so a site that
// stored a node rowid could only ever describe an intra-file call. Storing the
// qualifier and callee as text defers resolution to a pass that can see the
// whole index — and lets an unresolvable site still be recorded and counted
// rather than dropped, which is what today's extractor does with every callee it
// cannot match (`golang/extractor.go` fileCallEdges).
type CallSite struct {
	// EnclosingIdx is the 0-based index, into the nodes slice returned alongside
	// this site, of the declaration the site sits in — a function, or the
	// package-level variable whose initialiser holds it. -1 when the site has no
	// enclosing declaration node at all.
	EnclosingIdx int
	Kind         CallSiteKind
	// Callee is the identifier being called (the selector's right half for a
	// qualified call), or the field name for a CallSiteField site.
	Callee string
	// Qualifier is the text to the left of the final dot — an imported package
	// name, a receiver variable, or a composite literal's type for a field site.
	// Empty when the call is a bare identifier.
	Qualifier string
	StartByte int
	StartLine int
	// FirstStringArg is the first string-literal argument's value (the field's
	// value, for a field site). HasStringArg distinguishes an absent literal from
	// a present empty one — `Use: ""` is a real, different fact from no `Use:`.
	FirstStringArg string
	HasStringArg   bool
	// ArgIdents are the identifier-shaped arguments, in order, capped at
	// MaxCallSiteArgIdents.
	ArgIdents []string
	// ArgCount is the number of arguments BEFORE the cap, so a truncated list is
	// detectable.
	ArgCount int
	// ArgSpread reports that the call passed a slice spread (`f(xs...)`). The
	// spread's element identifiers do not exist syntactically, so ArgIdents holds
	// the slice's name and would otherwise read as a single ordinary argument.
	ArgSpread bool
}

// CallGraphStatus is what the cross-file call resolver measured on its last
// pass. Every qualified call site falls into exactly one of the six buckets —
// Resolved, RepeatOfEdge, NoCallerNode, UnresolvedReceiver, ExternalPackage and
// UnmatchedTarget — so they sum to QualifiedSites, which is what makes "how much
// of the call graph is this" a number rather than an impression.
//
// Two of the six are not resolution outcomes and exist because they were the two
// ways a site used to leave the count: a repeat of an edge already emitted was
// skipped in silence, and a site with no caller node was folded into
// UnmatchedTarget, whose printed wording is a claim about the TARGET and untrue
// of it.
//
// All zeroes mean the pass has not run, never that the workspace has no calls.
type CallGraphStatus struct {
	// CallSites is EVERY recorded call site in an admitted language — the
	// denominator the reach percentage is published against, so that the bare
	// calls this resolver does not attempt are not quietly excluded from it.
	CallSites int
	// QualifiedSites is the subset carrying a qualifier, and equals the sum of
	// the six buckets below.
	QualifiedSites int
	// Resolved is the number of cross-file `call-resolver` edges written, which
	// is also the number of qualified sites that produced one.
	Resolved int
	// ResolvedNonTest is the subset of Resolved whose caller is not a Go test
	// file. Test callers are resolved and counted; this is the split, published
	// so a consumer that must exclude them can, and so neither number can be
	// quoted alone.
	ResolvedNonTest int
	// UnresolvedReceiver counts sites whose qualifier is not an import of their
	// own file — method calls on a receiver, which carry no type information
	// under SkipObjectResolution and are left absent rather than guessed.
	UnresolvedReceiver int
	// ExternalPackage counts sites whose qualifier IS an import, but one that
	// leaves the indexed tree (standard library, third party).
	ExternalPackage int
	// UnmatchedTarget counts sites resolving to an indexed package that declares
	// no exported top-level function of that name — a type conversion, a
	// package-level variable, or a method on one.
	UnmatchedTarget int
	// RepeatOfEdge counts sites whose caller→target edge an earlier site already
	// produced. One edge is written for the pair, so these sites resolve without
	// adding one, and counting them as Resolved would make Resolved stop being an
	// edge count.
	RepeatOfEdge int
	// NoCallerNode counts sites that resolved to a real target but have no
	// enclosing declaration to be the edge's tail. That is a fact about the
	// caller; it is not UnmatchedTarget, whose wording is about the target.
	NoCallerNode int
}
