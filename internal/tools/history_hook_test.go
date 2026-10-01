package tools

import (
	"context"
	"sync"
	"testing"

	"github.com/plumbkit/plumb/internal/history"
)

// fakeHistory records Changes for write-site tests (Tasks 11–14 use it).
type fakeHistory struct {
	mu sync.Mutex
	cs []history.Change
}

func (f *fakeHistory) record(_ context.Context, c history.Change) {
	f.mu.Lock()
	f.cs = append(f.cs, c)
	f.mu.Unlock()
}

func (f *fakeHistory) all() []history.Change {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]history.Change(nil), f.cs...)
}

func (f *fakeHistory) only(t *testing.T) history.Change {
	t.Helper()
	cs := f.all()
	if len(cs) != 1 {
		t.Fatalf("recorded %d changes, want 1: %+v", len(cs), cs)
	}
	return cs[0]
}

func TestRecordHistoryStampsTimeAndKind(t *testing.T) {
	var f fakeHistory
	d := WriteDeps{HistoryFn: f.record}
	d.recordHistory(context.Background(), history.Change{Op: history.OpUpdate, Path: "/x"})
	c := f.only(t)
	if c.At.IsZero() || c.Kind != history.KindFile {
		t.Fatalf("change = %+v", c)
	}
}

func TestHistoryOffCostsNothing(t *testing.T) {
	var f fakeHistory
	off := WriteDeps{HistoryFn: f.record, HistoryEnabledFn: func() bool { return false }}
	if off.historyOn() {
		t.Fatal("disabled config reported on")
	}
	if (WriteDeps{}).historyOn() {
		t.Fatal("nil HistoryFn reported on")
	}
	var nilSink historySink
	nilSink.recordHistory(history.Change{}) // must not panic
	if !(WriteDeps{HistoryFn: f.record}).historyOn() {
		t.Fatal("positive control: an enabled recorder must report on")
	}
}
