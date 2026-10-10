package tools

// context_hint_api_shim.go — a TEMPORARY copy of the context-hint API that
// PLAN-462 Slice A publishes in context_hint_api.go (warm-cobra, branch
// atlas/plan-462-collector). Slice B (the advisory hooks) builds against these
// exact names so that, when Slice A's commit lands, deleting this file is the
// whole swap. Do not add behaviour here and do not let it drift from the agreed
// shapes (private design note PLAN-462-sliceB-adoption.md §7).

import (
	"context"
	"time"
)

// ContextSeed is one starting point: a path, a symbol selector (any form
// topology.SelectorVariants accepts), or both.
type ContextSeed struct {
	Path   string
	Symbol string
}

// HintRequest asks for a selector-only hint for a workspace the caller has
// already resolved to its canonical root.
type HintRequest struct {
	Workspace string
	Agent     string
	Seeds     []ContextSeed
	MaxBytes  int
	Deadline  time.Time
}

// HintLine is one selector with its location and provenance. It never carries
// source, documentation, memory or mail text.
type HintLine struct {
	Selector   string
	Path       string
	Kind       string
	Provenance string
	Line       int
}

// HintResult is a hint's lines plus what it left out and why.
type HintResult struct {
	Lines     []HintLine
	Omitted   int
	Freshness string
	Gaps      []string
}

// ContextHinter produces selector-only hints. An implementation records no
// reads: it has no read tracker to record into.
type ContextHinter interface {
	Hint(ctx context.Context, req HintRequest) (HintResult, error)
}
