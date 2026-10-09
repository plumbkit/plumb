package tools

// catalogue_budget_test.go — PLAN-413 phase 3: the byte budget for the whole
// tools/list catalogue, and a deterministic per-tool size report.
//
// Every client pays for the catalogue: at discovery, in its prompt cache, and
// on (nearly) every model step on today's clients (measured on the wire in
// cmd/clientsmoke; see docs/token-efficiency.md). TestPinnedSetBudget guards
// the pinned subset; this guards the full set, so one verbose tool cannot
// quietly undo the compaction of #612.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// maxCatalogueBytes is the full 59-tool tools/list payload budget. Measured,
// not guessed: 73,361 bytes after the PLAN-413 compaction (from 113,668, and
// against the card's 108,332-byte baseline), plus about 5% headroom for the
// next tool or two. It is a ratchet. When a change goes over it, trim
// descriptions or parameter prose first; raise it only with a reviewed reason,
// and lower it whenever a trim leaves more than the headroom unused.
const maxCatalogueBytes = 77000

// TestCatalogueBudget guards maxCatalogueBytes.
func TestCatalogueBudget(t *testing.T) {
	set := append(leanToolSet(), nonLeanToolSet()...)
	full := payloadBytes(t, set)
	t.Logf("full tools/list payload: %d bytes (%d tools)", full, len(set))
	if full > maxCatalogueBytes {
		t.Errorf("full tools/list payload is %d bytes, over the %d-byte budget — run `make tool-sizes` and trim the largest descriptions or parameter prose",
			full, maxCatalogueBytes)
	}
}

// toolSize is one tool's share of the catalogue, by component.
type toolSize struct {
	name                     string
	nameB, descB, schemaB, n int
}

// toolSizes measures every tool by component: the JSON-encoded name,
// description and (compacted) input schema, and the whole tools/list entry.
func toolSizes(t *testing.T) []toolSize {
	t.Helper()
	set := append(leanToolSet(), nonLeanToolSet()...)
	out := make([]toolSize, 0, len(set))
	for _, tl := range set {
		size := func(v any) int {
			b, err := json.Marshal(v)
			if err != nil {
				t.Fatalf("marshal %s: %v", tl.Name(), err)
			}
			return len(b)
		}
		out = append(out, toolSize{
			name:    tl.Name(),
			nameB:   size(tl.Name()),
			descB:   size(tl.Description()),
			schemaB: size(tl.InputSchema()),
			n:       size(toolDef{Name: tl.Name(), Description: tl.Description(), InputSchema: tl.InputSchema()}),
		})
	}
	// Largest first; the name breaks ties so the report is deterministic.
	sort.Slice(out, func(i, j int) bool {
		if out[i].n != out[j].n {
			return out[i].n > out[j].n
		}
		return out[i].name < out[j].name
	})
	return out
}

// renderToolSizes is the report `make tool-sizes` prints.
func renderToolSizes(sizes []toolSize, pinned func(string) bool) string {
	var sb strings.Builder
	var name, desc, schema, total int
	fmt.Fprintf(&sb, "%-24s %7s %7s %7s %7s  %s\n", "tool", "total", "desc", "schema", "name", "pinned")
	for _, s := range sizes {
		mark := ""
		if pinned(s.name) {
			mark = "yes"
		}
		fmt.Fprintf(&sb, "%-24s %7d %7d %7d %7d  %s\n", s.name, s.n, s.descB, s.schemaB, s.nameB, mark)
		name, desc, schema, total = name+s.nameB, desc+s.descB, schema+s.schemaB, total+s.n
	}
	fmt.Fprintf(&sb, "%-24s %7d %7d %7d %7d\n", fmt.Sprintf("TOTAL (%d tools)", len(sizes)), total, desc, schema, name)
	return sb.String()
}

// TestToolSizeReport prints the per-tool size report under -v. Its assertions
// keep the report honest: every tool appears once and the components add up.
func TestToolSizeReport(t *testing.T) {
	sizes := toolSizes(t)
	seen := map[string]bool{}
	for _, s := range sizes {
		if seen[s.name] {
			t.Errorf("%s appears twice in the report", s.name)
		}
		seen[s.name] = true
		// A tools/list entry is the three components plus a fixed JSON frame;
		// a component missing from the sum would show up as a negative frame.
		if frame := s.n - s.nameB - s.descB - s.schemaB; frame < 0 || frame > 64 {
			t.Errorf("%s: total %d does not decompose into name %d + description %d + schema %d (frame %d bytes)",
				s.name, s.n, s.nameB, s.descB, s.schemaB, frame)
		}
	}
	t.Log("\n" + renderToolSizes(sizes, IsPinned))
}
