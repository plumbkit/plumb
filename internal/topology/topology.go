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

// Status is a snapshot of the topology index health.
type Status struct {
	IndexedFiles int
	SkippedFiles int
	EmptyFiles   int // successfully indexed files with zero nodes (comment-only, unsupported language, etc.)
	TotalNodes   int
	TotalEdges   int
	DBSizeBytes  int64
	LastSync     time.Time
	IndexerState string
	Languages    []string
	LastError    string
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
