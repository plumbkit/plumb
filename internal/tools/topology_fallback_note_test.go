package tools

import (
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/topology"
)

// warmupFixed returns an LSPWarmupFn reporting a fixed warm-up state.
func warmupFixed(warming bool, elapsed time.Duration) LSPWarmupFn {
	return func(string) (bool, time.Duration) { return warming, elapsed }
}

func TestTopologyFallbackNoteFor(t *testing.T) {
	tests := []struct {
		name         string
		fn           LSPWarmupFn
		wantExact    string // non-empty ⇒ the note must equal this byte-for-byte
		wantContains []string
		wantAbsent   []string
	}{
		{
			name:      "nil fn keeps the legacy note",
			fn:        nil,
			wantExact: topologyFallbackNote,
		},
		{
			name:      "not warming keeps the legacy note",
			fn:        warmupFixed(false, 0),
			wantExact: topologyFallbackNote,
		},
		{
			name:         "warming with elapsed names the state and duration",
			fn:           warmupFixed(true, 4*time.Second),
			wantContains: []string{"still warming", "~4s", "retry shortly", "source=topology, mode=indexed-approximate"},
			wantAbsent:   []string{"LSP unavailable"},
		},
		{
			name:         "warming with zero elapsed omits the duration parenthetical",
			fn:           warmupFixed(true, 0),
			wantContains: []string{"still warming"},
			wantAbsent:   []string{"elapsed", "(~"},
		},
		{
			name:         "sub-half-second elapsed rounds to zero and is omitted",
			fn:           warmupFixed(true, 300*time.Millisecond),
			wantContains: []string{"still warming"},
			wantAbsent:   []string{"elapsed"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := topologyFallbackNoteFor(nil, tt.fn, "file:///x.go")
			if tt.wantExact != "" && got != tt.wantExact {
				t.Fatalf("note = %q, want exactly %q", got, tt.wantExact)
			}
			for _, w := range tt.wantContains {
				if !strings.Contains(got, w) {
					t.Errorf("note missing %q: %q", w, got)
				}
			}
			for _, w := range tt.wantAbsent {
				if strings.Contains(got, w) {
					t.Errorf("note should not contain %q: %q", w, got)
				}
			}
		})
	}
}

// TestStaleIndexSuffix is PLAN-490's core rule: a healthy index adds NOTHING to a
// fallback banner — the tests around this one pin the legacy text byte-for-byte
// with a nil store, which is that same case — and a failing index adds the
// stale-index clause, in indexHealthNote's exact wording so the six topology_*
// query tools and these fallbacks cannot drift apart about what a failing index
// means.
func TestStaleIndexSuffix(t *testing.T) {
	now := time.Now()
	healthy := topology.Health{State: "idle", LastSync: now.Add(-time.Minute)}
	if got := staleIndexSuffix(healthy, now); got != "" {
		t.Errorf("a healthy index must add nothing, got %q", got)
	}

	failing := topology.Health{State: "error", LastSync: now.Add(-2 * time.Hour), LastError: "boom", Failing: true}
	got := staleIndexSuffix(failing, now)
	if got == "" {
		t.Fatal("a failing index must add the stale-index clause")
	}
	if !strings.HasPrefix(got, " ") {
		t.Errorf("the clause is appended to a banner, so it needs its separating space: %q", got)
	}
	if want := " " + indexHealthNote(failing, now); got != want {
		t.Errorf("the clause must BE indexHealthNote's wording, not a copy:\n got %q\nwant %q", got, want)
	}
	for _, w := range []string{"failing", "boom", "absence"} {
		if !strings.Contains(got, w) {
			t.Errorf("the clause must say %q: %q", w, got)
		}
	}
}

func TestTopologyDefinitionNoteFor(t *testing.T) {
	if got := topologyDefinitionNoteFor(nil, nil, ""); got != topologyDefinitionNote {
		t.Fatalf("nil fn: note = %q, want the legacy const", got)
	}
	if got := topologyDefinitionNoteFor(nil, warmupFixed(false, 0), ""); got != topologyDefinitionNote {
		t.Fatalf("not warming: note = %q, want the legacy const", got)
	}
	got := topologyDefinitionNoteFor(nil, warmupFixed(true, 4*time.Second), "file:///x.go")
	for _, w := range []string{"still warming", "~4s", "declaration line not cursor offset", "retry shortly"} {
		if !strings.Contains(got, w) {
			t.Errorf("warming definition note missing %q: %q", w, got)
		}
	}
	if strings.Contains(got, "unavailable") {
		t.Errorf("warming definition note must not claim the server is unavailable: %q", got)
	}
	if got := topologyDefinitionNoteFor(nil, warmupFixed(true, 0), ""); strings.Contains(got, "elapsed") {
		t.Errorf("zero elapsed should omit the duration parenthetical: %q", got)
	}
}

func TestTreeSitterFallbackNote(t *testing.T) {
	if got := treeSitterFallbackNote(fallbackLSPUnavailable, nil, "", 0); got != treeSitterFallbackLegacyNote {
		t.Fatalf("nil fn: banner = %q, want the legacy const", got)
	}
	if got := treeSitterFallbackNote(fallbackLSPUnavailable, warmupFixed(false, 0), "", 0); got != treeSitterFallbackLegacyNote {
		t.Fatalf("not warming: banner = %q, want the legacy const", got)
	}
	got := treeSitterFallbackNote(fallbackLSPUnavailable, warmupFixed(true, 4*time.Second), "file:///x.go", 0)
	for _, w := range []string{"still warming", "~4s", "located by tree-sitter", "line-granular"} {
		if !strings.Contains(got, w) {
			t.Errorf("warming banner missing %q: %q", w, got)
		}
	}
	if strings.Contains(got, "LSP unavailable") {
		t.Errorf("warming banner must not claim the server is unavailable: %q", got)
	}
	if !strings.HasSuffix(got, "\n\n") {
		t.Errorf("banner must keep the trailing blank line: %q", got)
	}
	if got := treeSitterFallbackNote(fallbackLSPUnavailable, warmupFixed(true, 0), "", 0); strings.Contains(got, "elapsed") {
		t.Errorf("zero elapsed should omit the duration parenthetical: %q", got)
	}
}

func TestTopologyFallbackNoteWhen_SlowServerIsNotCalledUnavailable(t *testing.T) {
	// The disclosure the card requires in the RESPONSE: a server that is up and
	// merely missed its (shortened) attempt budget must not be reported as
	// absent, because "unavailable" argues for abandoning semantic tools while
	// the right move is to retry.
	got := topologyFallbackNoteWhen(fallbackLSPTimedOut, nil, nil, "file:///x.go", 15*time.Second)
	if !strings.Contains(got, "did not answer within 15s") {
		t.Errorf("a timed-out attempt must name the budget it missed: %q", got)
	}
	if strings.Contains(got, "LSP unavailable") {
		t.Errorf("a slow server is not an absent one: %q", got)
	}
	if !strings.Contains(got, "source=topology, mode=indexed-approximate") {
		t.Errorf("the provenance suffix must survive the new variant: %q", got)
	}

	// Warming outranks timed-out: the agent's action differs (wait for the
	// handshake vs. retry a query), and the handshake is the more specific fact.
	warm := topologyFallbackNoteWhen(fallbackLSPTimedOut, nil, warmupFixed(true, 4*time.Second), "file:///x.go", 15*time.Second)
	if !strings.Contains(warm, "still warming") {
		t.Errorf("a warming server must keep the warming banner: %q", warm)
	}

	// No attempt budget to name (the server answered, just not usefully) keeps
	// the historical wording byte-for-byte.
	if got := topologyFallbackNoteWhen(fallbackNotUsed, nil, nil, "file:///x.go", 0); got != topologyFallbackNote {
		t.Errorf("no missed budget must keep the legacy note, got %q", got)
	}
}

func TestRoundedDuration_QuotesAReadableFigure(t *testing.T) {
	// The banner quoted a raw monotonic figure — "did not answer within
	// 999.997166ms" — whenever the caller supplied its own deadline.
	tests := []struct {
		in   time.Duration
		want string
	}{
		{999997166 * time.Nanosecond, "1s"},
		{15 * time.Second, "15s"},
		{22*time.Second + 345678*time.Microsecond, "22s"},
		{432*time.Millisecond + 117*time.Microsecond, "432ms"},
	}
	for _, tt := range tests {
		if got := roundedDuration(tt.in).String(); got != tt.want {
			t.Errorf("roundedDuration(%v) = %s, want %s", tt.in, got, tt.want)
		}
	}
}
