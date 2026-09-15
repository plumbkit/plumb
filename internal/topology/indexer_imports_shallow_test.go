package topology_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/topology"
	goext "github.com/plumbkit/plumb/internal/topology/extractors/golang"
)

// indexer_imports_shallow_test.go covers the import resolver at the depth it is
// actually sensitive to: how far from the workspace root a package sits.
//
// The resolver keys packages by workspace-relative DIRECTORY and refused any
// candidate shorter than two path segments, so that `import "strings"` could not
// bind to a local strings/ directory. Applied to the directory rather than to
// the import NAME, that rule also refused every package sitting one directory
// deep: for `example.com/m/stats` the loop only ever formed
// `example.com/m/stats` and `m/stats` — never `stats`. A repository whose
// packages live at the top level (api/, store/, cli/ — the ordinary shape for a
// small module) therefore produced ZERO cross-package import edges, and
// topology_affected silently fell back to co-located tests with nothing in its
// output saying the dependency arm had found nothing.
//
// This is driven through a real Open plus the real Go extractor on purpose. The
// unit test over matchImportDir picks its own two-segment directory keys and
// cannot see this; neither can the end-to-end test that inserts nodes by hand
// with those same keys. Only an indexer walking a real tree decides how deep the
// packages are.

// writeSourceTree writes each rel → content under root, creating directories.
func writeSourceTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
}

// TestPackageGraph_LinksImportsAtEveryPackageDepth indexes the same two
// packages twice, identical but for how deep they sit, and requires the import
// edge in both. The shallow arm is the regression: it reported no edges at all.
//
// Both arms also carry a local strings/ package and a real `import "strings"`,
// because the depth rule was standing in for the collision rule. Loosening it
// must not start binding the stdlib name to the local directory that shadows it
// — the false positive the whole minimum exists to refuse.
func TestPackageGraph_LinksImportsAtEveryPackageDepth(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
	}{
		{"deep layout (internal/…)", "internal/"},
		{"shallow layout (top-level packages)", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			statsDir, cliDir := tc.prefix+"stats", tc.prefix+"cli"
			writeSourceTree(t, ws, map[string]string{
				"go.mod": "module example.com/m\n\ngo 1.25\n",
				statsDir + "/stats.go": "package stats\n\n" +
					"func Savings() int { return 41 }\n",
				cliDir + "/report.go": "package cli\n\n" +
					"import (\n" +
					"\t\"strings\"\n\n" +
					"\t\"example.com/m/" + statsDir + "\"\n" +
					")\n\n" +
					"func Report() string { return strings.TrimSpace(\" x \") }\n\n" +
					"func Total() int { return stats.Savings() }\n",
				// A local package that shadows a stdlib name. It must stay unlinked.
				"strings/strings.go": "package strings\n\n" +
					"func Local() {}\n",
			})

			store, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
				[]topology.Extractor{goext.New()})
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })

			// Wait on the EDGE, not on the nodes. linkImports runs at the END of an
			// index pass, so a file's symbols are queryable well before its import
			// edges exist; polling node visibility and then asserting on edges is
			// what once made this look impossible to test from a fixture at all.
			ctx := context.Background()
			var graph *topology.PackageGraph
			deadline := time.Now().Add(30 * time.Second)
			for time.Now().Before(deadline) {
				time.Sleep(100 * time.Millisecond)
				g, gerr := store.PackageGraph(ctx)
				if gerr != nil {
					t.Fatalf("package graph: %v", gerr)
				}
				graph = g
				if g.Edges[cliDir][statsDir] {
					break
				}
			}
			if graph == nil || !graph.Edges[cliDir][statsDir] {
				t.Fatalf("no import edge %s → %s after indexing; a package this deep is "+
					"invisible to the import resolver, so topology_affected sees no "+
					"dependency at all (edges: %v)", cliDir, statsDir, edgesOf(graph))
			}
			if graph.Edges[cliDir]["strings"] {
				t.Errorf(`import "strings" was linked to the local strings/ package; a ` +
					`single-segment import with no module prefix to strip must stay refused`)
			}
		})
	}
}

// edgesOf renders a graph's edges for a failure message, nil-safe.
func edgesOf(g *topology.PackageGraph) map[string]map[string]bool {
	if g == nil {
		return nil
	}
	return g.Edges
}
