//go:build parity

// Parity sweep harness for the PLAN-1 WASM retirement gate. NOT part of the
// normal test suite (build tag `parity`); delete after the sweep.
//
// Runs the WASM (canonical tree-sitter) and pure-Go (gotreesitter v0.48.x)
// extractors over every .swift/.ts/.tsx file under PARITY_CORPUS and diffs the
// extracted node/edge sets per file.
// Usage: PARITY_CORPUS=<dir> go test -tags parity ./internal/topology/extractors/ -run TestParitySweep -v
//
// Note this file is invisible to `make verify` and to `golangci-lint run`, which
// do not pass -tags parity. Lint it explicitly before touching it:
// golangci-lint run --build-tags parity ./internal/topology/extractors/
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

// nodeKey identifies a node by every field a consumer can see. Signature and
// EndLine are part of the key deliberately: the Swift walks on both sides set
// Signature (file_outline renders it), so a kind|name|startLine key would call
// a signature regression "parity".
func nodeKey(n topology.Node) string {
	return fmt.Sprintf("%s|%s|%s|%d-%d", n.Kind, n.Name, n.Signature, n.StartLine, n.EndLine)
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

func diff(a, b []string) (onlyA, onlyB []string) {
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
	return onlyA, onlyB
}

// sweep accumulates the per-file verdicts.
type sweep struct {
	files   int
	perExt  map[string]int
	drifted int
	// extractionMisses counts files the pure-Go path could not extract at all
	// while WASM could. This — not an error mismatch — is how a gotreesitter
	// parse failure actually surfaces: every extractor swallows a failed parse
	// and returns (nil, nil, nil) with a NIL error, so comparing error values
	// finds nothing. The ~8% real-world failure rate is measured here.
	extractionMisses int
	errMismatches    int
	reports          []string
}

func (s *sweep) reportf(format string, args ...any) {
	s.reports = append(s.reports, fmt.Sprintf(format, args...))
}

// compare extracts rel through both engines and records the verdict.
func (s *sweep) compare(p pair, rel string, src []byte) {
	ctx := context.Background()
	wn, we, werr := p.wasm.Extract(ctx, rel, src)
	gn, ge, gerr := p.pure.Extract(ctx, rel, src)
	if (werr != nil) != (gerr != nil) {
		s.errMismatches++
		s.reportf("%s: error mismatch wasm=%v pure=%v", rel, werr, gerr)
		return
	}
	if len(wn) > 0 && len(gn) == 0 {
		s.extractionMisses++
		s.reportf("%s: pure-Go extracted nothing while wasm found %d nodes — gotreesitter parse failure", rel, len(wn))
		return
	}
	wNodes, wEdges := normalise(wn, we)
	gNodes, gEdges := normalise(gn, ge)
	nodeOnlyW, nodeOnlyG := diff(wNodes, gNodes)
	edgeOnlyW, edgeOnlyG := diff(wEdges, gEdges)
	if len(nodeOnlyW)+len(nodeOnlyG)+len(edgeOnlyW)+len(edgeOnlyG) == 0 {
		return
	}
	s.drifted++
	var b strings.Builder
	fmt.Fprintf(&b, "%s:\n", rel)
	for _, group := range []struct {
		label string
		items []string
	}{
		{"node only-wasm", nodeOnlyW},
		{"node only-pure", nodeOnlyG},
		{"edge only-wasm", edgeOnlyW},
		{"edge only-pure", edgeOnlyG},
	} {
		for _, item := range group.items {
			fmt.Fprintf(&b, "  %s:  %s\n", group.label, item)
		}
	}
	s.reports = append(s.reports, b.String())
}

// walk sweeps every corpus file whose extension has an extractor pair.
func (s *sweep) walk(root string, pairs map[string]pair) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "node_modules" || d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		// .jsx shares the TSX grammar; .d.ts is plain TS. Only the three keys above.
		ext := filepath.Ext(path)
		p, ok := pairs[ext]
		if !ok {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		s.files++
		s.perExt[ext]++
		s.compare(p, strings.TrimPrefix(path, root), src)
		return nil
	})
}

func TestParitySweep(t *testing.T) {
	root := os.Getenv("PARITY_CORPUS")
	if root == "" {
		t.Fatal("PARITY_CORPUS is unset — point it at the differential corpus (see the PLAN-1 card). " +
			"There is deliberately no default: a stale default path would sweep zero files and report a pass.")
	}
	pairs := map[string]pair{
		".swift": {wasm: wasmts.NewSwift(), pure: treesitter.NewSwift()},
		".ts":    {wasm: wasmts.NewTypeScript(), pure: treesitter.NewTypeScript()},
		".tsx":   {wasm: wasmts.NewTSX(), pure: treesitter.NewTSX()},
	}
	s := &sweep{perExt: map[string]int{}}

	if err := s.walk(root, pairs); err != nil {
		t.Fatal(err)
	}

	t.Logf("swept %d files %v: %d drifted, %d extraction misses, %d error mismatches",
		s.files, s.perExt, s.drifted, s.extractionMisses, s.errMismatches)
	for _, r := range s.reports {
		t.Log("\n" + r)
	}

	// A sweep that examined nothing must never read as a pass — this test is the
	// evidence for deleting wasmts/wazero, and "0 files, 0 drift" is otherwise
	// indistinguishable from "492 files, 0 drift".
	if s.files == 0 {
		t.Fatalf("swept 0 files under %s — wrong PARITY_CORPUS, or the corpus is empty", root)
	}
	for ext := range pairs {
		if s.perExt[ext] == 0 {
			t.Errorf("no %s files in the corpus — that language is unproven, so the flip is ungated for it", ext)
		}
	}
	if s.drifted > 0 || s.extractionMisses > 0 || s.errMismatches > 0 {
		t.Errorf("parity sweep: %d drifted, %d extraction misses, %d error mismatches (see log)",
			s.drifted, s.extractionMisses, s.errMismatches)
	}
}
