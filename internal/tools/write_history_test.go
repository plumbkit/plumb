package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/history"
	"github.com/plumbkit/plumb/internal/paths"
	"github.com/plumbkit/plumb/internal/stats"
)

func TestWriteHistory_Metadata(t *testing.T) {
	tool := NewWriteHistory()
	if tool.Name() != "write_history" {
		t.Errorf("got Name %q, want write_history", tool.Name())
	}
	if tool.Description() == "" {
		t.Error("Description is empty")
	}

	var schema struct {
		Type                 string                 `json:"type"`
		Properties           map[string]interface{} `json:"properties"`
		AdditionalProperties bool                   `json:"additionalProperties"`
	}
	if err := json.Unmarshal(tool.InputSchema(), &schema); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	if schema.AdditionalProperties {
		t.Error("expected additionalProperties to be false")
	}
	for _, prop := range []string{"mode", "seq", "call_id", "session", "agent", "tool", "file", "limit", "offset", "max_diff_bytes", "workspace"} {
		if _, ok := schema.Properties[prop]; !ok {
			t.Errorf("expected property %q in schema", prop)
		}
	}
}

func TestWriteHistory_Validate(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{
			name: "default list mode",
			raw:  `{}`,
		},
		{
			name:    "invalid mode",
			raw:     `{"mode": "destroy"}`,
			wantErr: "mode must be 'list' or 'show'",
		},
		{
			name:    "show without seq or call_id",
			raw:     `{"mode": "show"}`,
			wantErr: "show mode requires 'seq' or 'call_id'",
		},
		{
			name:    "show with both seq and call_id",
			raw:     `{"mode": "show", "seq": 1, "call_id": "c1"}`,
			wantErr: "cannot specify both 'seq' and 'call_id'",
		},
		{
			name:    "negative offset",
			raw:     `{"offset": -1}`,
			wantErr: "offset must be non-negative",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args, err := parseWriteHistoryArgs(json.RawMessage(tc.raw))
			if err != nil {
				t.Fatalf("unexpected parse error: %v", err)
			}
			err = args.validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected validation error: %v", err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got err %v, want substring %q", err, tc.wantErr)
				}
			}
		})
	}
}

func setupTestHistoryDB(t *testing.T, ws string) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "history.db")
	s, err := history.Open(dbPath, history.Options{})
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}

	// 1. Create file1
	f1 := filepath.Join(ws, "file1.txt")
	i1 := history.Item{
		Change: history.Change{
			At:     time.UnixMilli(1000),
			Op:     history.OpCreate,
			Kind:   history.KindFile,
			Tool:   "write_file",
			Path:   f1,
			Before: history.Side{},
			After:  history.SideFromBytes([]byte("line 1\nline 2\n")),
		},
		Workspace:    ws,
		CallID:       "CALL_01",
		SessionID:    "sess-alpha",
		SessionName:  "Alpha",
		LogicalAgent: "atlas",
	}

	// 2. Update file1 (generates diff)
	i2 := history.Item{
		Change: history.Change{
			At:     time.UnixMilli(2000),
			Op:     history.OpUpdate,
			Kind:   history.KindFile,
			Tool:   "edit_file",
			Path:   f1,
			Before: history.SideFromBytes([]byte("line 1\nline 2\n")),
			After:  history.SideFromBytes([]byte("line 1\nline 2 updated\n")),
		},
		Workspace:    ws,
		CallID:       "CALL_02",
		SessionID:    "sess-beta",
		SessionName:  "Beta",
		LogicalAgent: "peer-agent",
	}

	// 3. Gap on file1 (external update before write)
	i3 := history.Item{
		Change: history.Change{
			At:     time.UnixMilli(3000),
			Op:     history.OpUpdate,
			Kind:   history.KindFile,
			Tool:   "edit_file",
			Path:   f1,
			Before: history.SideFromBytes([]byte("EXTERNAL EDIT\n")),
			After:  history.SideFromBytes([]byte("EXTERNAL EDIT\nline 3\n")),
		},
		Workspace:    ws,
		CallID:       "CALL_03",
		SessionID:    "sess-alpha",
		SessionName:  "Alpha",
		LogicalAgent: "atlas",
	}

	// 4. Sensitive file (withheld:sensitive marker, no diff)
	f2 := filepath.Join(ws, ".env")
	i4 := history.Item{
		Change: history.Change{
			At:     time.UnixMilli(4000),
			Op:     history.OpCreate,
			Kind:   history.KindFile,
			Tool:   "write_file",
			Path:   f2,
			Before: history.Side{},
			After:  history.SideFromBytes([]byte("SECRET=12345\n")),
		},
		Workspace:    ws,
		CallID:       "CALL_04",
		SessionID:    "sess-alpha",
		SessionName:  "Alpha",
		LogicalAgent: "atlas",
		Content:      history.ContentSensitive,
		Added:        1,
	}

	s.Enqueue(i1)
	s.Enqueue(i2)
	s.Enqueue(i3)
	s.Enqueue(i4)

	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("history.Close: %v", err)
	}
	return dbPath
}

func setupTestStatsDB(t *testing.T, ws string) string {
	t.Helper()
	dataDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataDir)
	sdb, err := stats.Open()
	if err != nil {
		t.Fatalf("stats.Open: %v", err)
	}
	defer sdb.Close()

	if err := sdb.Record(stats.Call{
		Workspace:   ws,
		SessionID:   "sess-alpha",
		SessionName: "Alpha",
		Tool:        "edit_file",
		CalledAt:    time.UnixMilli(2000),
		DurationMs:  42,
		Success:     true,
		CallID:      "CALL_02",
	}); err != nil {
		t.Fatalf("Record stats: %v", err)
	}
	return filepath.Join(dataDir, "plumb", "stats.db")
}

func TestWriteHistory_List(t *testing.T) {
	ws := paths.Canonical(t.TempDir())
	dbPath := setupTestHistoryDB(t, ws)

	tool := NewWriteHistory().
		WithWorkspace(func(_ context.Context) string { return ws }).
		WithSelfSession(func() string { return "sess-alpha" }).
		withOpeners(
			func() (*history.Reader, error) { return history.OpenReadOnlyAt(dbPath) },
			func() (*stats.DB, error) { return nil, nil },
		)

	// List all
	out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute list: %v", err)
	}
	if !strings.Contains(out, "Write history (4 change(s), offset 0):") {
		t.Errorf("expected 4 changes in header, got:\n%s", out)
	}
	if !strings.Contains(out, "[withheld:sensitive]") {
		t.Errorf("expected withheld marker in listing, got:\n%s", out)
	}
	if !strings.Contains(out, "unrecorded change (outside plumb's write tools)") {
		t.Errorf("expected gap line in listing, got:\n%s", out)
	}

	// Filter session="self" (should match 3 out of 4)
	outSelf, err := tool.Execute(context.Background(), json.RawMessage(`{"session": "self"}`))
	if err != nil {
		t.Fatalf("Execute session=self: %v", err)
	}
	if !strings.Contains(outSelf, "Write history (3 change(s), offset 0):") {
		t.Errorf("expected 3 changes for session=self, got:\n%s", outSelf)
	}

	// Filter tool="write_file" (should match 2: i1 and i4)
	outTool, err := tool.Execute(context.Background(), json.RawMessage(`{"tool": "write_file"}`))
	if err != nil {
		t.Fatalf("Execute tool=write_file: %v", err)
	}
	if !strings.Contains(outTool, "Write history (2 change(s), offset 0):") {
		t.Errorf("expected 2 changes for tool=write_file, got:\n%s", outTool)
	}

	// Filter agent="peer-agent" (should match 1: i2)
	outAgent, err := tool.Execute(context.Background(), json.RawMessage(`{"agent": "peer-agent"}`))
	if err != nil {
		t.Fatalf("Execute agent=peer-agent: %v", err)
	}
	if !strings.Contains(outAgent, "Write history (1 change(s), offset 0):") {
		t.Errorf("expected 1 change for agent=peer-agent, got:\n%s", outAgent)
	}

	// Filter file="file1.txt" (should match 3: i1, i2, i3)
	outFile, err := tool.Execute(context.Background(), json.RawMessage(`{"file": "file1.txt"}`))
	if err != nil {
		t.Fatalf("Execute file=file1.txt: %v", err)
	}
	if !strings.Contains(outFile, "Write history (3 change(s), offset 0):") {
		t.Errorf("expected 3 changes for file=file1.txt, got:\n%s", outFile)
	}

	// Paging: limit=2, offset=1
	outPaged, err := tool.Execute(context.Background(), json.RawMessage(`{"limit": 2, "offset": 1}`))
	if err != nil {
		t.Fatalf("Execute paging: %v", err)
	}
	if !strings.Contains(outPaged, "Write history (2 change(s), offset 1):") {
		t.Errorf("expected 2 changes at offset 1, got:\n%s", outPaged)
	}
}

func TestWriteHistory_Show_SeqAndCallID(t *testing.T) {
	ws := paths.Canonical(t.TempDir())
	dbPath := setupTestHistoryDB(t, ws)
	setupTestStatsDB(t, ws)

	tool := NewWriteHistory().
		WithWorkspace(func(_ context.Context) string { return ws }).
		WithSelfSession(func() string { return "sess-alpha" }).
		withOpeners(
			func() (*history.Reader, error) { return history.OpenReadOnlyAt(dbPath) },
			func() (*stats.DB, error) { return stats.OpenReadOnly() },
		)

	// Show seq 2 (update with diff)
	outSeq2, err := tool.Execute(context.Background(), json.RawMessage(`{"seq": 2}`))
	if err != nil {
		t.Fatalf("Execute show seq 2: %v", err)
	}
	if !strings.Contains(outSeq2, "--- a/") || !strings.Contains(outSeq2, "+++ b/") {
		t.Errorf("expected diff header in show seq 2, got:\n%s", outSeq2)
	}
	if !strings.Contains(outSeq2, "+line 2 updated") {
		t.Errorf("expected diff body in show seq 2, got:\n%s", outSeq2)
	}

	// Show seq 4 (sensitive row - must display marker only, never content)
	outSeq4, err := tool.Execute(context.Background(), json.RawMessage(`{"seq": 4}`))
	if err != nil {
		t.Fatalf("Execute show seq 4: %v", err)
	}
	if !strings.Contains(outSeq4, "[withheld:sensitive]") {
		t.Errorf("expected [withheld:sensitive] marker in show seq 4, got:\n%s", outSeq4)
	}
	if strings.Contains(outSeq4, "SECRET=12345") {
		t.Errorf("SECURITY VIOLATION: withheld secret leaked in show output:\n%s", outSeq4)
	}

	// Show call_id CALL_02 (joins with stats)
	outCall, err := tool.Execute(context.Background(), json.RawMessage(`{"call_id": "CALL_02"}`))
	if err != nil {
		t.Fatalf("Execute show CALL_02: %v", err)
	}
	if !strings.Contains(outCall, "edit_file (42ms, success)") {
		t.Errorf("expected call summary in show output, got:\n%s", outCall)
	}
}

func TestWriteHistory_DiffTruncation(t *testing.T) {
	ws := paths.Canonical(t.TempDir())
	dbPath := filepath.Join(t.TempDir(), "history.db")
	s, err := history.Open(dbPath, history.Options{})
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}

	longBefore := strings.Repeat("A\n", 500)
	longAfter := strings.Repeat("B\n", 500)
	item := history.Item{
		Change: history.Change{
			At:     time.UnixMilli(1000),
			Op:     history.OpUpdate,
			Kind:   history.KindFile,
			Tool:   "edit_file",
			Path:   filepath.Join(ws, "big.txt"),
			Before: history.SideFromBytes([]byte(longBefore)),
			After:  history.SideFromBytes([]byte(longAfter)),
		},
		Workspace: ws,
		CallID:    "CALL_BIG",
	}
	s.Enqueue(item)
	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("history.Close: %v", err)
	}

	tool := NewWriteHistory().
		WithWorkspace(func(_ context.Context) string { return ws }).
		withOpeners(
			func() (*history.Reader, error) { return history.OpenReadOnlyAt(dbPath) },
			nil,
		)

	// Show with small max_diff_bytes (100 bytes)
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"seq": 1, "max_diff_bytes": 100}`))
	if err != nil {
		t.Fatalf("Execute with max_diff_bytes: %v", err)
	}
	if !strings.Contains(out, "… (diff truncated; use max_diff_bytes to expand)") {
		t.Errorf("expected truncation notice in output, got:\n%s", out)
	}
}

func TestWriteHistory_IsolationAndErrors(t *testing.T) {
	ws := paths.Canonical(t.TempDir())
	otherWS := paths.Canonical(t.TempDir())
	dbPath := setupTestHistoryDB(t, ws)

	tool := NewWriteHistory().
		WithWorkspace(func(_ context.Context) string { return otherWS }).
		withOpeners(
			func() (*history.Reader, error) { return history.OpenReadOnlyAt(dbPath) },
			nil,
		)

	// Attempting to show an entry belonging to a different workspace is rejected
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"seq": 1}`))
	if err == nil || !strings.Contains(err.Error(), "belongs to a different workspace") {
		t.Fatalf("expected cross-workspace error, got %v", err)
	}

	// Boundary guard refusal
	guardTool := NewWriteHistory().
		WithWorkspace(func(_ context.Context) string { return ws }).
		WithBoundary(func(_ context.Context, p string) error {
			if strings.Contains(p, "forbidden") {
				return errors.New("forbidden path")
			}
			return nil
		}).
		withOpeners(
			func() (*history.Reader, error) { return history.OpenReadOnlyAt(dbPath) },
			nil,
		)

	_, err = guardTool.Execute(context.Background(), json.RawMessage(`{"file": "forbidden/secret.txt"}`))
	if err == nil || !strings.Contains(err.Error(), "file boundary") {
		t.Fatalf("expected boundary error, got %v", err)
	}

	// No workspace resolved
	noWSTool := NewWriteHistory()
	_, err = noWSTool.Execute(context.Background(), json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "no workspace resolved") {
		t.Fatalf("expected no workspace error, got %v", err)
	}
}

func TestWriteHistory_RelativeFileFilterScopedToRequestedWorkspace(t *testing.T) {
	rootRepo := paths.Canonical(t.TempDir())
	subproject := filepath.Join(rootRepo, "subproject")
	filePath := filepath.Join(subproject, "file1.txt")
	dbPath := filepath.Join(t.TempDir(), "history.db")

	s, err := history.Open(dbPath, history.Options{})
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}
	s.Enqueue(history.Item{
		Change: history.Change{
			Kind:   history.KindFile,
			Op:     history.OpUpdate,
			Tool:   "edit_file",
			Path:   filePath,
			Before: history.SideFromBytes([]byte("old")),
			After:  history.SideFromBytes([]byte("new")),
		},
		Workspace: subproject,
		CallID:    "CALL_SUB",
	})
	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("history.Close: %v", err)
	}

	tool := NewWriteHistory().
		WithWorkspace(func(_ context.Context) string { return rootRepo }).
		withOpeners(
			func() (*history.Reader, error) { return history.OpenReadOnlyAt(dbPath) },
			nil,
		)

	// Query with explicit workspace = subproject, and relative file = "file1.txt"
	query := fmt.Sprintf(`{"workspace": %q, "file": "file1.txt"}`, subproject)
	out, err := tool.Execute(context.Background(), json.RawMessage(query))
	if err != nil {
		t.Fatalf("Execute with relative file: %v", err)
	}

	if !strings.Contains(out, "file1.txt") || !strings.Contains(out, "#1") {
		t.Fatalf("expected output to contain file1.txt (#1), got:\n%s", out)
	}
}

func TestWriteHistory_BoundedCallOutput(t *testing.T) {
	ws := paths.Canonical(t.TempDir())
	dbPath := filepath.Join(t.TempDir(), "history.db")

	s, err := history.Open(dbPath, history.Options{})
	if err != nil {
		t.Fatalf("history.Open: %v", err)
	}

	// Create 20 changes with 16 KiB diffs in a single call (total raw diff ~320 KiB)
	largeDiffContent := strings.Repeat("x", 16*1024)
	for i := range 20 {
		s.Enqueue(history.Item{
			Change: history.Change{
				Kind:   history.KindFile,
				Op:     history.OpUpdate,
				Tool:   "edit_file",
				Path:   filepath.Join(ws, fmt.Sprintf("file_%02d.txt", i)),
				Before: history.SideFromBytes([]byte("")),
				After:  history.SideFromBytes([]byte(largeDiffContent)),
			},
			Workspace: ws,
			CallID:    "CALL_MANY_FILES",
		})
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("history.Close: %v", err)
	}

	tool := NewWriteHistory().
		WithWorkspace(func(_ context.Context) string { return ws }).
		withOpeners(
			func() (*history.Reader, error) { return history.OpenReadOnlyAt(dbPath) },
			nil,
		)

	// Show mode with call_id without pagination: must be bounded by aggregate budget
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"call_id": "CALL_MANY_FILES"}`))
	if err != nil {
		t.Fatalf("Execute for call_id: %v", err)
	}

	// Should not exceed aggregate budget plus safety margin
	if len(out) > maxTotalShowBytes+2048 {
		t.Fatalf("output size %d exceeded maxTotalShowBytes budget %d", len(out), maxTotalShowBytes)
	}

	if !strings.Contains(out, "omitted to stay within response budget") {
		t.Fatalf("expected omitted entries notice in output, got tail:\n%s", out[max(0, len(out)-300):])
	}

	// Paging with offset and limit should work
	pagedOut, err := tool.Execute(context.Background(), json.RawMessage(`{"call_id": "CALL_MANY_FILES", "offset": 2, "limit": 2}`))
	if err != nil {
		t.Fatalf("Execute for paged call_id: %v", err)
	}

	if !strings.Contains(pagedOut, "file_02.txt") || !strings.Contains(pagedOut, "file_03.txt") {
		t.Fatalf("expected paged output to contain file_02 and file_03, got:\n%s", pagedOut)
	}
	if strings.Contains(pagedOut, "file_01.txt") || strings.Contains(pagedOut, "file_04.txt") {
		t.Fatalf("paged output contained out-of-range files, got:\n%s", pagedOut)
	}
}
