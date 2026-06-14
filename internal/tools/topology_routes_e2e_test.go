package tools_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/tools"
	"github.com/plumbkit/plumb/internal/topology"
	"github.com/plumbkit/plumb/internal/topology/extractors/wasmts"
)

// TestTopologyRoutes_ArgumentParserEndToEnd indexes a real Swift ArgumentParser
// command through the topology store and runs the topology_routes tool, proving
// the full path: extractor propagates the type's ParsableCommand conformance onto
// run()'s signature → FTS5 indexes it → the argument-parser.run pattern's query
// finds it → isRouteCandidate's nameEquals guard accepts run() (and only run()).
func TestTopologyRoutes_ArgumentParserEndToEnd(t *testing.T) {
	ws := t.TempDir()
	src := `import ArgumentParser

struct Greet: ParsableCommand {
    func run() throws {
        print("hello")
    }

    func validate() throws {}
}
`
	path := filepath.Join(ws, "Greet.swift")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	// Use the production Swift extractor (wasmts, the canonical-grammar WASM
	// route) so this e2e exercises the real route path, not the init-failure
	// fallback (treesitter.NewSwift).
	store, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
		[]topology.Extractor{wasmts.NewSwift()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	uri := "file://" + path
	// Wait for the WASM Swift extractor to index the file. The deadline is
	// generous: under -race on a loaded CI runner, wazero's cold start can take
	// several seconds, and a too-short wait makes route detection run against an
	// empty index — surfacing as a misleading "no route patterns matched".
	indexed := false
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if nodes, _ := store.SymbolsInFile(context.Background(), uri); len(nodes) > 0 {
			indexed = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !indexed {
		t.Fatal("timed out waiting for the Swift extractor to index Greet.swift")
	}

	tool := tools.NewTopologyRoutes(func() *topology.Store { return store })
	args, _ := json.Marshal(map[string]any{"framework": "argument-parser"})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("topology_routes Execute: %v", err)
	}

	if !strings.Contains(out, "run") || !strings.Contains(out, "argument-parser.run") {
		t.Errorf("expected run() detected as argument-parser.run entry point; got:\n%s", out)
	}
	// The sibling validate() must not be reported as a CLI entry point.
	if strings.Contains(out, "validate") {
		t.Errorf("validate() must not be flagged as a route; got:\n%s", out)
	}
}
