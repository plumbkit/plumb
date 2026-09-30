package sessionstate

import (
	"testing"
	"time"
)

// TestPerAgentReadIsolation pins the v6 logical_agent_id dimension: reads
// recorded under one agent are invisible to another, and both are distinct from
// the connection-level ("") rows.
func TestPerAgentReadIsolation(t *testing.T) {
	s := newTestStore(t)
	mtime := time.Unix(1_700_000_000, 0)

	if err := s.UpsertReadForAgent("proxyX", "agent-a", "/ws", "/ws/a.go", mtime, "sha-a"); err != nil {
		t.Fatalf("UpsertReadForAgent a: %v", err)
	}
	if err := s.UpsertReadForAgent("proxyX", "agent-b", "/ws", "/ws/b.go", mtime, "sha-b"); err != nil {
		t.Fatalf("UpsertReadForAgent b: %v", err)
	}

	a, err := s.LoadReadsForAgent("proxyX", "agent-a", "/ws")
	if err != nil || len(a) != 1 || a[0].Path != "/ws/a.go" {
		t.Fatalf("agent-a reads = %v (err %v), want just /ws/a.go", a, err)
	}
	b, err := s.LoadReadsForAgent("proxyX", "agent-b", "/ws")
	if err != nil || len(b) != 1 || b[0].Path != "/ws/b.go" {
		t.Fatalf("agent-b reads = %v (err %v), want just /ws/b.go", b, err)
	}
	conn, err := s.LoadReads("proxyX", "/ws")
	if err != nil || len(conn) != 0 {
		t.Fatalf("connection-level reads = %v (err %v), want none", conn, err)
	}
}

// TestPerAgentPinIsolation pins that a per-agent pin and the connection-level
// pin do not overwrite each other.
func TestPerAgentPinIsolation(t *testing.T) {
	s := newTestStore(t)

	if err := s.UpsertPin("proxyX", "/conn", "go", PinSourceRoots); err != nil {
		t.Fatalf("UpsertPin conn: %v", err)
	}
	if err := s.UpsertPinForAgent("proxyX", "agent-a", "/ws-a", "zig", PinSourceSessionStart); err != nil {
		t.Fatalf("UpsertPinForAgent: %v", err)
	}

	connRoot, _, _, connOK, err := s.LoadPin("proxyX")
	if err != nil || !connOK || connRoot != "/conn" {
		t.Fatalf("connection pin = %q ok=%v err=%v, want /conn", connRoot, connOK, err)
	}
	aRoot, aLang, _, aOK, err := s.LoadPinForAgent("proxyX", "agent-a")
	if err != nil || !aOK || aRoot != "/ws-a" || aLang != "zig" {
		t.Fatalf("agent pin = %q/%q ok=%v err=%v, want /ws-a/zig", aRoot, aLang, aOK, err)
	}
}

// TestDeclaredLinkageRoundTripAndPrune pins the v9 declared_linkage record
// (issue #513): declarations are scoped by proxy session, blank values are
// dropped, a re-declaration refreshes rather than duplicates, and Prune reclaims
// an aged row unless its proxy session is live.
func TestDeclaredLinkageRoundTripAndPrune(t *testing.T) {
	s := newTestStore(t)
	for _, l := range []string{"conv-a", "conv-b", "conv-a", ""} {
		if err := s.RecordDeclaredLinkage("proxyX", l); err != nil {
			t.Fatalf("RecordDeclaredLinkage %q: %v", l, err)
		}
	}
	if err := s.RecordDeclaredLinkage("proxyY", "conv-other"); err != nil {
		t.Fatalf("RecordDeclaredLinkage proxyY: %v", err)
	}
	got, err := s.DeclaredLinkagesFor("proxyX")
	if err != nil {
		t.Fatalf("DeclaredLinkagesFor: %v", err)
	}
	if len(got) != 2 || !containsAll(got, "conv-a", "conv-b") {
		t.Fatalf("declared linkages = %v, want exactly conv-a and conv-b", got)
	}

	// proxyY is live, so only proxyX's rows are reclaimed.
	if err := s.Prune(time.Now().Add(time.Hour), "proxyY"); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if got, _ := s.DeclaredLinkagesFor("proxyX"); len(got) != 0 {
		t.Errorf("aged declarations survived Prune: %v", got)
	}
	if got, _ := s.DeclaredLinkagesFor("proxyY"); len(got) != 1 {
		t.Errorf("a live proxy's declaration was pruned: %v", got)
	}
}

func containsAll(have []string, want ...string) bool {
	set := map[string]bool{}
	for _, h := range have {
		set[h] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

// TestTouchDeclaredLinkageRefreshesButNeverInserts: an admitted call keeps an
// existing declaration young, but admission is not declaration, so a linkage
// with no row never gains one.
func TestTouchDeclaredLinkageRefreshesButNeverInserts(t *testing.T) {
	s := newTestStore(t)
	if err := s.RecordDeclaredLinkage("proxyX", "conv-a"); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := s.BackdateLogicalAgents("proxyX", time.Now().Add(-48*time.Hour)); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	if err := s.TouchDeclaredLinkage("proxyX", "conv-a"); err != nil {
		t.Fatalf("touch: %v", err)
	}
	if err := s.TouchDeclaredLinkage("proxyX", "never-declared"); err != nil {
		t.Fatalf("touch undeclared: %v", err)
	}
	if err := s.Prune(time.Now().Add(-24 * time.Hour)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	got, err := s.DeclaredLinkagesFor("proxyX")
	if err != nil {
		t.Fatalf("DeclaredLinkagesFor: %v", err)
	}
	if len(got) != 1 || got[0] != "conv-a" {
		t.Fatalf("after touch + prune = %v, want only the refreshed conv-a", got)
	}
}
