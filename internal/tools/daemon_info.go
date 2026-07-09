package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/stats"
)

// ConfigStatus is a snapshot of the live config store, surfaced by daemon_info.
type ConfigStatus struct {
	Generation    uint64    // monotonic; increments on every config reload
	LastReloaded  time.Time // time of the most recent reload
	RestartNeeded bool      // a restart-bound setting changed since daemon start
}

// LSPStatus is a snapshot of the session's primary language-server state,
// surfaced by daemon_info as a three-state row: ready, warming, or none
// attached.
type LSPStatus struct {
	Language string        // attached LSP language; "" or "none" means no server is attached
	Warming  bool          // the server is attached but its handshake has not completed
	Elapsed  time.Duration // how long the warm-up has been running; 0 when unknown
}

// daemonInfo returns session and daemon metadata to the calling agent.
type daemonInfo struct {
	sessID        string
	name          func() string
	daemonVersion string
	startedAt     time.Time
	configStatus  func() ConfigStatus // optional; nil when no store is wired
	purpose       func() string       // optional; nil when no purpose accessor is wired
	lspStatus     func() LSPStatus    // optional; nil when no LSP accessor is wired
}

// WithLSPStatus wires an accessor returning the session's live language-server
// state. Nil-safe: when unset, daemon_info omits the lsp row. Returns the
// receiver for chaining.
func (t *daemonInfo) WithLSPStatus(fn func() LSPStatus) *daemonInfo {
	t.lspStatus = fn
	return t
}

// WithPurpose wires an accessor returning this session's human-readable purpose
// tag ("" when unset). Nil-safe: when unset or returning empty, daemon_info omits
// the purpose line. Returns the receiver for chaining.
func (t *daemonInfo) WithPurpose(fn func() string) *daemonInfo {
	t.purpose = fn
	return t
}

// WithConfigStatus wires a provider for live config-store state (generation,
// last reload, restart-needed). Nil-safe: when unset, daemon_info omits those
// lines. Returns the receiver for chaining.
func (t *daemonInfo) WithConfigStatus(fn func() ConfigStatus) *daemonInfo {
	t.configStatus = fn
	return t
}

// NewDaemonInfo creates a tool that exposes session and daemon metadata.
// sessID and sessName identify the current MCP connection; daemonVersion and
// startedAt describe the daemon process itself.
func NewDaemonInfo(sessID, sessName, daemonVersion string, startedAt time.Time) *daemonInfo {
	return NewDaemonInfoFunc(sessID, func() string { return sessName }, daemonVersion, startedAt)
}

// NewDaemonInfoFunc creates a daemon_info tool whose session name can change
// during the session.
func NewDaemonInfoFunc(sessID string, name func() string, daemonVersion string, startedAt time.Time) *daemonInfo {
	return &daemonInfo{
		sessID:        sessID,
		name:          name,
		daemonVersion: daemonVersion,
		startedAt:     startedAt,
	}
}

func (t *daemonInfo) Name() string { return "daemon_info" }

func (t *daemonInfo) Description() string {
	return "Returns metadata about the current MCP session and daemon process: " +
		"session name (e.g. swift-falcon), session ID, daemon version, start timestamp, and uptime, " +
		"plus live config-store state (generation, last reload time, and whether a restart is needed " +
		"for a pending restart-bound change). " +
		"It also reports this session's total tool-call count and its slowest calls " +
		"(per-call durations from recorded stats). " +
		"Use this to identify which session you are operating in or to verify the daemon state."
}

func (t *daemonInfo) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
}

func (t *daemonInfo) Execute(_ context.Context, _ json.RawMessage) (string, error) {
	up := time.Since(t.startedAt)
	h := int(up.Hours())
	m := int(up.Minutes()) % 60
	s := int(up.Seconds()) % 60
	var upStr string
	switch {
	case h > 0:
		upStr = fmt.Sprintf("%dh %dm", h, m)
	case m > 0:
		upStr = fmt.Sprintf("%dm %ds", m, s)
	default:
		upStr = fmt.Sprintf("%ds", s)
	}
	out := fmt.Sprintf(
		"session name:   %s\nsession id:     %s\ndaemon version: %s\nstarted at:     %s\nuptime:         %s",
		t.name(),
		t.sessID,
		t.daemonVersion,
		t.startedAt.Format(time.RFC3339),
		upStr,
	)
	if t.purpose != nil {
		if p := t.purpose(); p != "" {
			out += fmt.Sprintf("\npurpose:        %s", p)
		}
	}
	if t.lspStatus != nil {
		out += fmt.Sprintf("\nlsp:            %s", formatLSPStatusRow(t.lspStatus()))
	}
	if t.configStatus != nil {
		cs := t.configStatus()
		restart := "no"
		if cs.RestartNeeded {
			restart = "yes — restart the daemon for the pending change to take effect"
		}
		out += fmt.Sprintf(
			"\nconfig generation: %d\nconfig reloaded:   %s\nrestart needed:    %s",
			cs.Generation,
			cs.LastReloaded.Format(time.RFC3339),
			restart,
		)
	}
	out += formatSessionLatency(t.sessID)
	return out, nil
}

// formatLSPStatusRow renders the three-state language-server row: ready,
// warming (with elapsed time when known), or none attached.
func formatLSPStatusRow(s LSPStatus) string {
	if s.Language == "" || s.Language == "none" {
		return "none attached"
	}
	if !s.Warming {
		return fmt.Sprintf("ready (%s)", s.Language)
	}
	if d := roundLSPElapsed(s.Elapsed); d > 0 {
		return fmt.Sprintf("warming (%s, ~%s elapsed)", s.Language, d)
	}
	return fmt.Sprintf("warming (%s)", s.Language)
}

// roundLSPElapsed rounds a warm-up duration for display: 100 ms precision under
// a second, whole seconds beyond. Local because the cli package's equivalent
// helper is unexported.
func roundLSPElapsed(d time.Duration) time.Duration {
	if d < time.Second {
		return d.Round(100 * time.Millisecond)
	}
	return d.Round(time.Second)
}

// sessionLatencyTimeout caps how long daemon_info will wait for its optional
// stats lookup. Beyond this, daemon_info returns core daemon metadata plus the
// timeout sentinel rather than blocking the MCP response.
const sessionLatencyTimeout = 250 * time.Millisecond

const sessionLatencyTimeoutMsg = "\nstats:          unavailable (stats DB query timed out)"

// formatSessionLatency renders this session's call count and slowest calls from
// the global stats DB, scoped by session id (the session_id column equals the
// value daemon_info holds, so the filter is exact). Returns "" when stats are
// unavailable or this session has no recorded calls yet (e.g. daemon_info is the
// first call of the session).
func formatSessionLatency(sessID string) string {
	return runWithTimeout(
		func() string { return formatSessionLatencySync(sessID) },
		sessionLatencyTimeout, sessionLatencyTimeoutMsg,
	)
}

// runWithTimeout invokes fn on a goroutine and returns either its result or
// timeoutMsg if fn does not return within timeout. The send channel is buffered
// so the producer never leaks on the timeout path.
func runWithTimeout(fn func() string, timeout time.Duration, timeoutMsg string) string {
	done := make(chan string, 1)
	go func() { done <- fn() }()
	select {
	case out := <-done:
		return out
	case <-time.After(timeout):
		return timeoutMsg
	}
}

func formatSessionLatencySync(sessID string) string {
	if sessID == "" {
		return ""
	}
	db, err := stats.SharedReadOnly()
	if err != nil || db == nil {
		return ""
	}
	filter := stats.Filter{SessionID: sessID}
	summary, err := db.Summary(filter)
	if err != nil || len(summary) == 0 {
		return ""
	}
	var calls int64
	for _, s := range summary {
		calls += s.Calls
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "\n\nthis session:   %d tool call(s)", calls)
	if slow, err := db.Slowest(5, filter); err == nil && len(slow) > 0 {
		sb.WriteString("\nslowest calls:")
		now := time.Now()
		for _, c := range slow {
			fmt.Fprintf(&sb, "\n  %-18s %5dms  (%s ago)", c.Tool, c.DurationMs, humaniseAge(now.Sub(c.CalledAt)))
		}
	}
	return sb.String()
}

// humaniseAge renders a duration as a compact age string (e.g. "5s", "3m", "2h").
func humaniseAge(d time.Duration) string {
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}
