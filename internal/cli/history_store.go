package cli

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/plumbkit/plumb/internal/history"
)

// historyStore is the daemon-global, lazily opened history writer (the
// statsStore pattern): opened on the first write, retried at most once a
// minute after a failure, so the daemon never fails because of history.
//
// Concurrency: safe for concurrent use; mu guards every field.
type historyStore struct {
	mu       sync.Mutex
	s        *history.Store
	closed   bool
	failedAt time.Time
	maxDiff  func() int64
}

func newHistoryStore(maxDiff func() int64) *historyStore { return &historyStore{maxDiff: maxDiff} }

func (h *historyStore) store() *history.Store {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || h.s != nil || (!h.failedAt.IsZero() && time.Since(h.failedAt) < time.Minute) {
		return h.s
	}
	s, err := history.Open(history.DBPath(), history.Options{MaxDiffBytes: h.maxDiff})
	if err != nil {
		h.failedAt = time.Now()
		slog.Warn("history: cannot open history.db; recording disabled for now", "err", err)
		return nil
	}
	h.s = s
	return s
}

// Enqueue hands it to the store (never blocks).
func (h *historyStore) Enqueue(it history.Item) {
	if s := h.store(); s != nil {
		s.Enqueue(it)
	}
}

// Close drains within 2 s (spec §9) and closes. Idempotent.
func (h *historyStore) Close() {
	if h == nil {
		return
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	s := h.s
	h.s = nil
	h.mu.Unlock()
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		slog.Warn("history: close", "err", err)
	}
}
