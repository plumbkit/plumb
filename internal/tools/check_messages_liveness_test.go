package tools

import (
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
)

// TestRenderReceipt_SaysWhetherTheRecipientCanStillReadIt is PLAN-496's outbox
// liveness: each unread row says whether the session it waits for is live, has
// ended with its name taken by another, or has no single holder — so a sender can
// tell a busy peer from a note that will expire unread whatever they wait for.
func TestRenderReceipt_SaysWhetherTheRecipientCanStillReadIt(t *testing.T) {
	now := time.Now()
	sent := now.Add(-10 * time.Minute)
	live := map[string]PeerSession{
		"busy-peer":    {ID: "sess-busy"},
		"renamed-peer": {ID: "sess-NEW"},
		"named-peer":   {ID: "sess-named"},
	}
	resolve := func(name string) (PeerSession, bool) {
		p, ok := live[name]
		return p, ok
	}
	cases := []struct {
		name string
		row  collab.Row
		want string
	}{
		{
			name: "bound, recipient live",
			row:  collab.Row{Addressee: "busy-peer", AddresseeID: "sess-busy", CreatedAt: sent},
			want: "the session it is bound to is live",
		},
		{
			name: "bound, name now held by another session",
			row:  collab.Row{Addressee: "renamed-peer", AddresseeID: "sess-OLD", CreatedAt: sent},
			want: "has ended and the name now belongs to another session, so it will expire unread",
		},
		{
			name: "bound, no live holder",
			row:  collab.Row{Addressee: "gone-peer", AddresseeID: "sess-gone", CreatedAt: sent},
			want: "has most likely ended and it will expire unread",
		},
		{
			name: "by name, live holder",
			row:  collab.Row{Addressee: "named-peer", CreatedAt: sent},
			want: "a live session answers to that name",
		},
		{
			name: "by name, no holder",
			row:  collab.Row{Addressee: "future-peer", CreatedAt: sent},
			want: "the next session to take it will receive it",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderReceipt([]collab.Row{tc.row}, now, resolve)
			if !strings.Contains(got, tc.want) {
				t.Errorf("receipt lacks %q:\n%s", tc.want, got)
			}
		})
	}
}

// TestRenderReceipt_WithoutAResolverIsUnchanged is the control: a receipt built
// with no resolver (liveness unknown) and a "next" note (no recipient yet) carry
// no liveness clause at all, so nothing is claimed that nobody checked.
func TestRenderReceipt_WithoutAResolverIsUnchanged(t *testing.T) {
	now := time.Now()
	row := collab.Row{Addressee: "busy-peer", AddresseeID: "sess-busy", CreatedAt: now.Add(-time.Minute)}
	got := renderReceipt([]collab.Row{row}, now, nil)
	for _, clause := range []string{"is live", "will expire unread —", "answers to that name"} {
		if strings.Contains(got, clause) {
			t.Errorf("a receipt with no resolver claims %q:\n%s", clause, got)
		}
	}
	next := collab.Row{Addressee: collab.AddresseeNext, CreatedAt: now.Add(-time.Minute)}
	called := false
	got = renderReceipt([]collab.Row{next}, now, func(string) (PeerSession, bool) {
		called = true
		return PeerSession{}, false
	})
	if called {
		t.Error("a \"next\" note was resolved by name; it has no recipient to check")
	}
	if strings.Contains(got, "answers to that name") {
		t.Errorf("a \"next\" note carries a liveness clause:\n%s", got)
	}
}
