package tools

// admission_gates_test.go — the derived-call admission gates that had no test
// of their own (PLAN-462 seam 1 mutation-checked them): topology_affected's
// use of an admitted cross-file call edge, and function reachability's refusal
// in a workspace with no admitted Go package.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/topology"
	goext "github.com/plumbkit/plumb/internal/topology/extractors/golang"
)

func openGoTopology(t *testing.T, ws string) *topology.Store {
	t.Helper()
	store, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024}, []topology.Extractor{goext.New()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func writeWS(t *testing.T, ws, rel, src string) {
	t.Helper()
	p := filepath.Join(ws, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A test in another package that calls the changed function directly is
// reached through the admitted cross-file call edge, so it is reported for the
// dependency edge, not merely because its package imports the changed one.
func TestTopologyAffected_AdmittedCrossFileCallReachesTheCallingTest(t *testing.T) {
	ws := t.TempDir()
	writeWS(t, ws, "go.mod", "module example.com/project\n\ngo 1.22\n")
	writeWS(t, ws, "internal/target/target.go", "package target\n\nfunc Mid() {}\n")
	writeWS(t, ws, "internal/caller/caller_test.go",
		"package caller\n\nimport (\n\t\"testing\"\n\n\t\"example.com/project/internal/target\"\n)\n\nfunc TestUsesMid(t *testing.T) { target.Mid() }\n")
	store := openGoTopology(t, ws)
	tool := NewTopologyAffected(func() *topology.Store { return store })

	var last *affectedResult
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		res, err := tool.run(context.Background(), store, topologyAffectedArgs{Symbols: []string{"Mid"}})
		if err == nil && res != nil {
			last = res
			for _, tc := range res.Tests {
				if tc.Node.Name == "TestUsesMid" && tc.Reason == reasonGraph {
					return
				}
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	var got []string
	if last != nil {
		for _, tc := range last.Tests {
			got = append(got, tc.Node.Name+" ("+tc.Reason+")")
		}
	}
	t.Fatalf("TestUsesMid was not reached through the admitted call edge; tests: %s", strings.Join(got, ", "))
}

// With no admitted Go package there is no function-level call graph, and the
// tool says so rather than answering from an empty one.
func TestTopologyImpact_FunctionReachabilityRefusedWithoutAGoPackage(t *testing.T) {
	ws := t.TempDir()
	writeWS(t, ws, "notes.txt", "nothing to index\n")
	store := openGoTopology(t, ws)
	tool := NewTopologyImpact(func() *topology.Store { return store })
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"mode":"reachability","granularity":"function","roots":["main"]}`))
	if err != nil {
		t.Fatalf("function reachability: %v", err)
	}
	if !strings.Contains(out, "cross-file call edges are unavailable") {
		t.Fatalf("a workspace with no Go package was answered from an empty graph:\n%s", out)
	}
}
