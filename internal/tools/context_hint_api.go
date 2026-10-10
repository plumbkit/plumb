package tools

import (
	"context"
	"time"
)

// This file is the contract between context_for_task's collector and its
// advisory lifecycle hooks (PLAN-462). A hook asks for a hint: selectors,
// locations and provenance only, never source, documentation, memory or mail
// text, and never a read record. The collector implements ContextHinter; the
// daemon's hook control command consumes it.

// ContextSeed names a starting point: a file, optionally narrowed to one
// symbol. Symbol accepts the selector forms topology.SelectorVariants expands
// ("Recv.Method", "(*Recv).Method", a bare name).
type ContextSeed struct {
	Path   string
	Symbol string
}

// HintRequest asks for a hint over one canonical workspace root. The caller
// (the hook control command) resolves Workspace for the hooked agent; the
// collector answers from that root's own index and never from another root.
// Agent is for attribution only. MaxBytes bounds the rendered hint.
type HintRequest struct {
	Workspace string
	Agent     string
	Seeds     []ContextSeed
	MaxBytes  int
	Deadline  time.Time
}

// HintLine is one selector-level pointer: where a relevant declaration lives
// and why it was offered. It carries no source text.
type HintLine struct {
	Selector   string
	Path       string
	Kind       string
	Provenance string
	Line       int
}

// HintResult is a bounded set of pointers plus what the collector could not
// say. Omitted counts lines dropped by the byte budget; Freshness is the index
// state the lines were taken from; Gaps names truthful partial-answer reasons
// (for example an ambiguous selector or an unsupported language).
type HintResult struct {
	Lines     []HintLine
	Omitted   int
	Freshness string
	Gaps      []string
}

// ContextHinter answers hint requests. Implementations must not record reads,
// read file bodies for rendering, or consult any root other than
// req.Workspace.
type ContextHinter interface {
	Hint(ctx context.Context, req HintRequest) (HintResult, error)
}
