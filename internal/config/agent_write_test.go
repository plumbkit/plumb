package config

import (
	"os"
	"testing"
)

func TestAgentApplyBatch_WritesAndStampsProvenance(t *testing.T) {
	ws := t.TempDir()
	base := Defaults()
	changed, err := AgentApplyBatch(base, ws,
		map[string]any{"log_level": "warn", "tasks.go.lint": "golangci-lint run"},
		ProvenanceEntry{Source: "agent", SessionID: "s1"})
	if err != nil {
		t.Fatalf("AgentApplyBatch: %v", err)
	}
	if len(changed) != 2 {
		t.Fatalf("changed = %v, want 2 keys", changed)
	}
	merged, err := LoadProject(base, ws)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if merged.LogLevel != "warn" {
		t.Errorf("log_level = %q, want warn", merged.LogLevel)
	}
	if merged.Tasks["go"].Lint != "golangci-lint run" {
		t.Errorf("tasks.go.lint = %q", merged.Tasks["go"].Lint)
	}
	prov, _ := LoadProvenance(ws)
	if prov["log_level"].Source != "agent" || prov["tasks.go.lint"].Source != "agent" {
		t.Errorf("provenance not stamped: %+v", prov)
	}
}

func TestAgentApplyBatch_RejectsNonWritableKey(t *testing.T) {
	ws := t.TempDir()
	_, err := AgentApplyBatch(Defaults(), ws, map[string]any{"git.allow_push": true}, ProvenanceEntry{})
	if err == nil {
		t.Fatal("expected a non-writable key to be refused")
	}
	if _, statErr := os.Stat(ProjectConfigPath(ws)); !os.IsNotExist(statErr) {
		t.Error("no config file should be written after a refused batch")
	}
}

func TestAgentApplyBatch_PartialInvalidRollsBack(t *testing.T) {
	ws := t.TempDir()
	// log_level "bad" is an invalid enum; the whole batch must be a no-op even
	// though tasks.go.lint is valid.
	_, err := AgentApplyBatch(Defaults(), ws,
		map[string]any{"log_level": "bad", "tasks.go.lint": "golangci-lint run"}, ProvenanceEntry{})
	if err == nil {
		t.Fatal("expected the invalid enum to reject the batch")
	}
	if _, statErr := os.Stat(ProjectConfigPath(ws)); !os.IsNotExist(statErr) {
		t.Error("no config file should be written on a rejected batch (atomicity)")
	}
}

func TestAgentApplyBatch_EmptyBatch(t *testing.T) {
	if _, err := AgentApplyBatch(Defaults(), t.TempDir(), nil, ProvenanceEntry{}); err == nil {
		t.Error("expected an error for an empty batch")
	}
}

// TestAgentApplyBatch_RecordsPriorValueFromFoldVariantTable pins the fourth
// walker of the raw map (#319): priorProjectValues reads through getNested,
// which was still exact-match after lookupNested/setNested/deleteNested were
// folded. setNested writes THROUGH the `[TASKS.go]` table, so an exact-match
// miss recorded "no previous value" for a write that in fact overwrote one
// — losing the prior value the provenance sidecar exists to display on revert.
func TestAgentApplyBatch_RecordsPriorValueFromFoldVariantTable(t *testing.T) {
	ws := t.TempDir()
	writeRawProjectConfig(t, ws, "[TASKS.go]\nlint = \"old-linter\"\n")

	changed, err := AgentApplyBatch(Defaults(), ws,
		map[string]any{"tasks.go.lint": "new-linter"},
		ProvenanceEntry{Source: "agent", SessionID: "s1"})
	if err != nil {
		t.Fatalf("AgentApplyBatch: %v", err)
	}
	if len(changed) != 1 {
		t.Fatalf("changed = %v, want 1 key", changed)
	}

	prov, err := LoadProvenance(ws)
	if err != nil {
		t.Fatalf("LoadProvenance: %v", err)
	}
	entry, ok := prov["tasks.go.lint"]
	if !ok {
		t.Fatal("no provenance entry for tasks.go.lint")
	}
	if entry.Previous == nil {
		data, _ := os.ReadFile(ws + "/.plumb/config.toml")
		t.Fatalf("provenance recorded NO previous value, but [TASKS.go] held one; file now:\n%s", data)
	}
	if *entry.Previous != "old-linter" {
		t.Errorf("previous = %q, want \"old-linter\"", *entry.Previous)
	}
}

// TestAgentApplyBatch_RecordsPriorValueFromAFoldVariantTopLevelKey covers the
// one-segment path through getNested, which the nested case cannot reach: a
// top-level key has no table to descend, so the leaf fold is the only thing
// standing between `plumb config unset` and a revert to a value that was never
// in force.
func TestAgentApplyBatch_RecordsPriorValueFromAFoldVariantTopLevelKey(t *testing.T) {
	ws := t.TempDir()
	writeRawProjectConfig(t, ws, "LOG_LEVEL = \"warn\"\n")

	if _, err := AgentApplyBatch(Defaults(), ws,
		map[string]any{"log_level": "error"},
		ProvenanceEntry{Source: "agent", SessionID: "s1"}); err != nil {
		t.Fatalf("AgentApplyBatch: %v", err)
	}

	prov, err := LoadProvenance(ws)
	if err != nil {
		t.Fatalf("LoadProvenance: %v", err)
	}
	entry, ok := prov["log_level"]
	if !ok {
		t.Fatal("no provenance entry for log_level")
	}
	if entry.Previous == nil {
		t.Fatal("provenance recorded NO previous value, but LOG_LEVEL held one")
	}
	if *entry.Previous != "warn" {
		t.Errorf("previous = %q, want \"warn\"", *entry.Previous)
	}
}
