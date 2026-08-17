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
