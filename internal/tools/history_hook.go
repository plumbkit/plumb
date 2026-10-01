package tools

import (
	"context"
	"log/slog"
	"time"

	"github.com/plumbkit/plumb/internal/history"
)

// historySink is a write-site's handle on the history recorder, for free
// functions (rollbacks, move/workspace-edit appliers, txlog) that have no
// WriteDeps. A nil sink records nothing. Its method is named recordHistory so
// the arch co-call rule (Task 15) sees every recording site by one name.
type historySink func(history.Change)

func (h historySink) recordHistory(c history.Change) {
	if h == nil {
		return
	}
	if c.At.IsZero() {
		c.At = time.Now()
	}
	if c.Kind == "" {
		c.Kind = history.KindFile
	}
	h(c)
}

// historyOn reports whether this call's writes are recorded; write sites check
// it before reading a before-side they would not otherwise need.
func (d WriteDeps) historyOn() bool {
	return d.HistoryFn != nil && (d.HistoryEnabledFn == nil || d.HistoryEnabledFn())
}

// recordHistory reports one landed write. Call it after the write succeeds and
// BEFORE releasing the per-path lock (the recordUndo contract), so ts order
// matches lock order.
func (d WriteDeps) recordHistory(ctx context.Context, c history.Change) {
	if !d.historyOn() {
		return
	}
	d.historySink(ctx).recordHistory(c)
}

// historySink binds ctx so a free function can record.
func (d WriteDeps) historySink(ctx context.Context) historySink {
	if !d.historyOn() {
		return nil
	}
	return func(c history.Change) { d.HistoryFn(ctx, c) }
}

// historySide reads path's current content for a change's before-side, or the
// zero (absent) Side when history is off — so a write never pays for a read it
// would not record. A read error degrades to an absent side, logged.
func (d WriteDeps) historySide(path string) history.Side {
	if !d.historyOn() {
		return history.Side{}
	}
	s, err := history.SideFromFile(path)
	if err != nil {
		slog.Debug("history: capture side", "path", path, "err", err)
	}
	return s
}
