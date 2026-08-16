package tools

import (
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
