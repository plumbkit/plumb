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

// unnamedDeps is collabTestDeps with the session name stripped — a connection
// that reached a tool before session_start registered it.
func unnamedDeps(t *testing.T, policy CollabPolicy) (CollabDeps, *collab.Store) {
	t.Helper()
	deps, store, _ := collabTestDeps(t, policy)
	deps.SessionName = func() string { return "" }
	return deps, store
}

func TestShareIntent_RefusesAnUnregisteredSession(t *testing.T) {
	deps, store := unnamedDeps(t, CollabPolicy{Intents: true, IntentTTLMinutes: 120})

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
	deps.SessionName = func() string { return "" }

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

// TestUnregisteredSessionRefusal_LetsARegisteredSessionThrough is the direction
// a too-eager guard would break: the refusal must fire on an absent name only,
// not on any name a caller might present.
func TestUnregisteredSessionRefusal_LetsARegisteredSessionThrough(t *testing.T) {
	for _, name := range []string{"icy-storm", "a", "Session-With-Caps", "ends-with-9"} {
		if got := unregisteredSessionRefusal("share_intent", name); got != "" {
			t.Errorf("session %q was refused: %q", name, got)
		}
	}
	// Whitespace is not a name — it would store a blank author just as surely.
	for _, blank := range []string{"", " ", "\t", "\n  "} {
		if got := unregisteredSessionRefusal("share_intent", blank); got == "" {
			t.Errorf("session name %q was accepted as registered", blank)
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
