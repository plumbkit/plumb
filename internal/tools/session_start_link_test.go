package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// unlinkedSessionNotice is the exact identity-block line session_start emits
// when the caller passed no session_id. Pinning the full string keeps the
// wording — and therefore the promise it makes — stable.
const unlinkedSessionNotice = "NOTE: this session has no external id — plumb mail and the peer wake hook cannot address it by name; pass session_id to session_start to link it."

// TestSessionStart_UnlinkedNotice pins the unlinked-session seam of the
// orientation packet. A session that never passes session_id to session_start
// is silently unaddressable: its wake stamp is keyed by a conversation id the
// caller never supplied, and leave_note (addressed by session name) cannot
// name it. session_start knows this at bootstrap and must say so in the
// identity block — the one section every agent reads. A linked session (raw
// input carrying a non-empty session_id) must stay byte-quiet on the subject.
func TestSessionStart_UnlinkedNotice(t *testing.T) {
	t.Run("no session_id", func(t *testing.T) {
		tool := NewSessionStart(func() string { return t.TempDir() }, nil, nil, nil, func() string { return "" }, nil)
		out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !strings.Contains(out, unlinkedSessionNotice) {
			t.Errorf("unlinked session must carry the notice, got:\n%s", out)
		}
	})

	t.Run("empty session_id", func(t *testing.T) {
		tool := NewSessionStart(func() string { return t.TempDir() }, nil, nil, nil, func() string { return "" }, nil)
		out, err := tool.Execute(context.Background(), json.RawMessage(`{"session_id":""}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !strings.Contains(out, unlinkedSessionNotice) {
			t.Errorf("an empty session_id is still unlinked and must carry the notice, got:\n%s", out)
		}
	})

	t.Run("session_id present, trivial externalIDFn", func(t *testing.T) {
		tool := NewSessionStart(func() string { return t.TempDir() }, nil, nil, nil, func() string { return "" }, nil).
			WithExternalID(func(string) string { return "" })
		out, err := tool.Execute(context.Background(), json.RawMessage(`{"session_id":"abc-123"}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if strings.Contains(out, unlinkedSessionNotice) {
			t.Errorf("linked session must not carry the notice, got:\n%s", out)
		}
	})

	t.Run("session_id present, externalIDFn returns name", func(t *testing.T) {
		tool := NewSessionStart(func() string { return t.TempDir() }, nil, nil, nil, func() string { return "" }, nil).
			WithExternalID(func(string) string { return "alice" })
		out, err := tool.Execute(context.Background(), json.RawMessage(`{"session_id":"abc-123"}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if strings.Contains(out, unlinkedSessionNotice) {
			t.Errorf("linked session must not carry the notice, got:\n%s", out)
		}
		if !strings.Contains(out, "Session:  alice (resumed)") {
			t.Errorf("linked session should resume its inherited name, got:\n%s", out)
		}
	})
}
