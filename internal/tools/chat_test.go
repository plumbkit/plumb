package tools

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
)

// TestRenderMessages_TruncationIsMarkedForRecipient is PLAN-301 D1's recipient
// half. A body over budget must not render as a bare ellipsis a recipient
// cannot tell apart from a sender who simply trailed off — it must say,
// explicitly, that it was cut and by how much.
func TestRenderMessages_TruncationIsMarkedForRecipient(t *testing.T) {
	body := strings.Repeat("a", 100)
	rows := []collab.Row{{
		AuthorSession:  "alice",
		Body:           body,
		ConversationID: "conv1",
		CreatedAt:      time.Now(),
	}}
	out := RenderMessages(rows, 20, time.Now())
	if !strings.Contains(out, "truncated") {
		t.Errorf("expected a truncation marker; got %q", out)
	}
	if !strings.Contains(out, "received") || !strings.Contains(out, "remaining") {
		t.Errorf("expected byte counts naming what was received and what remains; got %q", out)
	}
}

// TestRenderMessages_ShortBodyPassesThroughUnmarked guards against the fix
// firing on messages that never needed it.
func TestRenderMessages_ShortBodyPassesThroughUnmarked(t *testing.T) {
	rows := []collab.Row{{
		AuthorSession:  "alice",
		Body:           "short",
		ConversationID: "conv1",
		CreatedAt:      time.Now(),
	}}
	out := RenderMessages(rows, 2048, time.Now())
	if strings.Contains(out, "truncated") {
		t.Errorf("a body under budget must not be marked truncated; got %q", out)
	}
	if !strings.Contains(out, `"short"`) {
		t.Errorf("expected the body verbatim; got %q", out)
	}
}

// TestClampWithTruncationMarker_CountsOnlySenderBytes pins the arithmetic the
// marker promises, which "contains the word truncated" cannot. The clamp
// spends part of the budget on its own ellipsis and may back the cut up to a
// rune boundary, so the bytes of the sender's text that actually arrive are
// fewer than the clamped length — and a count that says otherwise points the
// remedy's resend past content neither side knows was dropped.
func TestClampWithTruncationMarker_CountsOnlySenderBytes(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		budget   int
		wantKept int
	}{
		// 20 bytes of budget, three of them spent on the ellipsis.
		{"ascii", strings.Repeat("a", 100), 20, 17},
		// 17 lands mid-rune, so the cut backs up to five whole characters.
		{"cjk backs up to a rune boundary", strings.Repeat("漢", 40), 20, 15},
		{"emoji backs up to a rune boundary", strings.Repeat("😀", 25), 20, 16},
		// Too little budget for the marker at all: every byte is the sender's.
		{"budget below the marker width", strings.Repeat("a", 100), 2, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clamped, marker, kept := clampWithTruncationMarker(tc.body, tc.budget)
			if kept != tc.wantKept {
				t.Fatalf("kept = %d, want %d (clamped %q)", kept, tc.wantKept, clamped)
			}
			// The delivered text really is the first kept bytes of the body,
			// followed by nothing but the trim marker.
			if !strings.HasPrefix(clamped, tc.body[:kept]) {
				t.Errorf("clamped %q does not start with the first %d bytes of the body", clamped, kept)
			}
			if tail := clamped[kept:]; tail != "" && tail != "…" {
				t.Errorf("clamped carries %q past the sender's bytes; only the trim marker may follow", tail)
			}
			for _, want := range []string{
				fmt.Sprintf("received %d of %d bytes", kept, len(tc.body)),
				fmt.Sprintf("remaining %d]", len(tc.body)-kept),
			} {
				if !strings.Contains(marker, want) {
					t.Errorf("want %q in marker %q", want, marker)
				}
			}
		})
	}
}

// TestRenderMessages_MultipleRowsRetainReplyHandles guards that delivery of
// multiple notes across distinct threads retains an actionable reply handle for
// every conversation thread, rather than dropping earlier handles.
func TestRenderMessages_MultipleRowsRetainReplyHandles(t *testing.T) {
	rows := []collab.Row{
		{
			AuthorSession:  "alice",
			Body:           "first note",
			ConversationID: "conv-alice",
			CreatedAt:      time.Now(),
		},
		{
			AuthorSession:  "bob",
			Body:           "second note",
			ConversationID: "conv-bob",
			CreatedAt:      time.Now(),
		},
		{
			AuthorSession:  "alice",
			Body:           "third note in same thread",
			ConversationID: "conv-alice",
			CreatedAt:      time.Now(),
		},
	}
	out := RenderMessages(rows, 1024, time.Now())
	wantAlice := `reply: leave_note({to: "alice", conversation_id: "conv-alice", body: "…"})`
	wantBob := `reply: leave_note({to: "bob", conversation_id: "conv-bob", body: "…"})`

	if !strings.Contains(out, wantAlice) {
		t.Errorf("expected alice reply handle %q; got:\n%s", wantAlice, out)
	}
	if !strings.Contains(out, wantBob) {
		t.Errorf("expected bob reply handle %q; got:\n%s", wantBob, out)
	}
	if strings.Count(out, wantAlice) != 1 {
		t.Errorf("expected exactly one reply handle for alice's thread; got %d in:\n%s", strings.Count(out, wantAlice), out)
	}
}
