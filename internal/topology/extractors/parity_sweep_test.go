//go:build parity

// Parity sweep harness for the PLAN-1 WASM retirement gate. NOT part of the
// normal test suite (build tag `parity`); delete after the sweep.
//
// Runs the WASM (canonical tree-sitter) and pure-Go (gotreesitter v0.47.x)
// extractors over every .swift/.ts/.tsx file under PARITY_CORPUS (default
// /tmp/parity-corpus) and diffs the extracted node/edge sets per file.
// Usage: go test -tags parity ./internal/topology/extractors/ -run TestParitySweep -v
package extractors_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
	"github.com/plumbkit/plumb/internal/topology/extractors/treesitter"
	"github.com/plumbkit/plumb/internal/topology/extractors/wasmts"
)

type pair struct {
	wasm *wasmts.Extractor
	pure topology.Extractor
}

func nodeKey(n topology.Node) string {
	return fmt.Sprintf("%s|%s|%d", n.Kind, n.Name, n.StartLine)
}

func normalise(nodes []topology.Node, edges []topology.Edge) (ns, es []string) {
	for _, n := range nodes {
		ns = append(ns, nodeKey(n))
	}
	sort.Strings(ns)
	for _, e := range edges {
		from, to := "?", "?"
		if e.FromID >= 0 && int(e.FromID) < len(nodes) {
			from = nodeKey(nodes[e.FromID])
		}
		if e.ToID >= 0 && int(e.ToID) < len(nodes) {
			to = nodeKey(nodes[e.ToID])
		}
		es = append(es, fmt.Sprintf("%s|%s|%s", e.Kind, from, to))
	}
	sort.Strings(es)
	return ns, es
}

func diff(label string, a, b []string) (onlyA, onlyB []string) {
	ca, cb := map[string]int{}, map[string]int{}
	for _, s := range a {
		ca[s]++
	}
	for _, s := range b {
		cb[s]++
	}
	for s, n := range ca {
		if cb[s] < n {
			onlyA = append(onlyA, fmt.Sprintf("%s x%d", s, n-cb[s]))
		}
	}
	for s, n := range cb {
		if ca[s] < n {
			onlyB = append(onlyB, fmt.Sprintf("%s x%d", s, n-ca[s]))
		}
	}
	_ = label
	return onlyA, onlyB
}

func TestParitySweep(t *testing.T) {
	root := os.Getenv("PARITY_CORPUS")
	if root == "" {
		root = "/tmp/parity-corpus"
	}
	pairs := map[string]pair{
		".swift": {wasm: wasmts.NewSwift(), pure: treesitter.NewSwift()},
		".ts":    {wasm: wasmts.NewTypeScript(), pure: treesitter.NewTypeScript()},
		".tsx":   {wasm: wasmts.NewTSX(), pure: treesitter.NewTSX()},
	}

	ctx := context.Background()
	files, drifted, parseMisses := 0, 0, 0
	var reports []string

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "node_modules" || d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(path)
		// .jsx shares the TSX grammar; .d.ts is plain TS. Only the three keys above.
		p, ok := pairs[ext]
		if !ok {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		files++
		rel := strings.TrimPrefix(path, root)
		wn, we, werr := p.wasm.Extract(ctx, rel, src)
		gn, ge, gerr := p.pure.Extract(ctx, rel, src)
		if (werr != nil) != (gerr != nil) {
			parseMisses++
			reports = append(reports, fmt.Sprintf("%s: error mismatch wasm=%v pure=%v", rel, werr, gerr))
			return nil
		}
		wNodes, wEdges := normalise(wn, we)
		gNodes, gEdges := normalise(gn, ge)
		nodeOnlyW, nodeOnlyG := diff("nodes", wNodes, gNodes)
		edgeOnlyW, edgeOnlyG := diff("edges", wEdges, gEdges)
		if len(nodeOnlyW)+len(nodeOnlyG)+len(edgeOnlyW)+len(edgeOnlyG) > 0 {
			drifted++
			var b strings.Builder
			fmt.Fprintf(&b, "%s:\n", rel)
			for _, s := range nodeOnlyW {
				fmt.Fprintf(&b, "  node only-wasm:  %s\n", s)
			}
			for _, s := range nodeOnlyG {
				fmt.Fprintf(&b, "  node only-pure:  %s\n", s)
			}
			for _, s := range edgeOnlyW {
				fmt.Fprintf(&b, "  edge only-wasm:  %s\n", s)
			}
			for _, s := range edgeOnlyG {
				fmt.Fprintf(&b, "  edge only-pure:  %s\n", s)
			}
			reports = append(reports, b.String())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("swept %d files: %d drifted, %d error mismatches", files, drifted, parseMisses)
	for _, r := range reports {
		t.Log("\n" + r)
	}
	if drifted > 0 || parseMisses > 0 {
		t.Errorf("parity sweep: %d drifted files, %d error mismatches (see log)", drifted, parseMisses)
	}
}
