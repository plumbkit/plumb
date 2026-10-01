package mcp

// result_notes.go — a per-call scratchpad for facts only a tool's own run knows
// and the tools/call result's `_meta` must carry.
//
// Server.ToolResultMeta sees the call's name, arguments and ctx AFTER the tool
// ran, but not what the tool decided on the way. Re-deriving a decision from the
// state it left behind is unsound whenever the run itself changes that state: a
// call that wrote a per-agent row looks, afterwards, like a call routed to that
// agent. So the code that makes the decision records it here, and the hook reads
// the record, rather than the hook guessing from what the decision left.
//
// The scratchpad lives and dies with one tools/call: handleToolsCall mints it
// before the tool runs, so a nested or concurrent call never sees another's.

import (
	"context"
	"sync"
)

type resultNotesKey struct{}

// resultNotes is the scratchpad itself. A tool's callbacks may run on another
// goroutine than the handler's (execTool runs a bounded tool in its own), so it
// is guarded.
type resultNotes struct {
	mu sync.Mutex
	m  map[string]string
}

// WithResultNotes returns a ctx carrying a fresh, empty scratchpad. The
// tools/call handler installs one per call; tests that exercise a ToolResultMeta
// hook directly install their own.
func WithResultNotes(ctx context.Context) context.Context {
	return context.WithValue(ctx, resultNotesKey{}, &resultNotes{m: map[string]string{}})
}

// NoteResultMeta records key=value for the current tools/call. A later note on
// the same key replaces an earlier one. A no-op on a ctx with no scratchpad (a
// call made in-process rather than through the handler), which is the point:
// recording a fact must never be a reason for a call to fail.
func NoteResultMeta(ctx context.Context, key, value string) {
	n, ok := ctx.Value(resultNotesKey{}).(*resultNotes)
	if !ok {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.m[key] = value
}

// ResultMetaNote reads back what NoteResultMeta recorded for key on this call.
// ok is false when nothing was recorded, which a reader must treat as "the
// decision was not reported", never as a value.
func ResultMetaNote(ctx context.Context, key string) (value string, ok bool) {
	n, found := ctx.Value(resultNotesKey{}).(*resultNotes)
	if !found {
		return "", false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	value, ok = n.m[key]
	return value, ok
}
