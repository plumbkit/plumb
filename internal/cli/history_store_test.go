package cli

import (
	"testing"

	"github.com/plumbkit/plumb/internal/history"
)

func TestHistoryStoreLazyOpen(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	hs := newHistoryStore(func() int64 { return 1024 })
	hs.mu.Lock()
	if hs.s != nil {
		hs.mu.Unlock()
		t.Fatal("store should be nil before first enqueue")
	}
	hs.mu.Unlock()

	hs.Enqueue(history.Item{Change: history.Change{Op: history.OpCreate, Path: "/test"}})
	if hs.store() == nil {
		t.Fatal("store() should be non-nil after enqueue")
	}
	hs.mu.Lock()
	if hs.s == nil {
		hs.mu.Unlock()
		t.Fatal("store internal pointer should be non-nil after enqueue")
	}
	hs.mu.Unlock()
	hs.Close()
}

func TestHistoryStoreCloseIdempotent(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	hs := newHistoryStore(nil)
	hs.Enqueue(history.Item{Change: history.Change{Op: history.OpCreate, Path: "/test"}})
	hs.Close()
	hs.Close() // must not panic or error
}

func TestHistoryStoreEnqueueAfterCloseDropped(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	hs := newHistoryStore(nil)
	hs.Close()
	// Must not panic and must not open a new store
	hs.Enqueue(history.Item{Change: history.Change{Op: history.OpCreate, Path: "/test"}})
	hs.mu.Lock()
	if hs.s != nil {
		hs.mu.Unlock()
		t.Fatal("store opened after Close")
	}
	hs.mu.Unlock()
}
