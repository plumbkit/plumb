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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
)

// maxCatalogueBytes is the full 59-tool catalogue budget, measured as the
// tools/list result the LARGEST client receives (wireCatalogueBytes): with
// the identity argument declared on every schema, as a client that strips
// undeclared arguments gets it, and the alwaysLoad _meta on pinned tools.
// Measured, not guessed: 77,839 bytes after the PLAN-413 compaction (from
// about 118 KB, against the card's 108,332-byte core baseline), plus about 5%
// headroom. It is a ratchet. When a change goes over it, trim descriptions or
// parameter prose first; raise it only with a reviewed reason, and lower it
// whenever a trim leaves more than the headroom unused.
const maxCatalogueBytes = 82000

// wireCatalogueBytes serves tools/list for set on a real mcp.Server
// configured as the largest client sees it, and returns the byte size of the
// result: exactly what goes on the wire, envelope and _meta included.
func wireCatalogueBytes(t *testing.T, set []describable) int {
	t.Helper()
	srv := mcp.New(mcp.ServerInfo{Name: "plumb", Version: "budget"})
	for _, tl := range set {
		mt, ok := tl.(mcp.Tool)
		if !ok {
			t.Fatalf("%s does not implement mcp.Tool", tl.Name())
		}
		srv.Register(mt)
	}
	srv.DeclareIdentityArg = func() bool { return true }
	srv.AlwaysLoad = IsPinned
	var out bytes.Buffer
	req := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n"
	if err := srv.Serve(context.Background(), strings.NewReader(req), &out); err != nil {
		t.Fatalf("serve tools/list: %v", err)
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil || len(resp.Result) == 0 {
		t.Fatalf("decode tools/list response (%v): %s", err, out.String())
	}
	var check struct {
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &check); err != nil || len(check.Tools) != len(set) {
		t.Fatalf("tools/list returned %d tools, want %d (%v)", len(check.Tools), len(set), err)
	}
	return len(resp.Result)
}

// TestCatalogueBudget guards maxCatalogueBytes.
func TestCatalogueBudget(t *testing.T) {
	set := append(leanToolSet(), nonLeanToolSet()...)
	wire := wireCatalogueBytes(t, set)
	t.Logf("full tools/list result, worst-case client: %d bytes (%d tools); core name+description+schema: %d bytes",
		wire, len(set), payloadBytes(t, set))
	if wire > maxCatalogueBytes {
		t.Errorf("full tools/list result is %d bytes, over the %d-byte budget — run `make tool-sizes` and trim the largest descriptions or parameter prose",
			wire, maxCatalogueBytes)
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

// renderToolSizes is the report `make tool-sizes` prints. Its TOTAL row sums
// the entries; the budgets above measure the served tools/list result, which
// adds the envelope, commas, the identity argument and _meta.
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
	empty, _ := json.Marshal(toolDef{Name: "", Description: "", InputSchema: json.RawMessage("{}")})
	entryFrame := len(empty) - len(`""`) - len(`""`) - len(`{}`)
	sizes := toolSizes(t)
	seen := map[string]bool{}
	for _, s := range sizes {
		if seen[s.name] {
			t.Errorf("%s appears twice in the report", s.name)
		}
		seen[s.name] = true
		// An entry is exactly its three components plus the fixed JSON frame
		// of an empty entry, so a component dropped from (or double-counted
		// in) the report cannot pass.
		if frame := s.n - s.nameB - s.descB - s.schemaB; frame != entryFrame {
			t.Errorf("%s: total %d does not decompose into name %d + description %d + schema %d + the %d-byte frame (got %d)",
				s.name, s.n, s.nameB, s.descB, s.schemaB, entryFrame, frame)
		}
	}
	t.Log("\n" + renderToolSizes(sizes, IsPinned))
}
