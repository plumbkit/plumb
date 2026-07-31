package config

import (
	"os"
	"path/filepath"
	"strings"
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

func TestProvenance_GitignoresSidecar(t *testing.T) {
	ws := t.TempDir()
	if err := RecordAgentWrite(ws, "log_level", ProvenanceEntry{Source: "agent"}); err != nil {
		t.Fatalf("RecordAgentWrite: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(ws, ".plumb", ".gitignore"))
	if err != nil {
		t.Fatalf("expected .plumb/.gitignore: %v", err)
	}
	if !strings.Contains(string(data), "config.provenance.json") {
		t.Errorf(".gitignore should exclude the provenance sidecar:\n%s", data)
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

// TestProvenance_GitignoreEntryNotSuppressedBySubstring pins the exact-line
// matching of the gitignore append. The original hand-rolled implementation
// used a substring test, so any line merely containing the entry — a
// commented-out copy, a negation, a longer path — convinced it the entry was
// already present and the sidecar was silently left unignored.
func TestProvenance_GitignoreEntryNotSuppressedBySubstring(t *testing.T) {
	ws := t.TempDir()
	giPath := filepath.Join(ws, ".plumb", ".gitignore")
	if err := os.MkdirAll(filepath.Dir(giPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(giPath, []byte("#config.provenance.json\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := RecordAgentWrite(ws, "log_level", ProvenanceEntry{Source: "agent"}); err != nil {
		t.Fatalf("RecordAgentWrite: %v", err)
	}
	data, err := os.ReadFile(giPath)
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	bare := 0
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.TrimSpace(line) == "config.provenance.json" {
			bare++
		}
	}
	if bare != 1 {
		t.Errorf("config.provenance.json must appear as its own line exactly once, got %d:\n%s", bare, data)
	}
	if !strings.Contains(string(data), "#config.provenance.json\n") {
		t.Errorf("pre-existing commented-out line was lost:\n%s", data)
	}
}
