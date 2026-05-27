package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/golimpio/plumb/internal/stats"
)

func TestDaemonInfo_OmitsConfigStatusWhenUnset(t *testing.T) {
	d := NewDaemonInfo("sess-1", "swift-falcon", "0.7.x", time.Now())
	out, err := d.Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(out, "config generation") {
		t.Errorf("config status should be omitted when no provider is wired:\n%s", out)
	}
}

func TestDaemonInfo_IncludesConfigStatus(t *testing.T) {
	reloaded := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	d := NewDaemonInfo("sess-1", "swift-falcon", "0.7.x", time.Now()).
		WithConfigStatus(func() ConfigStatus {
			return ConfigStatus{Generation: 5, LastReloaded: reloaded, RestartNeeded: true}
		})
	out, err := d.Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "config generation: 5") {
		t.Errorf("missing generation line:\n%s", out)
	}
	if !strings.Contains(out, "restart needed:    yes") {
		t.Errorf("expected restart-needed yes:\n%s", out)
	}
}

func TestDaemonInfo_RestartNeededNo(t *testing.T) {
	d := NewDaemonInfo("sess-1", "swift-falcon", "0.7.x", time.Now()).
		WithConfigStatus(func() ConfigStatus {
			return ConfigStatus{Generation: 1, LastReloaded: time.Now(), RestartNeeded: false}
		})
	out, err := d.Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "restart needed:    no") {
		t.Errorf("expected restart-needed no:\n%s", out)
	}
}

func TestFormatSessionLatency(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	db, err := stats.Open()
	if err != nil {
		t.Fatalf("stats.Open: %v", err)
	}
	defer db.Close()

	now := time.Now()
	calls := []stats.Call{
		{SessionID: "sess-x", Workspace: "/w", Tool: "read_file", CalledAt: now, DurationMs: 5, Success: true},
		{SessionID: "sess-x", Workspace: "/w", Tool: "edit_file", CalledAt: now, DurationMs: 280, Success: true},
		{SessionID: "other", Workspace: "/w", Tool: "git", CalledAt: now, DurationMs: 900, Success: true},
	}
	for _, c := range calls {
		if err := db.Record(c); err != nil {
			t.Fatalf("Record %s: %v", c.Tool, err)
		}
	}

	out := formatSessionLatency("sess-x")
	if !strings.Contains(out, "this session:") || !strings.Contains(out, "2 tool call(s)") {
		t.Fatalf("want session header with 2 calls:\n%s", out)
	}
	if !strings.Contains(out, "edit_file") || !strings.Contains(out, "280ms") {
		t.Fatalf("want slowest edit_file/280ms:\n%s", out)
	}
	if strings.Contains(out, "git") {
		t.Fatalf("another session's call leaked into the sess-x block:\n%s", out)
	}
	if formatSessionLatency("") != "" {
		t.Fatalf("empty session id should yield an empty block")
	}
}
