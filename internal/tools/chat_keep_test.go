package tools

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
)

// TestRenderBacklog states or stays silent: zero (or a failed count read
// upstream) renders nothing, a positive count names the number, the
// as-of-this-call snapshot framing, and the drain-now remedy.
func TestRenderBacklog(t *testing.T) {
	if got := RenderBacklog(0); got != "" {
		t.Errorf("RenderBacklog(0) = %q, want empty", got)
	}
	got := RenderBacklog(4)
	for _, want := range []string{"4 more waiting as of this call", "check_messages", "before replying"} {
		if !strings.Contains(got, want) {
			t.Errorf("RenderBacklog(4) = %q, want it to name %q", got, want)
		}
	}
}

// TestCheckMessages_BacklogNamesRemainder: with more mail waiting than one
// delivery may hand over, the reply must say so — "3 new" on its own cannot
// distinguish three-of-three from three-of-more, and an agent that goes idle
// here believes it has read everything.
func TestCheckMessages_BacklogNamesRemainder(t *testing.T) {
	deps, store, _ := collabTestDeps(t, CollabPolicy{Mailbox: true, ChatBudgetBytes: 2048})
	deps.StoreIfExists = func() *collab.Store { return store }
	seedNotes(t, store, 4)

	out, err := NewCheckMessages(deps).Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1 more waiting as of this call") {
		t.Errorf("a capped delivery must name the remainder; got:\n%s", out)
	}

	// The drain: a second call hands over the last note and stays silent about
	// a backlog that no longer exists.
	out2, err := NewCheckMessages(deps).Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out2, "more waiting") {
		t.Errorf("an uncapped delivery must not claim a backlog; got:\n%s", out2)
	}
}

// TestCheckMessages_KeepsDeliveredNotes: the recipient's policy is the
// permanence trigger — claiming under keep_delivered_notes stamps the row
// far-future in the same statement that marks it read.
func TestCheckMessages_KeepsDeliveredNotes(t *testing.T) {
	deps, store, _ := collabTestDeps(t, CollabPolicy{Mailbox: true, ChatBudgetBytes: 2048, KeepDeliveredNotes: true})
	deps.StoreIfExists = func() *collab.Store { return store }
	seedNotes(t, store, 1)

	if _, err := NewCheckMessages(deps).Execute(context.Background(), json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	rows, err := store.SentBy(context.Background(), "sess-bob", time.Now(), 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("SentBy = %d rows (err %v), want the kept transcript row", len(rows), err)
	}
	if got := rows[0].ExpiresAt.UnixNano(); got != math.MaxInt64 {
		t.Errorf("kept row expires_at = %d, want the far-future MaxInt64 stamp", got)
	}
}

// TestCheckMessages_PlainPolicyExpiresDelivered: without the flag, a claimed
// note keeps the expiry it was sent with — the default is retention-neutral.
func TestCheckMessages_PlainPolicyExpiresDelivered(t *testing.T) {
	deps, store, _ := collabTestDeps(t, CollabPolicy{Mailbox: true, ChatBudgetBytes: 2048})
	deps.StoreIfExists = func() *collab.Store { return store }
	seedNotes(t, store, 1)

	if _, err := NewCheckMessages(deps).Execute(context.Background(), json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	rows, err := store.SentBy(context.Background(), "sess-bob", time.Now(), 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("SentBy = %d rows (err %v)", len(rows), err)
	}
	if rows[0].ExpiresAt.After(time.Now().Add(200 * 365 * 24 * time.Hour)) {
		t.Error("without keep_delivered_notes a delivered note must keep its ordinary expiry")
	}
}

// TestResolveNoteTTL_FollowsIntentUntilSet: the fallback ordering is the
// no-change-upgrade contract — note_ttl_minutes when set, else the
// intent_ttl_minutes the deployment already runs, else the compiled default.
func TestResolveNoteTTL_FollowsIntentUntilSet(t *testing.T) {
	cases := []struct {
		name   string
		policy CollabPolicy
		want   time.Duration
	}{
		{"follows intent until set", CollabPolicy{IntentTTLMinutes: 1440}, 1440 * time.Minute},
		{"own key wins", CollabPolicy{IntentTTLMinutes: 1440, NoteTTLMinutes: 30}, 30 * time.Minute},
		{"compiled default when both unset", CollabPolicy{}, 120 * time.Minute},
	}
	for _, tc := range cases {
		if got := resolveNoteTTL(tc.policy); got != tc.want {
			t.Errorf("%s: resolveNoteTTL = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// TestLeaveNote_KeptLineInReceipt: a same-project send under a keeping
// workspace tells the sender the truth about retention — the expiry shown is
// the unread window, not the lifetime — and a plain workspace says nothing new.
func TestLeaveNote_KeptLineInReceipt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		keep    bool
		wantHas bool
	}{
		{"keeping workspace states it", true, true},
		{"plain workspace stays silent", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, _, _ := collabTestDeps(t, CollabPolicy{
				Mailbox: true, ChatBudgetBytes: 2048, KeepDeliveredNotes: tc.keep,
			})
			out, err := NewLeaveNote(deps).Execute(context.Background(),
				json.RawMessage(`{"to":"peer","body":"check the receipt"}`))
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(out, "kept:"); got != tc.wantHas {
				t.Errorf("receipt kept-line present = %v, want %v;\n%s", got, tc.wantHas, out)
			}
		})
	}
}

// seedNotes stores n unread notes addressed to the harness session from a
// distinct author, bumping nothing — check_messages claims directly.
func seedNotes(t *testing.T, store *collab.Store, n int) {
	t.Helper()
	now := time.Now()
	for range n {
		if _, err := store.PutNote(context.Background(), collab.NoteInput{
			AuthorSession: "bob", AuthorID: "sess-bob",
			Body: "note", Addressee: "test-session", TTL: time.Hour,
		}, now); err != nil {
			t.Fatal(err)
		}
	}
}
