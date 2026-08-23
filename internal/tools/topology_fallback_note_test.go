package tools

import (
	"strings"
	"testing"
	"time"
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
			got := topologyFallbackNoteFor(tt.fn, "file:///x.go")
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

func TestTopologyDefinitionNoteFor(t *testing.T) {
	if got := topologyDefinitionNoteFor(nil, ""); got != topologyDefinitionNote {
		t.Fatalf("nil fn: note = %q, want the legacy const", got)
	}
	if got := topologyDefinitionNoteFor(warmupFixed(false, 0), ""); got != topologyDefinitionNote {
		t.Fatalf("not warming: note = %q, want the legacy const", got)
	}
	got := topologyDefinitionNoteFor(warmupFixed(true, 4*time.Second), "file:///x.go")
	for _, w := range []string{"still warming", "~4s", "declaration line not cursor offset", "retry shortly"} {
		if !strings.Contains(got, w) {
			t.Errorf("warming definition note missing %q: %q", w, got)
		}
	}
	if strings.Contains(got, "unavailable") {
		t.Errorf("warming definition note must not claim the server is unavailable: %q", got)
	}
	if got := topologyDefinitionNoteFor(warmupFixed(true, 0), ""); strings.Contains(got, "elapsed") {
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
	got := topologyFallbackNoteWhen(fallbackLSPTimedOut, nil, "file:///x.go", 15*time.Second)
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
	warm := topologyFallbackNoteWhen(fallbackLSPTimedOut, warmupFixed(true, 4*time.Second), "file:///x.go", 15*time.Second)
	if !strings.Contains(warm, "still warming") {
		t.Errorf("a warming server must keep the warming banner: %q", warm)
	}

	// No attempt budget to name (the server answered, just not usefully) keeps
	// the historical wording byte-for-byte.
	if got := topologyFallbackNoteWhen(fallbackNotUsed, nil, "file:///x.go", 0); got != topologyFallbackNote {
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
