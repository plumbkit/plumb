package sessionstate

// claims_test.go — a conversation's own superseded generations hold no claim.
//
// A `plumb serve` RESTART re-keys the same conversation under a fresh proxy
// secret, so SaveIdentity INSERTs a second row and the resume path then renames it
// onto the name its predecessor held. Both rows then hold that name, and identity
// rows are never expired by age, so LegacyNameConflicts named 16 of them at every
// daemon start on the machine this was measured on — 11 of which were one
// conversation's own churn, burying the genuinely ambiguous names among them.
//
// Those rows are NOT deleted and NOT rewritten. Each is the durable proof of which
// session a reconnecting proxy is, and "a newer row exists" is not proof that the
// older serve died: a serve whose socket dropped keeps its process and its proxy
// secret, and deleting the row would turn its next reconnect into first contact —
// a new session ID, a new name, and every note bound to the old ID stranded. What
// is retired is the CLAIM: independentClaims keeps only the newest row of each
// (conversation, name) group in the answers, so the reservation is held for the
// conversation that owns the name and the conflict report carries only what cannot
// be shown to be one conversation.
//
// Every test below that asserts a row was collapsed out of an answer also asserts,
// in the same run, that its RECORD survives. A collapse that damaged rows would
// otherwise pass.

import (
	"strings"
	"testing"
)

func reservationsFor(t *testing.T, s *Store, name string) []Reservation {
	t.Helper()
	res, err := s.Reservations()
	if err != nil {
		t.Fatalf("Reservations: %v", err)
	}
	var out []Reservation
	for _, r := range res {
		if strings.EqualFold(r.Name, name) {
			out = append(out, r)
		}
	}
	return out
}

// recordIntact asserts the row under proxyID is still there with its own session
// ID — the half a reconnect needs, and the half a delete-based retirement would
// have destroyed.
func recordIntact(t *testing.T, s *Store, proxyID, sess, name, external string) {
	t.Helper()
	rec, ok, err := s.LoadIdentity(proxyID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Errorf("the identity RECORD for %s is gone; retirement must retire a claim, not a row", proxyID)
		return
	}
	if rec.SessionID != sess || rec.Name != name || rec.ExternalID != external {
		t.Errorf("LoadIdentity(%s) = %+v, want Name=%q SessionID=%q ExternalID=%q — a row that loses its "+
			"session ID cannot be resumed by the serve holding its proxy secret", proxyID, rec, name, sess, external)
	}
}

// TestReservations_CollapsesAConversationsOwnSupersededGenerations is the measured
// shape: three generations of one conversation's row for one name, which must
// resolve to a single claim held by the newest.
func TestReservations_CollapsesAConversationsOwnSupersededGenerations(t *testing.T) {
	s := newTestStore(t)
	seedRaw(t, s, "proxy-1", "azure-lark", "sess-1", "conv-a", 1000)
	seedRaw(t, s, "proxy-2", "azure-lark", "sess-2", "conv-a", 1001)
	seedRaw(t, s, "proxy-3", "azure-lark", "sess-3", "conv-a", 1002)
	// A different conversation holding the same name keeps its own claim.
	seedRaw(t, s, "proxy-other", "azure-lark", "sess-other", "conv-b", 900)
	// Names are compared case-insensitively here because that is how name
	// uniqueness itself is compared: a case variant must not become a second claim.
	seedRaw(t, s, "proxy-case-1", "Case-Lark", "sess-case-1", "conv-case", 1000)
	seedRaw(t, s, "proxy-case-2", "case-lark", "sess-case-2", "conv-case", 1001)

	got := reservationsFor(t, s, "azure-lark")
	if len(got) != 2 {
		t.Fatalf("claims on azure-lark = %+v, want 2 — the newest generation of conv-a and conv-b's", got)
	}
	byConv := map[string]string{}
	for _, r := range got {
		byConv[r.ExternalID] = r.SessionID
	}
	if byConv["conv-a"] != "sess-3" {
		t.Errorf("conv-a's claim is held by %q, want sess-3, the newest generation", byConv["conv-a"])
	}
	if byConv["conv-b"] != "sess-other" {
		t.Errorf("another conversation lost its claim: %+v", got)
	}
	if got := reservationsFor(t, s, "case-lark"); len(got) != 1 || got[0].SessionID != "sess-case-2" {
		t.Errorf("claims on a case variant = %+v, want only the newest row of the one conversation", got)
	}
	// Collapsed out of the answer, not out of the table.
	recordIntact(t, s, "proxy-case-1", "sess-case-1", "Case-Lark", "conv-case")
	recordIntact(t, s, "proxy-case-2", "sess-case-2", "case-lark", "conv-case")
	recordIntact(t, s, "proxy-1", "sess-1", "azure-lark", "conv-a")
	recordIntact(t, s, "proxy-2", "sess-2", "azure-lark", "conv-a")
	recordIntact(t, s, "proxy-3", "sess-3", "azure-lark", "conv-a")
	recordIntact(t, s, "proxy-other", "sess-other", "azure-lark", "conv-b")
}

// TestReservations_KeepsClaimsItCannotShowAreOneConversation: a blank linkage is
// UNKNOWN, not a conversation, so those rows may not collapse into each other.
// The linked pair beside them is the positive control.
func TestReservations_KeepsClaimsItCannotShowAreOneConversation(t *testing.T) {
	s := newTestStore(t)
	seedRaw(t, s, "proxy-1", "gentle-mink", "sess-1", "", 1000)
	seedRaw(t, s, "proxy-2", "gentle-mink", "sess-2", "", 1001)
	seedRaw(t, s, "proxy-3", "linked-lark", "sess-3", "conv-a", 1000)
	seedRaw(t, s, "proxy-4", "linked-lark", "sess-4", "conv-a", 1001)

	if got := reservationsFor(t, s, "gentle-mink"); len(got) != 2 {
		t.Errorf("claims on gentle-mink = %+v, want both: a blank linkage is UNKNOWN, not a conversation", got)
	}
	if got := reservationsFor(t, s, "linked-lark"); len(got) != 1 || got[0].SessionID != "sess-4" {
		t.Errorf("claims on linked-lark = %+v, want only the newest of the one conversation — without "+
			"this the assertion above would hold for a store that never collapses anything", got)
	}
}

// TestReservations_NewestWinsRegardlessOfInsertOrder pins the determinism the doc
// advertises: the survivor may not depend on the order rows come back in, and a
// shared millisecond is resolved by the proxy session ID.
func TestReservations_NewestWinsRegardlessOfInsertOrder(t *testing.T) {
	for _, order := range [][2]string{{"p-a", "p-b"}, {"p-b", "p-a"}} {
		s := newTestStore(t)
		for _, proxy := range order {
			at := int64(10)
			if proxy == "p-b" {
				at = 11
			}
			seedRaw(t, s, proxy, "tame-lark", "sess-"+proxy, "conv-a", at)
		}
		got := reservationsFor(t, s, "tame-lark")
		if len(got) != 1 || got[0].SessionID != "sess-p-b" {
			t.Errorf("insert order %v: claims = %+v, want only the newer row (sess-p-b)", order, got)
		}
	}

	s := newTestStore(t)
	seedRaw(t, s, "p-a", "tied-lark", "sess-p-a", "conv-a", 10)
	seedRaw(t, s, "p-b", "tied-lark", "sess-p-b", "conv-a", 10)
	if got := reservationsFor(t, s, "tied-lark"); len(got) != 1 || got[0].SessionID != "sess-p-b" {
		t.Errorf("claims on a millisecond tie = %+v, want the deterministic survivor sess-p-b", got)
	}
}

// TestLegacyNameConflicts_ReportsOnlyWhatItCannotProveApart is the point of the
// collapse: the report is the residue, not the whole table.
func TestLegacyNameConflicts_ReportsOnlyWhatItCannotProveApart(t *testing.T) {
	s := newTestStore(t)
	// One conversation's own churn: collapsed, so not a conflict.
	seedRaw(t, s, "p-old", "churn-lark", "sess-old", "conv-a", 1000)
	seedRaw(t, s, "p-new", "churn-lark", "sess-new", "conv-a", 1001)
	// Two conversations: genuinely ambiguous, reported.
	seedRaw(t, s, "p-a", "split-lark", "sess-a", "conv-a", 1000)
	seedRaw(t, s, "p-b", "split-lark", "sess-b", "conv-b", 1001)
	// A blank linkage cannot be shown to be one conversation: reported.
	seedRaw(t, s, "p-u", "unknown-lark", "sess-u", "", 1000)
	seedRaw(t, s, "p-v", "unknown-lark", "sess-v", "", 1001)

	conflicts, err := s.LegacyNameConflicts()
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(conflicts))
	for _, c := range conflicts {
		names = append(names, c.Name)
	}
	if got := strings.Join(names, ","); got != "split-lark,unknown-lark" {
		t.Errorf("conflicts = %v, want exactly [split-lark unknown-lark] — a conversation's own "+
			"superseded generations are not a conflict", names)
	}
	for _, tc := range []struct{ proxy, sess, name, ext string }{
		{"p-old", "sess-old", "churn-lark", "conv-a"},
		{"p-new", "sess-new", "churn-lark", "conv-a"},
		{"p-a", "sess-a", "split-lark", "conv-a"},
		{"p-b", "sess-b", "split-lark", "conv-b"},
		{"p-u", "sess-u", "unknown-lark", ""},
		{"p-v", "sess-v", "unknown-lark", ""},
	} {
		recordIntact(t, s, tc.proxy, tc.sess, tc.name, tc.ext)
	}
}

// seedRaw writes an identity row straight to the table with a chosen timestamp.
// It is how a backlog written by an older build is represented, and the only way
// to order rows deterministically without sleeping through a millisecond.
func seedRaw(t *testing.T, s *Store, proxyID, name, sessionID, externalID string, updatedAt int64) {
	t.Helper()
	if _, err := s.db.Exec(
		`INSERT INTO session_names (proxy_session_id, name, plumb_session_id, external_id, name_revision, updated_at)
		 VALUES (?, ?, ?, ?, 1, ?)`,
		proxyID, name, sessionID, externalID, updatedAt,
	); err != nil {
		t.Fatalf("seedRaw(%s): %v", proxyID, err)
	}
}
