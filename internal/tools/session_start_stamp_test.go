package tools

import (
	"context"
	"strings"
	"testing"
)

// The per-call identity channel is the ONLY channel the shared-connection write
// gate reads (internal/cli/conn_logical_agent.go refuse). A client whose runtime
// drops the PreToolUse `updatedInput` rewrite — observed on
// `local-agent-mode-plumb` — declares its identity through session_start fine and
// still has every state-changing call refused, with a remedy line telling it to
// install a hook it already has. These tests pin the disclosure that turns that
// mid-session refusal into an orientation-time fact (PLAN-440 acceptance b).

func TestStampChannelNoteSilentWhenChannelLive(t *testing.T) {
	var s SessionStart
	s.WithStampChannel(func(context.Context) StampChannelState {
		return StampChannelState{Shared: true, PerCallStamped: true}
	})
	if got := s.stampChannelNote(context.Background()); got != "" {
		t.Fatalf("a stamped call must produce no note, got %q", got)
	}
}

func TestStampChannelNoteWarnsWhenSharedAndUnstamped(t *testing.T) {
	var s SessionStart
	s.WithStampChannel(func(context.Context) StampChannelState {
		return StampChannelState{Shared: true, PerCallStamped: false}
	})
	got := s.stampChannelNote(context.Background())
	if got == "" {
		t.Fatal("a shared connection with no per-call stamp must warn; got no note")
	}
	// The whole point is that the refusal's own remedy is wrong here: the hook
	// may be installed and still not reach the daemon. The note must not repeat
	// it, and must name the one remedy that does not depend on the client.
	if strings.Contains(got, "plumb hooks install") {
		t.Errorf("note must not repeat the hook remedy on a client that drops the stamp: %q", got)
	}
	if !strings.Contains(got, "one plumb serve per") {
		t.Errorf("note must name the transport remedy, got %q", got)
	}
}

func TestStampChannelNoteWarnsBeforeTheConnectionIsShared(t *testing.T) {
	var s SessionStart
	s.WithStampChannel(func(context.Context) StampChannelState {
		return StampChannelState{Shared: false, PerCallStamped: false}
	})
	got := s.stampChannelNote(context.Background())
	if got == "" {
		t.Fatal("acceptance (b): the gap must be discoverable BEFORE a second agent attaches")
	}
	if strings.Contains(got, "are being refused") {
		t.Errorf("a sole agent is not being refused yet; note must state a future cost: %q", got)
	}
}

func TestStampChannelNoteSilentWhenUnwired(t *testing.T) {
	var s SessionStart
	if got := s.stampChannelNote(context.Background()); got != "" {
		t.Fatalf("unwired accessor must stay silent, got %q", got)
	}
}
