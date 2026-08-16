package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
)

// collab_guards_test.go covers two small honesty guards.
//
// Both are about a write or a wait that succeeded on paper while failing in a
// way the caller could not see: an unattributable row that outlives the session
// that made it, and a wait silently reduced to a fraction of what was asked for.

// unregisteredDeps reproduces the PRODUCTION shape of an unregistered session,
// which is the part the first version of this test got wrong.
//
// Registration failure does NOT leave the session nameless: newConnSession logs
// "continuing unregistered and unaddressable" and then assigns a generated
// display name anyway, for the TUI and the logs. What is actually empty is the
// session ID. A test that only blanked the NAME therefore exercised a state the
// daemon never produces, and passed while the guard it was testing read the one
// field that is never empty.
func unregisteredDeps(t *testing.T, policy CollabPolicy) (CollabDeps, *collab.Store) {
	t.Helper()
	deps, store, _ := collabTestDeps(t, policy)
	deps.SessionName = func() string { return "lively-otter" } // generated, non-empty
	deps.SessionID = ""                                        // registration failed
	return deps, store
}

func TestShareIntent_RefusesAnUnregisteredSession(t *testing.T) {
	deps, store := unregisteredDeps(t, CollabPolicy{Intents: true, IntentTTLMinutes: 120})

	out, err := NewShareIntent(deps).Execute(context.Background(),
		json.RawMessage(`{"body":"refactoring the limiter"}`))
	if err != nil {
		t.Fatalf("a refusal must not be an error: %v", err)
	}
	if !strings.Contains(out, "registered session") || !strings.Contains(out, "session_start") {
		t.Errorf("expected a refusal naming the remedy; got %q", out)
	}

	// The point of refusing is that nothing durable is left behind.
	intents, err := store.LiveIntents(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(intents) != 0 {
		t.Errorf("a refused share_intent still wrote %d row(s): %v", len(intents), intents)
	}
}

func TestShareFindings_RefusesAnUnregisteredSession(t *testing.T) {
	deps, ws := shareFindingsTestDeps(t, CollabPolicy{KnowledgeHandoff: true}, 50)
	// Registration failed: the daemon still assigns a display name, but no ID.
	// ShareFindingsDeps no longer carries the name at all, since the guard that
	// was its last reader now keys on the ID.
	deps.SessionID = ""

	out, err := NewShareFindings(deps).Execute(context.Background(),
		json.RawMessage(`{"summary":"the limiter is per-connection, not per-session"}`))
	if err != nil {
		t.Fatalf("a refusal must not be an error: %v", err)
	}
	if !strings.Contains(out, "registered session") || !strings.Contains(out, "session_start") {
		t.Errorf("expected a refusal naming the remedy; got %q", out)
	}

	// No memory file was written: the refusal is the whole point precisely
	// because a finding outlives the session that shared it.
	entries, err := os.ReadDir(filepath.Join(ws, ".plumb", "memories"))
	if err == nil && len(entries) != 0 {
		t.Errorf("a refused share_findings still wrote %d memory file(s)", len(entries))
	}
}

// TestShareIntent_AllowsARegisteredSession is the direction a too-eager guard
// would break, driven through Execute rather than the helper for the same reason
// the refusal tests are: the wiring is what was wrong, not the predicate.
func TestShareIntent_AllowsARegisteredSession(t *testing.T) {
	deps, store, _ := collabTestDeps(t, CollabPolicy{Intents: true, IntentTTLMinutes: 120})

	out, err := NewShareIntent(deps).Execute(context.Background(),
		json.RawMessage(`{"body":"refactoring the limiter"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "registered session") {
		t.Errorf("a registered session was refused: %q", out)
	}
	intents, err := store.LiveIntents(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(intents) != 1 {
		t.Errorf("registered share_intent stored %d rows, want 1", len(intents))
	}
}

// TestUnregisteredSessionRefusal_KeysOnIdentityNotName pins the predicate, and
// pins WHY: a populated display name must not be mistaken for registration.
// This is the assertion whose absence let the inert version ship.
func TestUnregisteredSessionRefusal_KeysOnIdentityNotName(t *testing.T) {
	for _, id := range []string{"sess-1", "f9ae0c-1234", "x"} {
		if got := unregisteredSessionRefusal("share_intent", id); got != "" {
			t.Errorf("session id %q was refused: %q", id, got)
		}
	}
	// Whitespace is not an identity — it would store a blank author just as surely.
	for _, blank := range []string{"", " ", "\t", "\n  "} {
		if got := unregisteredSessionRefusal("share_intent", blank); got == "" {
			t.Errorf("session id %q was accepted as registered", blank)
		}
	}
}

// TestClampWait_ReportsWhenItClamped: the boolean is the whole point. A caller
// that asks for 300s and is given 55 needs to know, because the only other
// evidence it gets is an elapsed time that looks like an early return.
func TestClampWait_ReportsWhenItClamped(t *testing.T) {
	for _, tc := range []struct {
		name        string
		requested   int
		max         int
		wantWait    time.Duration
		wantClamped bool
	}{
		{"under the ceiling", 10, 55, 10 * time.Second, false},
		{"exactly the ceiling", 55, 55, 55 * time.Second, false},
		{"over the ceiling", 300, 55, 55 * time.Second, true},
		{"zero means no wait", 0, 55, 0, false},
		{"negative means no wait", -5, 55, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wait, clamped := clampWait(tc.requested, tc.max)
			if wait != tc.wantWait {
				t.Errorf("wait = %v, want %v", wait, tc.wantWait)
			}
			if clamped != tc.wantClamped {
				t.Errorf("clamped = %v, want %v", clamped, tc.wantClamped)
			}
		})
	}
}

func TestWaitClampNotice_SilentUnlessClamped(t *testing.T) {
	if got := waitClampNotice(10, 55, false); got != "" {
		t.Errorf("an unclamped wait produced a notice: %q", got)
	}
	got := waitClampNotice(300, 55, true)
	for _, want := range []string{"300s", "55s", "clamped", "max_wait_seconds"} {
		if !strings.Contains(got, want) {
			t.Errorf("clamp notice missing %q; got %q", want, got)
		}
	}
}

// TestCheckMessages_ReportsElapsedWaitInSeconds asserts the RENDERED reply, not
// the helper. An earlier version of this test called humaniseAge directly and
// SURVIVED a mutant that put humaniseTTL back into check_messages — which is the
// entire defect. It was testing that a correct helper exists, not that the
// reply uses it.
//
// The defect: elapsed time was rendered in whole minutes, so a full 55-second
// block and an instant return both printed "waited 0 min". The blocking half of
// the API is how an agent hands its turn to a peer instead of polling, and its
// output could not distinguish working from not working at all — which is why
// two sessions independently concluded it never blocked.
func TestCheckMessages_ReportsElapsedWaitInSeconds(t *testing.T) {
	deps, _, _ := chatTestDeps(t, CollabPolicy{Mailbox: true, MaxWaitSeconds: 1}, "alice")

	out, err := NewCheckMessages(deps).Execute(context.Background(), json.RawMessage(`{"wait_seconds":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "waited 1s") {
		t.Errorf("reply does not report the wait in seconds; got %q", out)
	}
	if strings.Contains(out, " min") {
		t.Errorf("a sub-minute wait was rendered as minutes, which cannot be told from no wait at all; got %q", out)
	}
}

// TestCheckMessages_SaysWhenTheWaitWasClamped exercises the notice through the
// real reply, for the same reason as above.
func TestCheckMessages_SaysWhenTheWaitWasClamped(t *testing.T) {
	deps, _, _ := chatTestDeps(t, CollabPolicy{Mailbox: true, MaxWaitSeconds: 1}, "alice")

	out, err := NewCheckMessages(deps).Execute(context.Background(), json.RawMessage(`{"wait_seconds":3600}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"3600s", "clamped", "max_wait_seconds"} {
		if !strings.Contains(out, want) {
			t.Errorf("clamp notice missing %q in the reply; got %q", want, out)
		}
	}
}

// TestCheckMessages_SilentWhenTheWaitFitsUnderTheCeiling: the notice must not
// fire for a wait that was honoured, or every ordinary call carries a warning
// about a limit it never touched.
func TestCheckMessages_SilentWhenTheWaitFitsUnderTheCeiling(t *testing.T) {
	deps, _, _ := chatTestDeps(t, CollabPolicy{Mailbox: true, MaxWaitSeconds: 5}, "alice")

	out, err := NewCheckMessages(deps).Execute(context.Background(), json.RawMessage(`{"wait_seconds":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "clamped") {
		t.Errorf("a wait that fit under the ceiling still reported a clamp; got %q", out)
	}
}
