package tools

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/stats"
)

// TestDaemonInfo_ReportsRuntimeAndArch pins the two facts daemon_info absorbed
// when the `version` tool was merged into it: the Go runtime and the os/arch
// pair. They are unconditional — a bug report must be fillable from this one
// call, and the `version` alias resolves here.
func TestDaemonInfo_ReportsRuntimeAndArch(t *testing.T) {
	d := NewDaemonInfo("", "swift-falcon", "1.2.3", time.Now())
	out, err := d.Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, want := range []string{
		"daemon version: 1.2.3",
		"go runtime:     " + runtime.Version(),
		fmt.Sprintf("os/arch:        %s/%s", runtime.GOOS, runtime.GOARCH),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("daemon_info output missing %q:\n%s", want, out)
		}
	}
}

// TestDaemonInfo_ReportsSourceCommit pins the source-commit row. It joins the
// unconditional bug-report set (daemon version, go runtime, os/arch) because a
// version string alone cannot say which commit a daemon was built from — and an
// unstamped build must say "unknown" out loud rather than omit the row or, worse,
// read as clean.
func TestDaemonInfo_ReportsSourceCommit(t *testing.T) {
	const rev = "4c6e4da9d8fafc5ca36d762460caf6abf46c5ca6"
	tests := []struct {
		name string
		tool *daemonInfo
		want string
	}{
		{
			name: "unstamped",
			tool: NewDaemonInfo("", "swift-falcon", "1.2.3", time.Now()),
			want: "source commit:  unknown (binary built without a revision stamp)",
		},
		{
			name: "clean",
			tool: NewDaemonInfo("", "swift-falcon", "1.2.3", time.Now()).
				WithSourceRevision(rev, false, true),
			want: "source commit:  " + rev,
		},
		{
			name: "dirty",
			tool: NewDaemonInfo("", "swift-falcon", "1.2.3", time.Now()).
				WithSourceRevision(rev, true, true),
			want: "source commit:  " + rev + " (dirty)",
		},
		{
			name: "dirty state unknown",
			tool: NewDaemonInfo("", "swift-falcon", "1.2.3", time.Now()).
				WithSourceRevision(rev, false, false),
			want: "source commit:  " + rev + " (dirty state unknown)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tc.tool.Execute(context.Background(), nil)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			// Matched with the trailing newline so the clean case cannot be
			// satisfied by a line that goes on to say "(dirty)".
			if !strings.Contains(out, tc.want+"\n") {
				t.Errorf("daemon_info output missing %q:\n%s", tc.want, out)
			}
		})
	}
}

// TestDaemonInfo_UptimeSpansSuspend pins the monotonic strip: startedAt must
// not carry a monotonic clock reading, or time.Since would exclude
// system-suspend time (CLOCK_MONOTONIC stops while suspended) and uptime
// would underreport wall-clock elapsed. time.Time's == compares the monotonic
// reading, so equality with its own Round(0) proves none is present.
func TestDaemonInfo_UptimeSpansSuspend(t *testing.T) {
	d := NewDaemonInfoFunc("sess-1", func() string { return "swift-falcon" }, "0.15.x", time.Now())
	if d.startedAt != d.startedAt.Round(0) {
		t.Errorf("startedAt carries a monotonic reading; uptime would exclude suspend time")
	}
}

// TestDaemonInfo_ProtocolRow pins the protocol-negotiation rendering: a
// mismatch names both revisions (the fleet-visibility signal), a match stays
// quiet about the offer, and an unwired or pre-initialize tool omits the rows.
func TestDaemonInfo_ProtocolRow(t *testing.T) {
	newTool := func() *daemonInfo {
		return NewDaemonInfo("", "swift-falcon", "1.2.3", time.Now())
	}
	tests := []struct {
		name   string
		wire   func(*daemonInfo) *daemonInfo
		want   []string
		absent []string
	}{
		{
			name: "wired with mismatch shows both revisions and caps",
			wire: func(d *daemonInfo) *daemonInfo {
				return d.WithProtocol(func() ProtocolStatus {
					return ProtocolStatus{
						Offered:      "2025-11-25",
						Answered:     "2024-11-05",
						Capabilities: []string{"elicitation", "roots.listChanged"},
					}
				})
			},
			want: []string{
				"protocol:       2024-11-05 (client offered 2025-11-25)",
				"client caps:    elicitation, roots.listChanged",
			},
		},
		{
			name: "wired with matching offer shows the answered revision only",
			wire: func(d *daemonInfo) *daemonInfo {
				return d.WithProtocol(func() ProtocolStatus {
					return ProtocolStatus{Offered: "2024-11-05", Answered: "2024-11-05"}
				})
			},
			want:   []string{"protocol:       2024-11-05"},
			absent: []string{"client offered", "client caps:"},
		},
		{
			name:   "unwired omits the protocol rows",
			wire:   func(d *daemonInfo) *daemonInfo { return d },
			absent: []string{"protocol:", "client caps:"},
		},
		{
			name: "wired but unanswered (pre-initialize) omits the rows",
			wire: func(d *daemonInfo) *daemonInfo {
				return d.WithProtocol(func() ProtocolStatus { return ProtocolStatus{} })
			},
			absent: []string{"protocol:", "client caps:"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tc.wire(newTool()).Execute(context.Background(), nil)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("daemon_info output missing %q:\n%s", want, out)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(out, absent) {
					t.Errorf("daemon_info output unexpectedly contains %q:\n%s", absent, out)
				}
			}
		})
	}
}

func TestFormatUptime(t *testing.T) {
	tests := []struct {
		up   time.Duration
		want string
	}{
		{26*time.Hour + 3*time.Minute + 42*time.Second, "26h 3m"},
		{5*time.Minute + 7*time.Second, "5m 7s"},
		{42 * time.Second, "42s"},
		// Boundaries and the clock-went-backwards case: wall-clock uptime can go
		// negative on an NTP correction, and a "-3m -20s" uptime is nonsense.
		{time.Hour, "1h 0m"},
		{time.Minute, "1m 0s"},
		{0, "0s"},
		{-3*time.Minute - 20*time.Second, "0s"},
		{-2 * time.Hour, "0s"},
	}
	for _, tt := range tests {
		if got := formatUptime(tt.up); got != tt.want {
			t.Errorf("formatUptime(%s) = %q, want %q", tt.up, got, tt.want)
		}
	}
}

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

// TestDaemonInfoLSPStatusRow covers the three-state lsp row (ready / warming /
// none attached), the routed-language suffix, and the row's omission when no
// accessor is wired.
func TestDaemonInfoLSPStatusRow(t *testing.T) {
	tests := []struct {
		name   string
		status *LSPStatus // nil ⇒ accessor unwired
		want   string     // "" ⇒ the row must be absent
	}{
		{"ready", &LSPStatus{Language: "go"}, "lsp:            ready (go)"},
		{"ready push mode stays quiet", &LSPStatus{Language: "go", DiagnosticsMode: "push"}, "lsp:            ready (go)"},
		{"ready pull mode is surfaced", &LSPStatus{Language: "go", DiagnosticsMode: "pull"}, "lsp:            ready (go, diagnostics: pull)"},
		{"ready hybrid mode is surfaced", &LSPStatus{Language: "go", DiagnosticsMode: "hybrid"}, "lsp:            ready (go, diagnostics: hybrid)"},
		{"ready pull-unavailable is surfaced", &LSPStatus{Language: "go", DiagnosticsMode: "pull-requested-but-unavailable"}, "lsp:            ready (go, diagnostics: pull-requested-but-unavailable)"},
		{"warming with elapsed", &LSPStatus{Language: "go", Warming: true, Elapsed: 3200 * time.Millisecond}, "lsp:            warming (go, ~3s elapsed)"},
		{"warming without elapsed", &LSPStatus{Language: "go", Warming: true}, "lsp:            warming (go)"},
		{"empty language means none attached", &LSPStatus{}, "lsp:            none attached"},
		{"LanguageNone means none attached", &LSPStatus{Language: "none"}, "lsp:            none attached"},
		// PLAN-258: "none attached" alone told agents no language server was
		// serving them while a routed one was answering every query.
		{"no primary but routed", &LSPStatus{Routed: []string{"go"}}, "lsp:            none attached (primary); routed: go"},
		{"no primary, several routed", &LSPStatus{Routed: []string{"go", "html"}}, "lsp:            none attached (primary); routed: go, html"},
		{"ready with a routed secondary", &LSPStatus{Language: "go", Routed: []string{"html"}}, "lsp:            ready (go); routed: html"},
		{"warming with a routed secondary", &LSPStatus{Language: "go", Warming: true, Routed: []string{"html"}}, "lsp:            warming (go); routed: html"},
		{"accessor unwired omits the row", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := NewDaemonInfo("sess-1", "swift-falcon", "0.7.x", time.Now())
			if tt.status != nil {
				s := *tt.status
				d = d.WithLSPStatus(func() LSPStatus { return s })
			}
			out, err := d.Execute(context.Background(), nil)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if tt.want == "" {
				if strings.Contains(out, "lsp:") {
					t.Errorf("lsp row should be omitted when no accessor is wired:\n%s", out)
				}
				return
			}
			if !strings.Contains(out, tt.want) {
				t.Errorf("missing %q:\n%s", tt.want, out)
			}
		})
	}
}

// TestDaemonInfo_ToolProfile covers the three states of the daemon_info
// profile line: wired lean (reason + hidden count), wired full (reason, no
// hidden count), and unwired (no accessor ⇒ no profile line at all).
func TestDaemonInfo_ToolProfile(t *testing.T) {
	t.Run("wired lean", func(t *testing.T) {
		d := NewDaemonInfo("sess-1", "swift-falcon", "0.7.x", time.Now()).
			WithToolProfile(func() (string, int, string) { return "lean", 33, "verified-deferred-discovery" })
		out, err := d.Execute(context.Background(), nil)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !strings.Contains(out, "Tool profile: lean (reason: verified-deferred-discovery), 33 tools hidden") {
			t.Errorf("missing lean profile line:\n%s", out)
		}
	})

	t.Run("wired full", func(t *testing.T) {
		d := NewDaemonInfo("sess-1", "swift-falcon", "0.7.x", time.Now()).
			WithToolProfile(func() (string, int, string) { return "full", 0, "schema-discovery-only-client" })
		out, err := d.Execute(context.Background(), nil)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !strings.Contains(out, "Tool profile: full (reason: schema-discovery-only-client)") {
			t.Errorf("missing full profile line:\n%s", out)
		}
		if strings.Contains(out, "tools hidden") {
			t.Errorf("full profile should not mention a hidden tool count:\n%s", out)
		}
	})

	t.Run("unwired silent", func(t *testing.T) {
		d := NewDaemonInfo("sess-1", "swift-falcon", "0.7.x", time.Now())
		out, err := d.Execute(context.Background(), nil)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if strings.Contains(out, "Tool profile:") {
			t.Errorf("profile line should be omitted when no accessor is wired:\n%s", out)
		}
	})
}

// TestDaemonInfo_PinProvenanceLine covers the optional workspace-pin
// provenance line: wired with a non-zero provenance, unwired, and wired but
// returning the zero value (which itself renders "", so the line must be
// absent the same as unwired).
func TestDaemonInfo_PinProvenanceLine(t *testing.T) {
	t.Run("wired non-zero", func(t *testing.T) {
		d := NewDaemonInfo("sess-1", "swift-falcon", "0.7.x", time.Now()).
			WithPinProvenance(func() PinProvenance {
				return PinProvenance{
					Source:   "session_start",
					At:       time.Now().Add(-2*time.Minute - 5*time.Second),
					Previous: "/x/cvex",
				}
			})
		out, err := d.Execute(context.Background(), nil)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !strings.Contains(out, "Pin provenance:") || !strings.Contains(out, "via session_start") {
			t.Errorf("missing pin provenance line:\n%s", out)
		}
	})

	t.Run("unwired", func(t *testing.T) {
		d := NewDaemonInfo("sess-1", "swift-falcon", "0.7.x", time.Now())
		out, err := d.Execute(context.Background(), nil)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if strings.Contains(out, "Pin provenance:") {
			t.Errorf("pin provenance line should be absent when unwired:\n%s", out)
		}
	})

	t.Run("wired zero value", func(t *testing.T) {
		d := NewDaemonInfo("sess-1", "swift-falcon", "0.7.x", time.Now()).
			WithPinProvenance(func() PinProvenance { return PinProvenance{} })
		out, err := d.Execute(context.Background(), nil)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if strings.Contains(out, "Pin provenance:") {
			t.Errorf("pin provenance line should be absent for the zero value:\n%s", out)
		}
	})
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
