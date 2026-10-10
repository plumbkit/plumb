package tools

import (
	"context"
	"log/slog"
	"os"
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
	return d.readSide(path)
}

// wantContent reports whether a write site should read content it would
// otherwise not need. The two consumers are independent: history records the
// bytes, and the response renders them. Either one wants them, or the read is
// skipped — which is why this is not historyOn().
func (d WriteDeps) wantContent() bool {
	return d.historyOn() || d.showWriteDiff()
}

// contentSide reads path for a site that needs the bytes for EITHER the
// history row or the response diff (see wantContent). Use it in place of
// historySide wherever a response diff is rendered from the same read, or a
// user running show_write_diff with history off would get no diff at all.
//
// With history OFF the bytes serve the response alone, and a response withholds
// anything past maxResponseDiffBytes (sideOf turns it into unknownSide and the
// diff says "file too large"). Reading — and, over history.HardMaxContentBytes,
// hashing — such a file to print that marker is pure waste, so it is stat'ed and
// answered with existence and size alone (PLAN-457). With history ON the store
// needs the bytes, so the read is unchanged.
func (d WriteDeps) contentSide(path string) history.Side {
	if !d.wantContent() {
		return history.Side{}
	}
	if !d.historyOn() {
		if s, ok := sideOverResponseCap(path); ok {
			return s
		}
	}
	return d.readSide(path)
}

// sideOverResponseCap stats path and, when it is a regular file larger than the
// response diff cap, returns the stat-only side a response needs. ok is false
// whenever the caller must read the file: absent, not a regular file (a
// directory, a device, a pipe), at or under the cap, or a stat that failed.
func sideOverResponseCap(path string) (history.Side, bool) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= maxResponseDiffBytes {
		return history.Side{}, false
	}
	return history.SideStat(info.Size()), true
}

// readSide reads path's current content as a Side. A read error degrades to an
// absent side, logged at debug: the write itself already succeeded, and a side
// that could not be read reports as absent rather than failing the call.
func (d WriteDeps) readSide(path string) history.Side {
	s, err := history.SideFromFile(path)
	if err != nil {
		slog.Debug("history: capture side", "path", path, "err", err)
	}
	return s
}
