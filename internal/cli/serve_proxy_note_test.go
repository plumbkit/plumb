package cli

// serve_proxy_note_test.go — the one-shot note an agent reads after a
// transparent reconnect.
//
// Split from serve_proxy_test.go (which covers the proxy's transport
// behaviour — reconnection, in-flight requests, heartbeats) because the note is
// a distinct contract with a distinct failure mode: it is the only thing that
// tells an agent what just happened to its session, so a note that overstates
// what the proxy observed misleads exactly the reader who cannot check.

import (
	"strings"
	"testing"
)

func TestInjectReconnectNote(t *testing.T) {
	t.Parallel()

	// Well-formed tools/call result: note appended, original content preserved.
	good := []byte(`{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"hello"}]}}`)
	out, ok := injectReconnectNote(good, "v9.9.9", "v9.9.9", true, reconnectOutcome{})
	if !ok {
		t.Fatal("expected injection into a well-formed tools/call result")
	}
	s := string(out)
	if !strings.Contains(s, "hello") || !strings.Contains(s, "connection re-established") || !strings.Contains(s, "v9.9.9") {
		t.Fatalf("expected note appended alongside original content, got %q", s)
	}

	// Fail-safe shapes: each returns the input unchanged with ok=false.
	for _, c := range []struct {
		name  string
		frame string
	}{
		{"error response", `{"jsonrpc":"2.0","id":3,"error":{"code":-32000,"message":"x"}}`},
		{"result without content", `{"jsonrpc":"2.0","id":3,"result":{"method":"x"}}`},
		{"not json", `not json at all`},
		{"content not array", `{"jsonrpc":"2.0","id":3,"result":{"content":"oops"}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, ok := injectReconnectNote([]byte(c.frame), "v1", "v1", true, reconnectOutcome{})
			if ok {
				t.Fatalf("expected ok=false for %s", c.name)
			}
			if string(got) != c.frame {
				t.Fatalf("a refused injection must return the frame unchanged, got %q", got)
			}
		})
	}
}

func TestReconnectNoteText(t *testing.T) {
	t.Parallel()

	// Same versions: plain note, no proxy-lag hint.
	same := reconnectNoteText("1.2.3", "1.2.3", true, reconnectOutcome{})
	if !strings.Contains(same, "(daemon 1.2.3)") || strings.Contains(same, "serve proxy") {
		t.Errorf("same-version note wrong: %q", same)
	}
	// The note must name the workspace pin, not only read-tracking/caches — the
	// original field report's sharpest point (a reconnect had silently changed
	// which repository writes landed in).
	if !strings.Contains(same, "workspace") {
		t.Errorf("note does not mention the workspace pin: %q", same)
	}
	// Unknown daemon version: fall back to the proxy's, no lag hint.
	fallback := reconnectNoteText("", "1.2.3", true, reconnectOutcome{})
	if !strings.Contains(fallback, "(daemon 1.2.3)") || strings.Contains(fallback, "serve proxy") {
		t.Errorf("fallback note wrong: %q", fallback)
	}
	// Differing versions: daemon's version leads, proxy lag stated.
	differ := reconnectNoteText("2.0.0", "1.2.3", true, reconnectOutcome{})
	if !strings.Contains(differ, "daemon now 2.0.0") ||
		!strings.Contains(differ, "this serve proxy is still 1.2.3") ||
		!strings.Contains(differ, "restart `plumb serve`") ||
		strings.Contains(differ, "start a new client session") {
		t.Errorf("differ note wrong: %q", differ)
	}
	// Differing versions with the mismatch clause suppressed (the proxy already
	// warned for this daemon version once): plain note naming the daemon's
	// version, no proxy-lag hint.
	suppressed := reconnectNoteText("2.0.0", "1.2.3", false, reconnectOutcome{})
	if !strings.Contains(suppressed, "(daemon 2.0.0)") || strings.Contains(suppressed, "serve proxy") {
		t.Errorf("suppressed-mismatch note wrong: %q", suppressed)
	}
}

// TestReconnectNoteText_ReportsObservedFactsOnly is the PLAN-426 half: the note
// may state only what the proxy actually established.
//
// The old note asserted two things it had not: it read as a daemon RESTART when
// the commonest cause is an idle eviction that restarted nothing, and it said
// session state "was rebuilt" unconditionally — wording that describes a
// successful identity recovery and a failed one identically. An agent that read
// it and concluded it had been renamed was reading it correctly; the note was
// wrong. Each case below pins the distinction the note now has to draw.
func TestReconnectNoteText_ReportsObservedFactsOnly(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		outcome reconnectOutcome
		want    []string
		avoid   []string
	}{
		{
			name:    "same daemon process is a transport reconnect, not a restart",
			outcome: reconnectOutcome{instanceKnown: true, restarted: false, recovery: string(recoveryRestored), name: "calm-stag", sessionID: "abc123"},
			want:    []string{"same daemon process", "transport reconnect, not a restart", "still calm-stag"},
			avoid:   []string{"process restarted"},
		},
		{
			name:    "a changed daemon process is reported as a restart",
			outcome: reconnectOutcome{instanceKnown: true, restarted: true, recovery: string(recoveryRestored), name: "calm-stag", sessionID: "abc123"},
			want:    []string{"process restarted", "identity was restored"},
			avoid:   []string{"transport reconnect, not a restart"},
		},
		{
			// The load-bearing negative: with no process marker on one side of
			// the reconnect the honest answer is neither "restarted" nor "did
			// not", and the note must not pick one.
			name:    "an unknown daemon process claims neither restart nor continuity",
			outcome: reconnectOutcome{instanceKnown: false, recovery: string(recoveryRestored), name: "calm-stag"},
			want:    []string{"connection re-established"},
			avoid:   []string{"process restarted", "same daemon process"},
		},
		{
			name:    "a failed recovery says so instead of implying success",
			outcome: reconnectOutcome{instanceKnown: true, restarted: true, recovery: string(recoveryDegraded)},
			want:    []string{"could NOT be restored", "temporary one", "durable record is intact"},
			avoid:   []string{"identity was restored"},
		},
		{
			name:    "no durable continuity is stated as such, not as a loss",
			outcome: reconnectOutcome{instanceKnown: true, restarted: true, recovery: string(recoveryUnavailable)},
			want:    []string{"no durable session identity"},
			avoid:   []string{"identity was restored", "could NOT be restored"},
		},
		{
			// A legacy daemon acknowledges no outcome. "Unknown" is the only
			// truthful report, and inventing either answer here is exactly the
			// failure this test exists to prevent.
			name:    "a legacy daemon's silence is reported as unknown",
			outcome: reconnectOutcome{instanceKnown: true, restarted: true},
			want:    []string{"did not report an identity outcome", "unknown"},
			avoid:   []string{"identity was restored", "could NOT be restored"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := reconnectNoteText("1.2.3", "1.2.3", true, c.outcome)
			for _, want := range c.want {
				if !strings.Contains(got, want) {
					t.Errorf("note does not state %q: %q", want, got)
				}
			}
			for _, avoid := range c.avoid {
				if strings.Contains(got, avoid) {
					t.Errorf("note claims %q, which this outcome does not establish: %q", avoid, got)
				}
			}
			// Every variant keeps the cache/pin advice: those really are rebuilt,
			// and the original field report's sharpest point was a reconnect
			// silently changing which repository writes landed in.
			if !strings.Contains(got, "workspace") || !strings.Contains(got, "re-read a file") {
				t.Errorf("note dropped the cache/pin advice: %q", got)
			}
		})
	}
}
