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
