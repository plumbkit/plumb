package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// leave_note's reply hint and its tool description both name check_messages,
// which is NOT in the lean set. These tests pin the reachability rule stated in
// leanNamingOnly: name it when the client can reach it, and when it cannot, say
// the true thing that needs no tool (a reply rides back on the next ordinary
// tool result) rather than handing over a broken pointer.
//
// The two suppression triggers are independent and only one is observable by
// plumb, so each is exercised separately: the lean PROFILE (plumb hid the tool
// from tools/list) and a client-side ALLOWLIST-capable client (the client's own
// config may have removed it before a call could reach plumb).

// sendNote runs one successful send and returns the rendered result.
func sendNote(t *testing.T, mutate func(*CollabDeps)) string {
	t.Helper()
	deps, _, _ := collabTestDeps(t, CollabPolicy{Mailbox: true, IntentTTLMinutes: 120})
	if mutate != nil {
		mutate(&deps)
	}
	out, err := NewLeaveNote(deps).Execute(context.Background(), json.RawMessage(`{"body":"ping","to":"alice"}`))
	if err != nil {
		t.Fatalf("leave_note: %v", err)
	}
	return out
}

func TestLeaveNote_ReplyHintNamesCheckMessagesWhenReachable(t *testing.T) {
	// Full profile, a client with no plumb-written allowlist: check_messages is
	// advertised and pinned, so naming it is a working pointer.
	out := sendNote(t, func(d *CollabDeps) {
		d.ToolProfile = func() string { return "full" }
		d.ClientName = func() string { return "claude-code" }
	})
	if !strings.Contains(out, "check_messages") {
		t.Errorf("a reachable check_messages must be named; got %q", out)
	}
	if !strings.Contains(out, "wait_seconds") {
		t.Errorf("the waiting mechanism must be spelled out; got %q", out)
	}
}

func TestLeaveNote_ReplyHintSuppressedWhenUnreachable(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*CollabDeps)
	}{
		{
			// The server hid it: under lean, check_messages is not in tools/list.
			"lean profile",
			func(d *CollabDeps) {
				d.ToolProfile = func() string { return "lean" }
				d.ClientName = func() string { return "claude-code" }
			},
		},
		{
			// The CLIENT may have hidden it. codex declares ClientSideAllowlist, so
			// `plumb setup codex --lean` can strip every non-lean tool from its own
			// config — a filter plumb cannot observe, hence the resolved profile is
			// deliberately "full" here.
			"client-side allowlist client",
			func(d *CollabDeps) {
				d.ToolProfile = func() string { return "full" }
				d.ClientName = func() string { return "codex" }
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := sendNote(t, tc.mutate)
			if strings.Contains(out, "check_messages") {
				t.Errorf("check_messages may be unreachable for this client and must not be named; got %q", out)
			}
			// Suppression must not leave the agent with nothing: delivery does not
			// depend on check_messages at all.
			if !strings.Contains(out, "next tool call") {
				t.Errorf("the suppressed form must still say how a reply arrives; got %q", out)
			}
		})
	}
}

// TestLeaveNote_DescriptionFollowsTheSameRule covers the other place the name
// ships — the text in tools/list, which every client reads.
func TestLeaveNote_DescriptionFollowsTheSameRule(t *testing.T) {
	reachable := NewLeaveNote(CollabDeps{
		ToolProfile: func() string { return "full" },
		ClientName:  func() string { return "claude-code" },
	}).Description()
	if !strings.Contains(reachable, "check_messages") {
		t.Errorf("description must name the receive half when it is reachable; got %q", reachable)
	}

	for _, deps := range []CollabDeps{
		{ToolProfile: func() string { return "lean" }},
		{ClientName: func() string { return "gemini" }},
	} {
		got := NewLeaveNote(deps).Description()
		if strings.Contains(got, "check_messages") {
			t.Errorf("description must not name a possibly-filtered tool; got %q", got)
		}
		if !strings.Contains(got, "send half of plumb's mailbox") {
			t.Errorf("suppressing the name must not gut the description; got %q", got)
		}
	}
}

// TestLeaveNote_DescriptionUnwiredDepsAreFull pins the nil-safety the tools/list
// budget test depends on: NewLeaveNote(CollabDeps{}) calls only the metadata
// methods, and must get the full text rather than a panic or the suppressed form.
func TestLeaveNote_DescriptionUnwiredDepsAreFull(t *testing.T) {
	if got := NewLeaveNote(CollabDeps{}).Description(); !strings.Contains(got, "check_messages") {
		t.Errorf("unwired deps must resolve to the permissive full text; got %q", got)
	}
}
