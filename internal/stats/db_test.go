package stats

import (
	"path/filepath"
	"testing"
	"time"
)

func TestRecent_RoundTripsErrorAndByteFields(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "stats.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	now := time.Now()
	db.Record(Call{
		SessionID:   "sess-1",
		Workspace:   "/tmp/ws",
		Tool:        "find_symbol",
		CalledAt:    now,
		DurationMs:  42,
		InputBytes:  128,
		OutputBytes: 4096,
		Success:     false,
		ErrorMsg:    "uri is required",
	})

	got, err := db.Recent(10, Filter{})
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	c := got[0]
	if c.Tool != "find_symbol" {
		t.Errorf("Tool = %q, want find_symbol", c.Tool)
	}
	if c.Success {
		t.Errorf("Success = true, want false")
	}
	if c.ErrorMsg != "uri is required" {
		t.Errorf("ErrorMsg = %q, want %q", c.ErrorMsg, "uri is required")
	}
	if c.InputBytes != 128 {
		t.Errorf("InputBytes = %d, want 128", c.InputBytes)
	}
	if c.OutputBytes != 4096 {
		t.Errorf("OutputBytes = %d, want 4096", c.OutputBytes)
	}
	if c.DurationMs != 42 {
		t.Errorf("DurationMs = %d, want 42", c.DurationMs)
	}
}

func TestRecent_SuccessfulCallHasEmptyErrorMsg(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "stats.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	db.Record(Call{
		SessionID:  "sess-1",
		Workspace:  "/tmp/ws",
		Tool:       "list_symbols",
		CalledAt:   time.Now(),
		DurationMs: 5,
		Success:    true,
	})

	got, _ := db.Recent(10, Filter{})
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].ErrorMsg != "" {
		t.Errorf("successful call ErrorMsg = %q, want empty", got[0].ErrorMsg)
	}
	if !got[0].Success {
		t.Errorf("Success = false, want true")
	}
}
