package sessionstate

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func newNamespace(t *testing.T, s *Store) (Namespace, string) {
	t.Helper()
	token, id, err := NewNamespaceToken()
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	now := time.Now()
	ns := Namespace{ID: id, TokenHash: HashNamespaceToken(token), TTL: 2 * time.Hour, CreatedAt: now, ExpiresAt: now.Add(2 * time.Hour), OwnerPID: 42, OwnerStart: 7}
	if err := s.CreateNamespace(ns); err != nil {
		t.Fatalf("create: %v", err)
	}
	return ns, token
}

// populate writes a row into every proxy-session table for psid.
func populate(t *testing.T, s *Store, psid, name string) {
	t.Helper()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.UpsertRead(psid, "/w", "/w/a.go", time.Now(), "sha"))
	must(s.UpsertPin(psid, "/w", "go", PinSourceSessionStart))
	must(s.RecordLogicalAgent(psid, "agent-1"))
	must(s.RecordDeclaredLinkage(psid, "conv-1"))
	must(s.SaveIdentity(psid, Identity{Name: name, SessionID: "sid-" + psid}))
	_, err := s.MintResumeCredential(psid, HashResumeSecret("rsk1-"+psid))
	must(err)
}

func rowCount(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil { //nolint:gosec // test: table from a constant list
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestNamespace_TokenLookupAndStates(t *testing.T) {
	s := newTestStore(t)
	ns, token := newNamespace(t, s)
	if !strings.HasPrefix(token, NamespaceTokenPrefix) || !strings.HasPrefix(ns.ID, "ns-") {
		t.Fatalf("token %q / id %q lack their prefixes", token, ns.ID)
	}
	got, err := s.NamespaceByToken(token)
	if err != nil || got.ID != ns.ID || got.State != NamespaceActive {
		t.Fatalf("by token = %+v, %v; want active %s", got, err, ns.ID)
	}
	if _, err := s.NamespaceByToken(token + "x"); !errors.Is(err, ErrNamespaceUnknown) {
		t.Fatalf("a wrong token: err = %v, want ErrNamespaceUnknown", err)
	}
	if err := s.RevokeNamespace(ns.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.NamespaceByID(ns.ID); got.State != NamespaceRevoked {
		t.Fatalf("state after revoke = %q", got.State)
	}
	// A revoked namespace cannot be renewed back to life.
	if err := s.SetNamespaceExpiry(ns.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.NamespaceByID(ns.ID); got.ExpiresAt.After(ns.ExpiresAt) {
		t.Fatal("a revoked namespace's expiry moved")
	}
}

// TestNamespace_MemberBelongsToOneNamespace: a proxy session joins one namespace
// for life; joining another is refused, never a silent move.
func TestNamespace_MemberBelongsToOneNamespace(t *testing.T) {
	s := newTestStore(t)
	a, _ := newNamespace(t, s)
	b, _ := newNamespace(t, s)
	if err := s.AddNamespaceMember(a.ID, "psid-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddNamespaceMember(a.ID, "psid-1"); err != nil {
		t.Fatalf("re-joining the same namespace: %v", err)
	}
	if err := s.AddNamespaceMember(b.ID, "psid-1"); err == nil {
		t.Fatal("a proxy session moved to a second namespace")
	}
	if got, _ := s.NamespaceOf("psid-1"); got != a.ID {
		t.Fatalf("NamespaceOf = %q, want %q", got, a.ID)
	}
	if got, _ := s.NamespaceOf("psid-default"); got != "" {
		t.Fatalf("a non-member's namespace = %q, want the default", got)
	}
}

// TestPurgeNamespace_RemovesOnlyItsMembersRows: the release acceptance — every
// namespaced row is gone and the default namespace's rows are unchanged, by
// row count per table.
func TestPurgeNamespace_RemovesOnlyItsMembersRows(t *testing.T) {
	s := newTestStore(t)
	ns, _ := newNamespace(t, s)
	other, _ := newNamespace(t, s)
	populate(t, s, "psid-default", "calm-otter")
	populate(t, s, "psid-other", "keen-lynx")
	if err := s.AddNamespaceMember(other.ID, "psid-other"); err != nil {
		t.Fatal(err)
	}
	before := map[string]int{}
	for _, table := range namespacedTables {
		before[table] = rowCount(t, s, table)
	}
	for _, psid := range []string{"psid-ns-1", "psid-ns-2"} {
		populate(t, s, psid, "ns-"+psid)
		if err := s.AddNamespaceMember(ns.ID, psid); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddNamespaceRoot(ns.ID, "/w"); err != nil {
		t.Fatal(err)
	}
	for _, table := range namespacedTables {
		if rowCount(t, s, table) <= before[table] {
			t.Fatalf("control: populate wrote no %s rows for the namespace", table)
		}
	}

	members, err := s.PurgeNamespace(ns.ID)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(members)
	if !slices.Equal(members, []string{"psid-ns-1", "psid-ns-2"}) {
		t.Fatalf("purged members = %v", members)
	}
	for _, table := range namespacedTables {
		if got := rowCount(t, s, table); got != before[table] {
			t.Fatalf("%s: %d rows after purge, want %d (the default and other namespaces' rows only)", table, got, before[table])
		}
	}
	if _, err := s.NamespaceByID(ns.ID); !errors.Is(err, ErrNamespaceUnknown) {
		t.Fatal("the namespace itself survived its purge")
	}
	if roots, _ := s.NamespaceRoots(ns.ID); len(roots) != 0 {
		t.Fatalf("roots survived: %v", roots)
	}
	if got, _ := s.NamespaceOf("psid-other"); got != other.ID {
		t.Fatal("purging one namespace dropped another's membership")
	}
}

// TestPurgeNamespace_CoversEveryProxySessionTable: every table with a
// proxy_session_id column is in namespacedTables, so a table added later cannot
// be forgotten by the purge.
func TestPurgeNamespace_CoversEveryProxySessionTable(t *testing.T) {
	s := newTestStore(t)
	rows, err := s.db.Query(`SELECT m.name FROM sqlite_master m, pragma_table_info(m.name) p
		WHERE m.type = 'table' AND p.name = 'proxy_session_id' ORDER BY m.name`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		if name != "namespace_member" {
			tables = append(tables, name)
		}
	}
	want := slices.Clone(namespacedTables)
	slices.Sort(want)
	if !slices.Equal(tables, want) {
		t.Fatalf("tables keyed by proxy_session_id = %v, purge covers %v", tables, want)
	}
}

// TestNamespaceSessionIDs lists the session files a purge must remove: each
// member's own session and each logical agent's roster row, nothing from the
// default namespace.
func TestNamespaceSessionIDs(t *testing.T) {
	s := newTestStore(t)
	ns, _ := newNamespace(t, s)
	populate(t, s, "psid-default", "calm-otter")
	populate(t, s, "psid-ns", "keen-lynx")
	if err := s.RecordRosterIdentity("psid-ns", "agent-1", "brave-wren", "sid-agent"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddNamespaceMember(ns.ID, "psid-ns"); err != nil {
		t.Fatal(err)
	}
	got, err := s.NamespaceSessionIDs(ns.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"sid-agent", "sid-psid-ns"}) {
		t.Fatalf("session IDs = %v, want the member's own and its agent's", got)
	}
}
