package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestSessionStart_UnlinkedNotice pins the unlinked-session seam of the
// orientation packet. A session that never passes session_id to session_start
// is silently unaddressable: its wake stamp is keyed by a conversation id the
// caller never supplied, and leave_note (addressed by session name) cannot
// name it. session_start knows this at bootstrap and must say so in the
// identity block — the one section every agent reads. A linked session (raw
// input carrying a non-empty session_id) must stay byte-quiet on the subject.
func TestSessionStart_UnlinkedNotice(t *testing.T) {
	t.Run("no session_id", func(t *testing.T) {
		tool := NewSessionStart(func(context.Context) string { return t.TempDir() }, nil, nil, nil, func() string { return "" }, nil)
		out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !strings.Contains(out, unlinkedSessionNotice) {
			t.Errorf("unlinked session must carry the notice, got:\n%s", out)
		}
	})

	t.Run("empty session_id", func(t *testing.T) {
		tool := NewSessionStart(func(context.Context) string { return t.TempDir() }, nil, nil, nil, func() string { return "" }, nil)
		out, err := tool.Execute(context.Background(), json.RawMessage(`{"session_id":""}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !strings.Contains(out, unlinkedSessionNotice) {
			t.Errorf("an empty session_id is still unlinked and must carry the notice, got:\n%s", out)
		}
	})

	t.Run("session_id present, trivial externalIDFn", func(t *testing.T) {
		tool := NewSessionStart(func(context.Context) string { return t.TempDir() }, nil, nil, nil, func() string { return "" }, nil).
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
		tool := NewSessionStart(func(context.Context) string { return t.TempDir() }, nil, nil, nil, func() string { return "" }, nil).
			WithExternalID(func(string) string { return "alice" })
		// An inherited name is exactly the auto-brief signal (PLAN-356), so this
		// scenario now defaults to the brief packet; ask for detail:"full"
		// explicitly since the identity-block behaviour under test—does the
		// resumed name render—is a full-packet concern.
		out, err := tool.Execute(context.Background(), json.RawMessage(`{"session_id":"abc-123","detail":"full"}`))
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

// A session_start that fails on a malformed `detail` must not link: linking
// declares the caller's identity on a shared connection (issue #513), and a
// failed call must commit nothing. The valid call is the positive control.
func TestSessionStart_InvalidDetailDoesNotLink(t *testing.T) {
	var linked []string
	tool := NewSessionStart(func(context.Context) string { return t.TempDir() }, nil, nil, nil, func() string { return "" }, nil).
		WithExternalID(func(id string) string { linked = append(linked, id); return "" })
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"session_id":"abc-123","detail":"bogus"}`)); err == nil {
		t.Fatal("an invalid detail must fail")
	}
	if len(linked) != 0 {
		t.Fatalf("a failed session_start linked %v", linked)
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"session_id":"abc-123","detail":"brief"}`)); err != nil {
		t.Fatalf("control: a valid session_start failed: %v", err)
	}
	if len(linked) != 1 || linked[0] != "abc-123" {
		t.Fatalf("control: a valid session_start must link once, got %v", linked)
	}
}
