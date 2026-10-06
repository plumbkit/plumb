package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/history"
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/stats"
)

func seedHistoryAndStats(t *testing.T, ws string) (*history.Store, *stats.DB) {
	t.Helper()
	dataDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataDir)

	histStore, err := history.Open(history.DBPath(), history.Options{})
	if err != nil {
		t.Fatalf("open history store: %v", err)
	}

	statsDB, err := stats.Open()
	if err != nil {
		t.Fatalf("open stats db: %v", err)
	}

	// Register a session in the test workspace so resolveWorkspace recognises it
	info, err := session.Register(session.Info{
		Folder:     ws,
		Name:       "test-session",
		ClientName: "test-client",
	})
	if err != nil {
		t.Fatalf("record session start: %v", err)
	}
	t.Cleanup(func() { session.Unregister(info.ID) })

	return histStore, statsDB
}

func TestHandleHistory_EmptyDB(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	s := &Server{}

	r := httptest.NewRequest(http.MethodGet, "/api/history?all=true", nil)
	w := httptest.NewRecorder()
	s.handleHistory(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var res historyListDTO
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(res.Changes) != 0 {
		t.Fatalf("len(Changes) = %d, want 0", len(res.Changes))
	}
}

func TestHandleHistory_ListsAndFilters(t *testing.T) {
	ws := t.TempDir()
	histStore, statsDB := seedHistoryAndStats(t, ws)
	defer histStore.Close(context.Background())
	defer statsDB.Close()

	ctx := context.Background()
	callTime := time.Now().Add(-5 * time.Second)

	// Record a tool call in stats
	callID := "call-edit-1"
	if err := statsDB.Record(stats.Call{
		Workspace:    ws,
		SessionID:    "sess-1",
		SessionName:  "test-session",
		Tool:         "edit_file",
		CalledAt:     callTime,
		DurationMs:   42,
		Success:      true,
		LogicalAgent: "test-agent",
		CallID:       callID,
	}); err != nil {
		t.Fatalf("record call: %v", err)
	}

	// Enqueue a regular change
	item1 := history.Item{
		CallID:       callID,
		SessionID:    "sess-1",
		SessionName:  "test-session",
		LogicalAgent: "test-agent",
		ClientName:   "test-client",
		Workspace:    ws,
		Change: history.Change{
			At:     callTime,
			Op:     history.OpUpdate,
			Kind:   history.KindFile,
			Tool:   "edit_file",
			Path:   filepath.Join(ws, "main.go"),
			Before: history.SideFromBytes([]byte("package main\n")),
			After:  history.SideFromBytes([]byte("package main\nfunc main() {}\n")),
		},
	}
	histStore.Enqueue(item1)

	// Enqueue a withheld item
	itemSensitive := history.Item{
		CallID:       "call-sensitive-2",
		SessionID:    "sess-1",
		SessionName:  "test-session",
		LogicalAgent: "test-agent",
		ClientName:   "test-client",
		Workspace:    ws,
		Content:      history.ContentSensitive,
		Change: history.Change{
			At:     callTime.Add(time.Second),
			Op:     history.OpCreate,
			Kind:   history.KindFile,
			Tool:   "write_file",
			Path:   filepath.Join(ws, ".env"),
			Before: history.Side{},
			After:  history.SideFromBytes([]byte("SECRET=xyz\n")),
		},
	}
	histStore.Enqueue(itemSensitive)

	if err := histStore.Sync(ctx); err != nil {
		t.Fatalf("sync history store: %v", err)
	}

	s := &Server{}

	// Query list for this workspace
	r := httptest.NewRequest(http.MethodGet, "/api/history?workspace="+ws, nil)
	w := httptest.NewRecorder()
	s.handleHistory(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", w.Code, w.Body.String())
	}

	var res historyListDTO
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(res.Changes) != 2 {
		t.Fatalf("got %d changes, want 2", len(res.Changes))
	}

	// Newest first: sensitive row is index 0
	if res.Changes[0].Content != "withheld:sensitive" {
		t.Errorf("change[0].Content = %q, want withheld:sensitive", res.Changes[0].Content)
	}
	if res.Changes[0].Path != ".env" {
		t.Errorf("change[0].Path = %q, want .env", res.Changes[0].Path)
	}

	// Index 1 is edit_file, should have enriched Call metadata
	if res.Changes[1].Call == nil {
		t.Fatal("change[1].Call is nil; expected enriched tool call")
	}
	if res.Changes[1].Call.Tool != "edit_file" || res.Changes[1].Call.DurationMs != 42 || !res.Changes[1].Call.Success {
		t.Errorf("unexpected call info: %+v", res.Changes[1].Call)
	}

	// Filter by tool=edit_file
	rTool := httptest.NewRequest(http.MethodGet, "/api/history?workspace="+ws+"&tool=edit_file", nil)
	wTool := httptest.NewRecorder()
	s.handleHistory(wTool, rTool)
	var resTool historyListDTO
	_ = json.Unmarshal(wTool.Body.Bytes(), &resTool)
	if len(resTool.Changes) != 1 || resTool.Changes[0].Tool != "edit_file" {
		t.Fatalf("tool filter: got %d changes, want 1", len(resTool.Changes))
	}

	// Filter by file=.env
	rFile := httptest.NewRequest(http.MethodGet, "/api/history?workspace="+ws+"&file=.env", nil)
	wFile := httptest.NewRecorder()
	s.handleHistory(wFile, rFile)
	var resFile historyListDTO
	_ = json.Unmarshal(wFile.Body.Bytes(), &resFile)
	if len(resFile.Changes) != 1 || resFile.Changes[0].Path != ".env" {
		t.Fatalf("file filter: got %d changes, want 1", len(resFile.Changes))
	}
}

func TestHandleHistoryDetail_BySeqAndCallID(t *testing.T) {
	ws := t.TempDir()
	histStore, statsDB := seedHistoryAndStats(t, ws)
	defer histStore.Close(context.Background())
	defer statsDB.Close()

	ctx := context.Background()
	callTime := time.Now()

	callID := "call-multi-1"
	if err := statsDB.Record(stats.Call{
		Workspace:    ws,
		SessionID:    "sess-1",
		Tool:         "edit_file",
		CalledAt:     callTime,
		DurationMs:   25,
		Success:      true,
		LogicalAgent: "test-agent",
		CallID:       callID,
	}); err != nil {
		t.Fatalf("record call: %v", err)
	}

	histStore.Enqueue(history.Item{
		CallID:    callID,
		Workspace: ws,
		Change: history.Change{
			At:     callTime,
			Op:     history.OpCreate,
			Kind:   history.KindFile,
			Tool:   "edit_file",
			Path:   filepath.Join(ws, "a.go"),
			Before: history.Side{},
			After:  history.SideFromBytes([]byte("package a\n")),
		},
	})
	histStore.Enqueue(history.Item{
		CallID:    callID,
		Workspace: ws,
		Content:   history.ContentSensitive,
		Change: history.Change{
			At:     callTime.Add(time.Millisecond),
			Op:     history.OpCreate,
			Kind:   history.KindFile,
			Tool:   "edit_file",
			Path:   filepath.Join(ws, "secret.key"),
			Before: history.Side{},
			After:  history.SideFromBytes([]byte("key-material\n")),
		},
	})

	if err := histStore.Sync(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}

	s := &Server{}

	// Test GET by seq 1
	rSeq := httptest.NewRequest(http.MethodGet, "/api/history/1", nil)
	rSeq.SetPathValue("target", "1")
	wSeq := httptest.NewRecorder()
	s.handleHistoryDetail(wSeq, rSeq)

	if wSeq.Code != http.StatusOK {
		t.Fatalf("seq 1 status = %d, want 200, body: %s", wSeq.Code, wSeq.Body.String())
	}
	var resSeq historyDetailDTO
	if err := json.Unmarshal(wSeq.Body.Bytes(), &resSeq); err != nil {
		t.Fatalf("unmarshal seq 1: %v", err)
	}
	if resSeq.Seq != 1 || resSeq.Entry == nil || resSeq.Entry.Path != "a.go" {
		t.Errorf("unexpected seq detail: %+v", resSeq)
	}
	if resSeq.Diff == "" {
		t.Error("diff should be non-empty for regular file create")
	}
	if resSeq.Call == nil || resSeq.Call.Tool != "edit_file" {
		t.Errorf("call metadata missing or wrong: %+v", resSeq.Call)
	}

	// Test GET by seq 2 (withheld sensitive)
	rSeq2 := httptest.NewRequest(http.MethodGet, "/api/history/2", nil)
	rSeq2.SetPathValue("target", "2")
	wSeq2 := httptest.NewRecorder()
	s.handleHistoryDetail(wSeq2, rSeq2)

	if wSeq2.Code != http.StatusOK {
		t.Fatalf("seq 2 status = %d, want 200", wSeq2.Code)
	}
	var resSeq2 historyDetailDTO
	_ = json.Unmarshal(wSeq2.Body.Bytes(), &resSeq2)
	if resSeq2.Diff != "" {
		t.Errorf("diff for sensitive row must be empty, got: %q", resSeq2.Diff)
	}
	if resSeq2.Entry.Content != "withheld:sensitive" {
		t.Errorf("content marker = %q, want withheld:sensitive", resSeq2.Entry.Content)
	}

	// Test GET by call_id
	rCall := httptest.NewRequest(http.MethodGet, "/api/history/"+callID, nil)
	rCall.SetPathValue("target", callID)
	wCall := httptest.NewRecorder()
	s.handleHistoryDetail(wCall, rCall)

	if wCall.Code != http.StatusOK {
		t.Fatalf("call_id status = %d, want 200", wCall.Code)
	}
	var resCall historyDetailDTO
	_ = json.Unmarshal(wCall.Body.Bytes(), &resCall)
	if len(resCall.Entries) != 2 || len(resCall.Diffs) != 2 {
		t.Fatalf("by call: got %d entries, %d diffs, want 2", len(resCall.Entries), len(resCall.Diffs))
	}
	if resCall.Call == nil || resCall.Call.Tool != "edit_file" {
		t.Errorf("call metadata for call_id lookup missing: %+v", resCall.Call)
	}

	// Test 404 for unknown seq / call_id
	r404 := httptest.NewRequest(http.MethodGet, "/api/history/99999", nil)
	r404.SetPathValue("target", "99999")
	w404 := httptest.NewRecorder()
	s.handleHistoryDetail(w404, r404)
	if w404.Code != http.StatusNotFound {
		t.Fatalf("unknown target status = %d, want 404", w404.Code)
	}
}

func TestHandleHistory_GapsAndMarkers(t *testing.T) {
	ws := t.TempDir()
	histStore, statsDB := seedHistoryAndStats(t, ws)
	defer histStore.Close(context.Background())
	defer statsDB.Close()

	ctx := context.Background()
	callTime := time.Now()

	// 1. Initial write
	histStore.Enqueue(history.Item{
		CallID:    "call-1",
		Workspace: ws,
		Change: history.Change{
			At:     callTime,
			Op:     history.OpCreate,
			Kind:   history.KindFile,
			Tool:   "write_file",
			Path:   filepath.Join(ws, "gap.txt"),
			Before: history.Side{},
			After:  history.SideFromBytes([]byte("v1\n")),
		},
	})

	// 2. Write with a before state that does NOT match v1 (gap introduced outside plumb)
	histStore.Enqueue(history.Item{
		CallID:    "call-2",
		Workspace: ws,
		Change: history.Change{
			At:     callTime.Add(time.Second),
			Op:     history.OpUpdate,
			Kind:   history.KindFile,
			Tool:   "edit_file",
			Path:   filepath.Join(ws, "gap.txt"),
			Before: history.SideFromBytes([]byte("v1_modified_by_external_tool\n")),
			After:  history.SideFromBytes([]byte("v2\n")),
		},
	})

	if err := histStore.Sync(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}

	s := &Server{}
	r := httptest.NewRequest(http.MethodGet, "/api/history?workspace="+ws, nil)
	w := httptest.NewRecorder()
	s.handleHistory(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var res historyListDTO
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(res.Changes) != 2 {
		t.Fatalf("got %d changes, want 2", len(res.Changes))
	}

	// Change 0 is call-2 (newest first). It should have GapBefore == true!
	if !res.Changes[0].GapBefore {
		t.Errorf("change[0].GapBefore = false, want true")
	}
	// Change 1 is call-1. First create has no gap before.
	if res.Changes[1].GapBefore {
		t.Errorf("change[1].GapBefore = true, want false")
	}
}
