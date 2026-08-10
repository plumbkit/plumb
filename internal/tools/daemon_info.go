package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
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
	// DiagnosticsMode is the resolved negotiation mode (push / pull / hybrid /
	// pull-requested-but-unavailable), or "" when unresolved. Surfaced on the
	// ready row only when it is a non-default value (anything but push), so a
	// default push setup stays quiet.
	DiagnosticsMode string
	// Routed lists the non-primary languages whose servers have actually served
	// this session. It is a SEPARATE fact from Language, not a fallback for it: a
	// workspace with no detectable primary language serves files purely through
	// per-file routing, and reporting only the (absent) primary told agents no
	// language server was attached while one was answering their queries. Empty
	// for the common single-language session.
	Routed []string
}

// daemonInfo returns session and daemon metadata to the calling agent.
type daemonInfo struct {
	sessID        string
	name          func() string
	daemonVersion string
	startedAt     time.Time
	configStatus  func() ConfigStatus                                // optional; nil when no store is wired
	purpose       func() string                                      // optional; nil when no purpose accessor is wired
	lspStatus     func() LSPStatus                                   // optional; nil when no LSP accessor is wired
	toolProfile   func() (profile string, hidden int, reason string) // optional; nil when no tool-profile accessor is wired
	pinProvenance func() PinProvenance                               // optional; nil when no provenance accessor is wired
	sourceRev     sourceRevision                                     // zero value means "not stamped"; renders as unknown
}

// sourceRevision is the build-time provenance of the running daemon binary: the
// commit it was built from, and whether that tree was clean. The daemon resolves
// it (ldflags stamps first, embedded VCS settings second) and hands the result
// down as primitives, since internal/tools sits below internal/cli.
//
// dirtyKnown is separate from dirty on purpose: an unstamped build must never be
// reported as clean.
type sourceRevision struct {
	revision   string
	dirty      bool
	dirtyKnown bool
}

// String renders the source-revision row's value. The revision is shown in full
// (this is a bug-report field, not a status line), with the dirty state appended
// only when the build actually knows it.
func (s sourceRevision) String() string {
	if s.revision == "" {
		return "unknown (binary built without a revision stamp)"
	}
	switch {
	case s.dirtyKnown && s.dirty:
		return s.revision + " (dirty)"
	case s.dirtyKnown:
		return s.revision
	default:
		return s.revision + " (dirty state unknown)"
	}
}

// WithSourceRevision records the source commit this binary was built from and
// whether that tree was dirty (dirtyKnown false when the build could not tell).
// An empty revision leaves the row reporting "unknown". Returns the receiver for
// chaining.
func (t *daemonInfo) WithSourceRevision(revision string, dirty, dirtyKnown bool) *daemonInfo {
	t.sourceRev = sourceRevision{revision: revision, dirty: dirty, dirtyKnown: dirtyKnown}
	return t
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

// WithToolProfile wires an accessor returning the connection's resolved tool
// profile ("lean"/"full"), the number of tools hidden from tools/list under
// it, and the stable kebab-case resolution reason (see resolveToolProfile).
// Nil-safe: when unset, daemon_info omits the tool profile line entirely.
// Returns the receiver for chaining.
func (t *daemonInfo) WithToolProfile(fn func() (profile string, hidden int, reason string)) *daemonInfo {
	t.toolProfile = fn
	return t
}

// WithPinProvenance wires an accessor returning this connection's current
// workspace-pin provenance. Nil-safe: when unset, or when the returned value
// is the zero PinProvenance, daemon_info omits the provenance line. Returns
// the receiver for chaining.
func (t *daemonInfo) WithPinProvenance(fn func() PinProvenance) *daemonInfo {
	t.pinProvenance = fn
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
		// Round(0) strips any monotonic clock reading so the uptime diff
		// uses wall-clock semantics and spans system suspend (the daemon
		// already strips it at capture; this keeps the tool correct for
		// any other caller).
		startedAt: startedAt.Round(0),
	}
}

func (t *daemonInfo) Name() string { return "daemon_info" }

func (t *daemonInfo) Description() string {
	return "Returns metadata about the current MCP session and daemon process: " +
		"session name (e.g. swift-falcon), session ID, daemon version, the source commit the binary " +
		"was built from (with a dirty marker, or an explicit unknown), Go runtime, OS/arch, " +
		"start timestamp, and uptime, " +
		"plus live config-store state (generation, last reload time, and whether a restart is needed " +
		"for a pending restart-bound change), and — when available — this connection's workspace-pin " +
		"provenance (how, when, and from where the pin was last set). " +
		"It also reports this session's total tool-call count and its slowest calls " +
		"(per-call durations from recorded stats). " +
		"Use this to identify which session you are operating in or to verify the daemon state."
}

func (t *daemonInfo) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
}

// formatUptime renders a daemon uptime as "Nh Nm", "Nm Ns", or "Ns"
// depending on magnitude. A negative duration is clamped to zero: uptime is now
// wall-clock (the monotonic reading is stripped so it spans suspend), which
// means a backwards clock step — an NTP correction — can put startedAt in the
// future, and "-3m -20s" is worse than "0s".
func formatUptime(up time.Duration) string {
	if up < 0 {
		up = 0
	}
	h := int(up.Hours())
	m := int(up.Minutes()) % 60
	s := int(up.Seconds()) % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm %ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

func (t *daemonInfo) Execute(_ context.Context, _ json.RawMessage) (string, error) {
	// The go runtime and os/arch rows are the two facts the retired `version`
	// tool reported that daemon_info lacked; they are unconditional so a bug
	// report can be filed from this one call. The source commit joins that set
	// for the same reason — a version string alone cannot say which commit the
	// running daemon was built from — and is likewise unconditional, so an
	// unstamped build says "unknown" out loud instead of omitting the row.
	out := fmt.Sprintf(
		"session name:   %s\nsession id:     %s\ndaemon version: %s\nsource commit:  %s\ngo runtime:     %s\nos/arch:        %s/%s\nstarted at:     %s\nuptime:         %s",
		t.name(),
		t.sessID,
		t.daemonVersion,
		t.sourceRev,
		runtime.Version(),
		runtime.GOOS,
		runtime.GOARCH,
		t.startedAt.Format(time.RFC3339),
		formatUptime(time.Since(t.startedAt)),
	)
	if t.pinProvenance != nil {
		if prov := t.pinProvenance().String(); prov != "" {
			out += "\n" + prov
		}
	}
	if t.purpose != nil {
		if p := t.purpose(); p != "" {
			out += "\npurpose:        " + p
		}
	}
	if t.lspStatus != nil {
		out += "\nlsp:            " + formatLSPStatusRow(t.lspStatus())
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
	if t.toolProfile != nil {
		profile, hidden, reason := t.toolProfile()
		out += fmt.Sprintf("\nTool profile: %s (reason: %s)", profile, reason)
		if profile == "lean" {
			out += fmt.Sprintf(", %d tools hidden", hidden)
		}
	}
	out += formatSessionLatency(t.sessID)
	return out, nil
}

// formatLSPStatusRow renders the language-server row: the primary's three-state
// status (ready, warming with elapsed time when known, or none attached) plus
// the set of languages routing has actually served this session. On the ready
// row a non-default diagnostics mode (anything but push) is appended, so a
// default push setup stays quiet.
func formatLSPStatusRow(s LSPStatus) string {
	return primaryLSPRow(s) + routedSuffix(s.Routed)
}

// primaryLSPRow renders the primary language server's own state. "none attached"
// is qualified as "(primary)" when something IS routed, so the row never reads as
// "no language server is serving you" while one is.
func primaryLSPRow(s LSPStatus) string {
	if s.Language == "" || s.Language == "none" {
		if len(s.Routed) > 0 {
			return "none attached (primary)"
		}
		return "none attached"
	}
	if !s.Warming {
		if s.DiagnosticsMode != "" && s.DiagnosticsMode != "push" {
			return fmt.Sprintf("ready (%s, diagnostics: %s)", s.Language, s.DiagnosticsMode)
		}
		return fmt.Sprintf("ready (%s)", s.Language)
	}
	if d := roundLSPElapsed(s.Elapsed); d > 0 {
		return fmt.Sprintf("warming (%s, ~%s elapsed)", s.Language, d)
	}
	return fmt.Sprintf("warming (%s)", s.Language)
}

// routedSuffix names the languages serving this session through per-file routing,
// appended to whatever the primary row says. Empty (the common case) leaves every
// row byte-identical to the pre-routed-facet output.
func routedSuffix(routed []string) string {
	if len(routed) == 0 {
		return ""
	}
	return "; routed: " + strings.Join(routed, ", ")
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
