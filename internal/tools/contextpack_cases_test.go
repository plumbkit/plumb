package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// contextpackCase is the slice of testdata/contextpack/cases.json this test
// checks: the declared seeds and the gold lists. The gold is the evaluation
// oracle for context_for_task (PLAN-462), so it is validated on its own,
// before any collector exists to agree with it.
type contextpackCase struct {
	ID    string `json:"id"`
	Mode  string `json:"mode"`
	Seeds struct {
		Files   []string `json:"files"`
		Symbols []struct {
			Path string `json:"path"`
		} `json:"symbols"`
	} `json:"seeds"`
	SeedsEquivalent []struct {
		Symbols []struct {
			Path string `json:"path"`
		} `json:"symbols"`
	} `json:"seeds_equivalent"`
	Gold map[string]json.RawMessage `json:"gold"`
}

// goldFileOf returns the fixture file a gold item names: "path::selector",
// "path#Section", optionally followed by a " (note)".
func goldFileOf(item string) string {
	item, _, _ = strings.Cut(item, " (")
	if before, _, ok := strings.Cut(item, "::"); ok {
		return before
	}
	before, _, _ := strings.Cut(item, "#")
	return before
}

// TestContextpackCases_ResidualIsDerivedFromSeeds pins the residual rule the
// PLAN-462 contract measures expansion recall by: residual gold is every gold
// item outside the declared seed files. A hand-written list that disagrees
// would silently move the headline metric, so it is recomputed here.
func TestContextpackCases_ResidualIsDerivedFromSeeds(t *testing.T) {
	root := filepath.Join("testdata", "contextpack")
	raw, err := os.ReadFile(filepath.Join(root, "cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Cases []contextpackCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Cases) < 12 {
		t.Fatalf("%d cases; the contract requires at least 12 reviewed fixture cases", len(doc.Cases))
	}
	goldKeys := []string{"bodies", "related", "constraints", "gap_candidates", "members", "related_intrafile"}
	withResidual := 0
	for _, c := range doc.Cases {
		seedFiles := slices.Clone(c.Seeds.Files)
		for _, s := range c.Seeds.Symbols {
			if s.Path != "" {
				seedFiles = append(seedFiles, s.Path)
			}
		}
		for _, eq := range c.SeedsEquivalent {
			for _, s := range eq.Symbols {
				seedFiles = append(seedFiles, s.Path)
			}
		}
		var derived []string
		for _, key := range goldKeys {
			var items []string
			if rawItems, ok := c.Gold[key]; ok {
				if err := json.Unmarshal(rawItems, &items); err != nil {
					t.Fatalf("%s gold.%s: %v", c.ID, key, err)
				}
			}
			for _, it := range items {
				f := goldFileOf(it)
				if _, err := os.Stat(filepath.Join(root, "shop", f)); err != nil {
					t.Errorf("%s gold.%s names %q, which is not a fixture file", c.ID, key, f)
				}
				if !slices.Contains(seedFiles, f) && !slices.Contains(derived, it) {
					derived = append(derived, it)
				}
			}
		}
		var declared []string
		if rawRes, ok := c.Gold["residual"]; ok {
			if err := json.Unmarshal(rawRes, &declared); err != nil {
				t.Fatalf("%s gold.residual: %v", c.ID, err)
			}
		}
		slices.Sort(derived)
		slices.Sort(declared)
		if !slices.Equal(derived, declared) {
			t.Errorf("%s residual = %q, but the gold outside its seed files is %q", c.ID, declared, derived)
		}
		if len(declared) > 0 && c.Mode == "discovery" {
			withResidual++
		}
	}
	if withResidual < 2 {
		t.Errorf("%d discovery cases carry cross-file residual gold; expansion recall needs at least 2 in the fixture", withResidual)
	}
}
