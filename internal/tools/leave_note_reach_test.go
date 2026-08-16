package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// leave_note's reply hint names check_messages, which is not in the lean set.
// An earlier attempt made the hint reachability-aware and suppressed the name
// for some clients; these tests pin the shipped wording and the invariant that
// removed the need for any gating.

func TestLeaveNote_ReplyHintNamesBothDeliveryPaths(t *testing.T) {
	deps, _, _ := collabTestDeps(t, CollabPolicy{Mailbox: true, IntentTTLMinutes: 120})
	out, err := NewLeaveNote(deps).Execute(context.Background(), json.RawMessage(`{"body":"ping","to":"alice"}`))
	if err != nil {
		t.Fatalf("leave_note: %v", err)
	}
	// The ACTIVE path. wait_seconds is the only way to hand your turn to a peer
	// rather than poll, and an agent cannot infer the parameter from the name.
	if !strings.Contains(out, "call check_messages with a wait_seconds value") {
		t.Errorf("the waiting path must be named in full; got %q", out)
	}
	// The PASSIVE path, which is what actually fires for an agent that carries on
	// working. Naming only the active one implied a reply needed a call the agent
	// might never make.
	if !strings.Contains(out, "otherwise it is appended to the result of your next tool call") {
		t.Errorf("the no-action delivery path must be named too; got %q", out)
	}
}

// TestMailboxPairIsReachableTogether is the invariant that makes gating this
// hint on reachability pointless, and it is the thing to re-check before anyone
// re-introduces such a gate.
//
// leave_note's hint and description both point at check_messages. Every
// server-side or config-side mechanism that can remove check_messages removes
// leave_note with it: the lean profile advertises only LeanTools (neither is in
// it), and a client-side allowlist is exactly LeanToolNames() — what
// `plumb setup <client> --lean` writes into Codex/Gemini/Kimi's own config —
// which contains neither. So a suppression rule can only fire when leave_note is
// equally gone (nothing renders) or when check_messages is in fact present
// (a working pointer, needlessly withheld — which is what cost a stock Codex
// session the wait_seconds mechanism). The one mechanism that CAN split the pair
// is client-side schema deferral, and MailboxTools/AlwaysLoad is its fix.
//
// If either half ever becomes independently reachable, this fails — and the
// suppression question genuinely reopens.
func TestMailboxPairIsReachableTogether(t *testing.T) {
	for name := range MailboxTools {
		if IsLean(name) {
			t.Errorf("%q is now lean while its partner may not be; the lean profile can split the pair", name)
		}
	}
	for _, name := range LeanToolNames() {
		if IsMailbox(name) {
			t.Errorf("%q is in the client-side allowlist LeanToolNames(); an allowlist can now keep one half and drop the other", name)
		}
	}
}

// TestLeaveNote_DescriptionNamesTheReceiveHalf covers the other place the name
// ships — the text in tools/list, which every client reads. It must be a fixed
// string: Description is called under mcp.Server.mu.RLock (see snapshotTools).
//
// The two checks are deliberately different claims, and both are needed. The
// first pins the pairing sentence ("there is a receive half, and it is called
// this"); the second pins the DELIVERY enumeration, where check_messages
// appears as one of the three paths a message travels. Collapsing the second to
// a bare "check_messages" would make it vacuous — the first substring already
// contains that token, so it would pass on the pairing sentence alone while the
// delivery paragraph silently lost the name.
func TestLeaveNote_DescriptionNamesTheReceiveHalf(t *testing.T) {
	got := NewLeaveNote(CollabDeps{}).Description()
	if !strings.Contains(got, "check_messages is the receive half") {
		t.Errorf("description must name the receive half; got %q", got)
	}
	if !strings.Contains(got, "check_messages, or session_start") {
		t.Errorf("description must name check_messages as a peer's delivery path; got %q", got)
	}
}
