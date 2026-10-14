package tools

import (
	"context"
	"sync"
)

// CallMemo remembers lookups for the length of one context_for_task collect. A
// pack asks the sensitive-path decision about a hundred or more files, and the
// daemon's decision resolves each file's project on disk; the files of one
// directory all resolve the same way, and nothing the call does creates a
// project marker, so within a call each directory need be resolved once.
//
// It lives on the call's context and nowhere else: Collect attaches a fresh one
// on every call, so an answer can never outlive the call that asked, and a
// marker created between two calls is seen by the second. A nil *CallMemo (a
// context no collect made) remembers nothing and runs every lookup.
//
// Concurrency: safe for concurrent use; a lookup runs under the memo's lock, so
// two askers of one key run it once.
type CallMemo struct {
	mu   sync.Mutex
	seen map[string]string
}

type callMemoKey struct{}

// WithCallMemo returns ctx carrying a fresh, empty CallMemo. Collect makes one
// per call; a test of a decision that reads it makes its own.
func WithCallMemo(ctx context.Context) context.Context {
	return context.WithValue(ctx, callMemoKey{}, &CallMemo{seen: map[string]string{}})
}

// CallMemoFrom returns the memo of the collect ctx belongs to, or nil outside one.
func CallMemoFrom(ctx context.Context) *CallMemo {
	m, _ := ctx.Value(callMemoKey{}).(*CallMemo)
	return m
}

// Once returns the answer remembered for key, running lookup to get it the first
// time. On a nil memo it runs lookup every time.
func (m *CallMemo) Once(key string, lookup func() string) string {
	if m == nil {
		return lookup()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.seen[key]; ok {
		return v
	}
	v := lookup()
	m.seen[key] = v
	return v
}
