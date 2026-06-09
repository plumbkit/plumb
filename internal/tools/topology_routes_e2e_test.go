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
	"github.com/plumbkit/plumb/internal/topology/extractors/treesitter"
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

	store, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
		[]topology.Extractor{treesitter.NewSwift()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	uri := "file://" + path
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if nodes, _ := store.SymbolsInFile(context.Background(), uri); len(nodes) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
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
