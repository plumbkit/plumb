package web

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/topology"
)

// TestTopologyDTOFromStatus proves the Status→DTO mapping keeps its wire shape:
// every scalar carried through, fileErrors present as path/message pairs when
// the index recorded skip reasons, and null (not a fabricated entry) when it
// recorded none. Pure function, no DB — the shape is the contract the SPA and
// any API consumer rely on.
func TestTopologyDTOFromStatus(t *testing.T) {
	lastSync := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	st := topology.Status{
		IndexerState: "idle",
		IndexedFiles: 10,
		SkippedFiles: 2,
		EmptyFiles:   1,
		TotalNodes:   100,
		TotalEdges:   40,
		DBSizeBytes:  2048,
		LastSync:     lastSync,
		Languages:    []string{"go"},
		LastError:    "",
		FileErrors: []topology.FileError{
			{Path: "a.go", Message: "parse stopped early: timeout"},
			{Path: "b.py", Message: "extractor panic: boom"},
		},
	}

	out := topologyDTOFromStatus("/ws", st)
	if !out.Available || out.Workspace != "/ws" {
		t.Errorf("availability/workspace not set: %+v", out)
	}
	if out.IndexedFiles != 10 || out.SkippedFiles != 2 || out.EmptyFiles != 1 ||
		out.TotalNodes != 100 || out.TotalEdges != 40 || out.DBSizeBytes != 2048 {
		t.Errorf("scalar fields not carried through: %+v", out)
	}
	if !out.LastSync.Equal(lastSync) || out.IndexerState != "idle" || len(out.Languages) != 1 {
		t.Errorf("sync/state/languages not carried through: %+v", out)
	}
	if len(out.FileErrors) != 2 ||
		out.FileErrors[0] != (fileErrorDTO{Path: "a.go", Message: "parse stopped early: timeout"}) ||
		out.FileErrors[1] != (fileErrorDTO{Path: "b.py", Message: "extractor panic: boom"}) {
		t.Errorf("fileErrors not mapped: %+v", out.FileErrors)
	}

	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	fes, ok := wire["fileErrors"].([]any)
	if !ok || len(fes) != 2 {
		t.Fatalf("wire fileErrors = %v, want 2 entries", wire["fileErrors"])
	}
	first, ok := fes[0].(map[string]any)
	if !ok || first["path"] != "a.go" || first["message"] != "parse stopped early: timeout" {
		t.Errorf("wire fileErrors[0] = %v, want path/message pair", fes[0])
	}

	// No recorded reasons: the field must be present and null, and no
	// fabricated entries.
	empty := topologyDTOFromStatus("/ws", topology.Status{IndexerState: "idle", IndexedFiles: 10})
	if empty.FileErrors != nil {
		t.Errorf("FileErrors = %v, want nil for a clean index", empty.FileErrors)
	}
	raw, err = json.Marshal(empty)
	if err != nil {
		t.Fatalf("marshal clean: %v", err)
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal clean: %v", err)
	}
	if v, present := wire["fileErrors"]; !present || v != nil {
		t.Errorf("wire fileErrors = %v (present=%v), want an explicit null", v, present)
	}
}
