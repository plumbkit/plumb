package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/session"
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

// waitForIndexReadable waits until a freshly opened store's index is readable
// from out of process — the state a daemon is in whenever its pool holds a store
// for the workspace.
func waitForIndexReadable(t *testing.T, ws string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := topology.StatusForWorkspace(ws); err == nil {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("topology index for %s never became readable: %v", ws, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestHandleTopology_FailingFlag proves the DTO's failing flag is carried from
// the LIVE indexer when the daemon holds one, and stays false — "not known",
// not "fine" — when the Server has no live source, which is the
// out-of-process snapshot path where topology.StatusForWorkspace attaches no
// indexer and its Failing is structurally false.
//
// The health source is faked at the Deps seam rather than driven through a
// fault-injected index: internal/tools' injectPersistFault and waitForHealth are
// unexported helpers in package tools, and the indexer's own failing
// determination is already covered end to end there. What this layer owns — and
// what this test pins — is the overlay of that health onto the wire DTO.
func TestHandleTopology_FailingFlag(t *testing.T) {
	ws := t.TempDir()
	// resolveWorkspace accepts only a workspace of a currently-attached session,
	// so register one in a data dir private to this test rather than the live
	// daemon's registry.
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	info, err := session.Register(session.Info{Folder: ws, Name: "test-session", ClientName: "test-client"})
	if err != nil {
		t.Fatalf("record session start: %v", err)
	}
	t.Cleanup(func() { session.Unregister(info.ID) })

	// A live store implies an on-disk index, so the daemon's own wiring always
	// reads the census path; open one here so this test exercises that path
	// rather than only the missing-index fallback.
	store, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024}, nil)
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	waitForIndexReadable(t, ws)

	var mu sync.Mutex
	failing := true
	live := New(Deps{
		Store:     config.NewStore(config.Defaults()),
		StartedAt: time.Now(),
		TopologyHealth: func(got string) (topology.Health, bool) {
			if got != ws {
				return topology.Health{}, false
			}
			mu.Lock()
			defer mu.Unlock()
			return topology.Health{State: "idle", Failing: failing}, true
		},
	})

	// get returns the decoded DTO and the raw wire value of "failing", so a
	// dropped JSON tag cannot pass on the struct's zero value.
	get := func(s *Server) (topologyDTO, any) {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/api/topology?workspace="+url.QueryEscape(ws), nil)
		w := httptest.NewRecorder()
		s.handleTopology(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		var res topologyDTO
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
		if !res.Available {
			t.Fatalf("available = false, want the census path: %s", w.Body.String())
		}
		var wire map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &wire); err != nil {
			t.Fatalf("unmarshal wire: %v", err)
		}
		raw, present := wire["failing"]
		if !present {
			t.Fatalf("wire has no failing field: %s", w.Body.String())
		}
		if raw != res.Failing {
			t.Fatalf("wire failing = %v, struct = %v", raw, res.Failing)
		}
		return res, raw
	}

	res, raw := get(live)
	if !res.Failing || raw != true {
		t.Errorf("failing = %v (wire %v), want true while the live indexer reports a failed cycle", res.Failing, raw)
	}

	mu.Lock()
	failing = false
	mu.Unlock()
	res, raw = get(live)
	if res.Failing || raw != false {
		t.Errorf("failing = %v (wire %v), want false once the live indexer recovered", res.Failing, raw)
	}

	// A Server built without Deps.TopologyHealth keeps the snapshot path: the
	// flag must be false, not absent, and never true from a source that cannot
	// know.
	res, raw = get(New(Deps{}))
	if res.Failing || raw != false {
		t.Errorf("failing = %v (wire %v), want false with no live health source", res.Failing, raw)
	}
}
