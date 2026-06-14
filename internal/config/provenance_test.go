package config

import (
	"testing"
	"time"
)

func TestProvenance_RoundTrip(t *testing.T) {
	ws := t.TempDir()
	if err := RecordAgentWrite(ws, "log_level", ProvenanceEntry{Source: "agent", SessionID: "s1", Timestamp: time.Unix(1, 0)}); err != nil {
		t.Fatalf("RecordAgentWrite: %v", err)
	}
	prov, err := LoadProvenance(ws)
	if err != nil {
		t.Fatalf("LoadProvenance: %v", err)
	}
	if prov["log_level"].Source != "agent" || prov["log_level"].SessionID != "s1" {
		t.Errorf("round-trip mismatch: %+v", prov["log_level"])
	}
}

func TestProvenance_MissingIsEmpty(t *testing.T) {
	prov, err := LoadProvenance(t.TempDir())
	if err != nil || len(prov) != 0 {
		t.Errorf("missing sidecar should be empty, got %v err=%v", prov, err)
	}
}

func TestProvenance_Drop(t *testing.T) {
	ws := t.TempDir()
	_ = RecordAgentWrite(ws, "a", ProvenanceEntry{Source: "agent"})
	_ = RecordAgentWrite(ws, "b", ProvenanceEntry{Source: "agent"})
	if err := DropProvenance(ws, "a"); err != nil {
		t.Fatalf("DropProvenance: %v", err)
	}
	prov, _ := LoadProvenance(ws)
	if _, ok := prov["a"]; ok {
		t.Error("dropped key should be gone")
	}
	if _, ok := prov["b"]; !ok {
		t.Error("other key should remain")
	}
}
