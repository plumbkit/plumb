package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/stats"
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

func TestDaemonInfo_IncludesPurposeWhenSet(t *testing.T) {
	d := NewDaemonInfo("sess-1", "swift-falcon", "0.7.x", time.Now()).
		WithPurpose(func() string { return "deploy-fix" })
	out, err := d.Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "purpose:        deploy-fix") {
		t.Errorf("expected purpose line:\n%s", out)
	}
}

func TestDaemonInfo_OmitsPurposeWhenEmpty(t *testing.T) {
	d := NewDaemonInfo("sess-1", "swift-falcon", "0.7.x", time.Now()).
		WithPurpose(func() string { return "" })
	out, err := d.Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(out, "purpose:") {
		t.Errorf("purpose line should be omitted when empty:\n%s", out)
	}
}

// TestRunWithTimeout_ReturnsResultBeforeTimeout verifies the happy path:
// a fast producer's value is returned, not the sentinel.
func TestRunWithTimeout_ReturnsResultBeforeTimeout(t *testing.T) {
	got := runWithTimeout(func() string { return "ok" }, time.Second, "timeout")
	if got != "ok" {
		t.Fatalf("got %q, want %q", got, "ok")
	}
}

// TestRunWithTimeout_ReturnsSentinelOnTimeout verifies a slow producer is
// abandoned and the configured sentinel returned instead. The bound itself
// is tight (50 ms) so the test stays fast while still exercising the path.
func TestRunWithTimeout_ReturnsSentinelOnTimeout(t *testing.T) {
	slow := func() string {
		time.Sleep(500 * time.Millisecond)
		return "never"
	}
	start := time.Now()
	got := runWithTimeout(slow, 50*time.Millisecond, "sentinel")
	elapsed := time.Since(start)
	if got != "sentinel" {
		t.Fatalf("got %q, want %q", got, "sentinel")
	}
	// Should return ~50 ms in, not wait the full 500 ms.
	if elapsed > 250*time.Millisecond {
		t.Fatalf("runWithTimeout waited %s; want close to the 50ms bound", elapsed)
	}
}

// TestSessionLatencyTimeoutConstants pins the daemon_info bound at 250 ms so
// the wired knob (the value daemon_info advertises) cannot silently drift.
func TestSessionLatencyTimeoutConstants(t *testing.T) {
	if sessionLatencyTimeout != 250*time.Millisecond {
		t.Errorf("sessionLatencyTimeout = %s, want 250ms", sessionLatencyTimeout)
	}
	if !strings.Contains(sessionLatencyTimeoutMsg, "unavailable") {
		t.Errorf("timeout sentinel %q should explain why stats are missing", sessionLatencyTimeoutMsg)
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
