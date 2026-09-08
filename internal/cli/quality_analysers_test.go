package cli

import (
	"slices"
	"testing"

	"github.com/plumbkit/plumb/internal/quality"
)

// TestBuildAnalysersCoversRegistry pins the constructor switch to the registry.
//
// The two have to be separate — only cli may import the adapter packages, and
// the registry has to stay importable by them — so nothing but this test stops
// them drifting. Drift here is not cosmetic: a name marked Implemented with no
// case resolves in the Settings pane, in `plumb doctor` and in the skipped-entry
// log as a working analyser, and then quietly does nothing at the one place it
// matters. That is the exact bug shape this whole change exists to remove, so it
// must not be reintroducible by adding a registry row and forgetting the case.
func TestBuildAnalysersCoversRegistry(t *testing.T) {
	want := quality.ImplementedNames()
	got := buildAnalysers(want, nil)
	if len(got) != len(want) {
		t.Fatalf("buildAnalysers(%v) returned %d analyser(s), want %d — a registry row "+
			"marked Implemented has no case in buildAnalysers", want, len(got), len(want))
	}
	for i, a := range got {
		if a.Name() != want[i] {
			t.Errorf("analyser %d = %q, want %q (order must follow the configured list)", i, a.Name(), want[i])
		}
	}
}

// The other direction: a case in the switch with no registry row would build an
// analyser the Settings pane calls unrecognised and doctor tells the user to
// delete. Every name the switch accepts must be a registry name.
func TestBuildAnalysers_EveryConstructedNameIsInTheRegistry(t *testing.T) {
	names := make([]string, 0, len(quality.Tools()))
	for _, tool := range quality.Tools() {
		names = append(names, tool.Name)
	}
	// Probe with every registry name plus a few that are definitely not, so a
	// case added for an unregistered name shows up as an extra analyser.
	probe := append(slices.Clone(names), "not-a-tool", "/usr/local/bin/ruff", "")
	for _, a := range buildAnalysers(probe, nil) {
		if _, ok := quality.ToolByName(a.Name()); !ok {
			t.Errorf("buildAnalysers constructed %q, which has no registry row", a.Name())
		}
	}
}

// An unrecognised entry must be skipped rather than crash or construct
// something. It is skipped LOUDLY now (logSkippedAnalysersOnce), but the
// skipping itself is what keeps a typo in a config file from breaking writes.
func TestBuildAnalysers_SkipsUnknownEntries(t *testing.T) {
	got := buildAnalysers([]string{"golangci-lint", "eslint", "/Users/x/.local/bin/ruff", "typo"}, nil)
	if len(got) != 1 || got[0].Name() != "golangci-lint" {
		t.Fatalf("want only the one implemented entry, got %d: %v", len(got), analyserNames(got))
	}
}

func analyserNames(as []quality.Analyser) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.Name())
	}
	return out
}
