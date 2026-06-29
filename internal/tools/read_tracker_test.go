package tools

import (
	"testing"
	"time"
)

func TestReadTrackerRecordFiresSink(t *testing.T) {
	rt := NewReadTracker()
	var gotPath, gotSHA string
	var gotMtime time.Time
	rt.SetPersistSink(func(path string, mtime time.Time, sha string) {
		gotPath, gotMtime, gotSHA = path, mtime, sha
	})

	mt := time.Unix(1000, 42)
	rt.Record("/ws/a.go", mt, "sha-a")

	if gotPath != "/ws/a.go" || gotSHA != "sha-a" || !gotMtime.Equal(mt) {
		t.Fatalf("sink got (%q, %v, %q), want (/ws/a.go, %v, sha-a)", gotPath, gotMtime, gotSHA, mt)
	}
}

func TestReadTrackerRecordCleansPathForSink(t *testing.T) {
	rt := NewReadTracker()
	var gotPath string
	rt.SetPersistSink(func(path string, _ time.Time, _ string) { gotPath = path })
	rt.Record("/ws/./sub/../a.go", time.Unix(1, 0), "")
	if gotPath != "/ws/a.go" {
		t.Fatalf("sink path = %q, want cleaned /ws/a.go", gotPath)
	}
}

func TestReadTrackerHydrateNoSink(t *testing.T) {
	rt := NewReadTracker()
	fired := false
	rt.SetPersistSink(func(string, time.Time, string) { fired = true })

	mt := time.Unix(2000, 7)
	rt.Hydrate([]ReadRecord{{Path: "/ws/b.go", Mtime: mt, SHA: "sha-b"}})

	if fired {
		t.Fatal("Hydrate fired the persist sink; it must not")
	}
	if got := rt.Mtime("/ws/b.go"); !got.Equal(mt) {
		t.Fatalf("hydrated mtime = %v, want %v", got, mt)
	}
	e, ok := rt.recorded("/ws/b.go")
	if !ok || e.sha != "sha-b" {
		t.Fatalf("hydrated record = (%+v, %v), want sha-b present", e, ok)
	}
}

func TestReadTrackerResetLeavesSinkInstalled(t *testing.T) {
	rt := NewReadTracker()
	calls := 0
	rt.SetPersistSink(func(string, time.Time, string) { calls++ })
	rt.Record("/ws/a.go", time.Unix(1, 0), "")
	rt.Reset()
	if got := rt.Mtime("/ws/a.go"); !got.IsZero() {
		t.Fatalf("Reset left an entry: %v", got)
	}
	// Sink survives Reset (Reset only clears entries) and fires on the next read.
	rt.Record("/ws/c.go", time.Unix(2, 0), "")
	if calls != 2 {
		t.Fatalf("sink call count = %d, want 2 (Reset must not drop the sink)", calls)
	}
}

func TestReadTrackerNilSafeNewMethods(t *testing.T) {
	var rt *ReadTracker
	rt.SetPersistSink(func(string, time.Time, string) {})
	rt.Hydrate([]ReadRecord{{Path: "/x", Mtime: time.Unix(1, 0)}})
	// no panic == pass
}
