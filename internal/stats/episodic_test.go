package stats

import (
	"testing"
	"time"
)

func TestEpisodic_RecordAndLatest(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	db, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	base := Episodic{
		Workspace: "/ws", SessionID: "s1", SessionName: "swift",
		GeneratedAt: time.UnixMilli(1000), Summary: "did stuff",
		TouchedFiles: []string{"a.go", "b.go"}, ReadCount: 3, WriteCount: 2,
	}
	if err := db.recordEpisodic(base); err != nil {
		t.Fatalf("recordEpisodic: %v", err)
	}
	newer := base
	newer.Summary = "newer summary"
	newer.GeneratedAt = time.UnixMilli(2000)
	if err := db.recordEpisodic(newer); err != nil {
		t.Fatalf("recordEpisodic newer: %v", err)
	}

	got, ok, err := db.LatestEpisodic("/ws")
	if err != nil || !ok {
		t.Fatalf("LatestEpisodic ok=%v err=%v", ok, err)
	}
	if got.Summary != "newer summary" {
		t.Errorf("want newest summary, got %q", got.Summary)
	}
	if len(got.TouchedFiles) != 2 {
		t.Errorf("touched files not round-tripped: %v", got.TouchedFiles)
	}
	// Workspace partitioning: another workspace must not see /ws's summary.
	if _, ok, _ := db.LatestEpisodic("/other"); ok {
		t.Error("episodic summary leaked across workspaces")
	}
}

func TestToolCallsForSession_Partitioned(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	db, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	now := time.Now()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(db.Record(Call{Workspace: "/ws", SessionID: "s1", Tool: "edit_file", CalledAt: now, InputJSON: `{"file_path":"/ws/a.go"}`, Success: true}))
	must(db.Record(Call{Workspace: "/ws", SessionID: "s1", Tool: "read_file", CalledAt: now, InputJSON: `{}`, Success: true}))
	must(db.Record(Call{Workspace: "/other", SessionID: "s1", Tool: "edit_file", CalledAt: now, InputJSON: `{}`, Success: true}))

	calls, err := db.ToolCallsForSession("/ws", "s1", now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("ToolCallsForSession: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("want 2 calls for /ws, got %d", len(calls))
	}
}
