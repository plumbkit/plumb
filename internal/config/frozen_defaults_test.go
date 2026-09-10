package config

import (
	"slices"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestFindFrozenDefaults_NoFalsePositivesOnCustomValues(t *testing.T) {
	customTOML := `
theme = "dark"

[edits]
strict = true

[git]
allow_destructive = true
protected_branches = ["main", "master", "develop"]

[quality]
analysers = ["golangci-lint", "staticcheck"]

[[command]]
name = "custom-test"
command = "go test ./..."
`
	got := FindFrozenDefaults([]byte(customTOML))
	if len(got) != 0 {
		t.Fatalf("expected no frozen defaults for customized config, got: %v", got)
	}
}

func TestFindFrozenDefaults_DetectsKnownFrozenDefaults(t *testing.T) {
	frozenTOML := `
command = []

[git]
protected_branches = ["main", "master"]

[quality]
analysers = ["golangci-lint"]

[topology]
exclude_patterns = []

[workspace]
extra_roots = []
read_roots = []

[lsp.go]
args = []
`
	got := FindFrozenDefaults([]byte(frozenTOML))
	want := []string{
		"command",
		"git.protected_branches",
		"lsp.go.args",
		"quality.analysers",
		"topology.exclude_patterns",
		"workspace.extra_roots",
		"workspace.read_roots",
	}

	if !slices.Equal(got, want) {
		t.Fatalf("FindFrozenDefaults mismatch:\n got: %v\nwant: %v", got, want)
	}
}

func TestPruneFrozenDefaults_RoundTrip(t *testing.T) {
	mixedTOML := `
theme = "nord"
command = []

[git]
allow_push = true
protected_branches = ["main", "master"]

[quality]
analysers = ["golangci-lint"]

[edits]
strict = true
`
	pruned, removed, err := PruneFrozenDefaults([]byte(mixedTOML))
	if err != nil {
		t.Fatalf("PruneFrozenDefaults failed: %v", err)
	}

	wantRemoved := []string{
		"command",
		"git.protected_branches",
		"quality.analysers",
	}
	if !slices.Equal(removed, wantRemoved) {
		t.Fatalf("removed keys mismatch:\n got: %v\nwant: %v", removed, wantRemoved)
	}

	// Verify pruned TOML no longer has frozen defaults
	remainingFrozen := FindFrozenDefaults(pruned)
	if len(remainingFrozen) != 0 {
		t.Fatalf("pruned TOML still has frozen keys: %v", remainingFrozen)
	}

	// Verify custom keys survived round-trip
	var m map[string]any
	if err := toml.Unmarshal(pruned, &m); err != nil {
		t.Fatalf("unmarshaling pruned TOML: %v", err)
	}
	if m["theme"] != "nord" {
		t.Errorf("theme key lost or corrupted: got %v", m["theme"])
	}
	gitTable, ok := m["git"].(map[string]any)
	if !ok || gitTable["allow_push"] != true {
		t.Errorf("git.allow_push lost or corrupted: got %v", m["git"])
	}
	if _, hasPB := gitTable["protected_branches"]; hasPB {
		t.Errorf("git.protected_branches was not pruned")
	}
	editsTable, ok := m["edits"].(map[string]any)
	if !ok || editsTable["strict"] != true {
		t.Errorf("edits.strict lost or corrupted: got %v", m["edits"])
	}
}

func TestFindFrozenDefaults_EmptyAndInvalidTOML(t *testing.T) {
	if got := FindFrozenDefaults(nil); got != nil {
		t.Errorf("expected nil for nil input, got %v", got)
	}
	if got := FindFrozenDefaults([]byte("")); got != nil {
		t.Errorf("expected nil for empty input, got %v", got)
	}
	if got := FindFrozenDefaults([]byte("invalid toml [[[]")); got != nil {
		t.Errorf("expected nil for invalid TOML, got %v", got)
	}
}
