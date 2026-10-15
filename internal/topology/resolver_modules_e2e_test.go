package topology_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/topology"
	"github.com/plumbkit/plumb/internal/topology/extractors/treesitter"
)

// writeTree writes files under a new workspace and returns it.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	ws := t.TempDir()
	for name, src := range files {
		p := filepath.Join(ws, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return ws
}

// openIndexed opens ws with the given extractors and waits for the first pass.
func openIndexed(t *testing.T, ws string, files int, ex ...topology.Extractor) *topology.Store {
	t.Helper()
	store, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024}, ex)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if st := store.Status(); st.IndexerState == "idle" && st.IndexedFiles >= files {
			return store
		}
	}
	t.Fatalf("index not ready: %+v", store.Status())
	return nil
}

// resolvedCallees lists "callee@path" for every call-resolver edge out of the
// declaration sel resolves to in path.
func resolvedCallees(t *testing.T, store *topology.Store, path, sel string) []string {
	t.Helper()
	ctx := context.Background()
	nodes, err := store.ResolveNodes(ctx, sel, topology.NodeHint{Path: path})
	if err != nil || len(nodes) != 1 {
		t.Fatalf("resolve %s#%s: %v %+v", path, sel, err, nodes)
	}
	n, err := store.ExploreFrom(ctx, nodes[0], topology.ExploreOpts{
		Depth: 1, MaxNodes: 50, Direction: topology.DirectionOutward,
		EdgeKinds: []string{string(topology.EdgeCalls)}, IncludeDerivedCalls: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[int64]topology.Node{}
	for _, x := range n.Nodes {
		byID[x.ID] = x
	}
	var out []string
	for _, e := range n.Edges {
		if e.Source == "call-resolver" && e.FromID == nodes[0].ID {
			out = append(out, byID[e.ToID].Name+"@"+byID[e.ToID].Path)
		}
	}
	sort.Strings(out)
	return out
}

// TestModuleResolver_Python runs real Python through the extractor, the index and
// the module resolver: aliased, absolute, relative and submodule imports, a
// member of an imported class, self and cls calls, and two same-named methods
// whose calls must stay their own.
func TestModuleResolver_Python(t *testing.T) {
	ws := writeTree(t, map[string]string{
		"pkg/__init__.py": "from .util import helper as h\n\ndef top():\n    return h()\n",
		"pkg/util.py": "def helper():\n    return 1\n\n" +
			"class Store:\n    def save(self):\n        return self.flush()\n\n    def flush(self):\n        return 2\n\n" +
			"    @classmethod\n    def make(cls):\n        return cls.flush()\n",
		"pkg/svc.py": "import pkg.util\nfrom pkg import util\nfrom pkg.util import Store\nfrom . import util as u2\n\n" +
			"def serve():\n    pkg.util.helper()\n    util.helper()\n    Store.save(None)\n    u2.helper()\n    unknown()\n\n" +
			"class A:\n    def run(self):\n        return self.go()\n\n    def go(self):\n        return 1\n\n" +
			"class B:\n    def run(self):\n        return self.stop()\n\n    def stop(self):\n        return 2\n",
	})
	store := openIndexed(t, ws, 3, treesitter.NewPython())
	cases := []struct {
		path, sel string
		want      []string
	}{
		{"pkg/__init__.py", "top", []string{"helper@pkg/util.py"}},
		{"pkg/svc.py", "serve", []string{"helper@pkg/util.py", "save@pkg/util.py"}},
		{"pkg/util.py", "save", []string{"flush@pkg/util.py"}},
		{"pkg/util.py", "make", []string{"flush@pkg/util.py"}},
	}
	for _, c := range cases {
		if got := resolvedCallees(t, store, c.path, c.sel); !slices.Equal(got, c.want) {
			t.Errorf("%s#%s calls %v, want %v", c.path, c.sel, got, c.want)
		}
	}
	// A.run and B.run share a name: each keeps its own self call.
	ctx := context.Background()
	runs, err := store.ResolveNodes(ctx, "run", topology.NodeHint{Path: "pkg/svc.py"})
	if err != nil {
		t.Fatal(err)
	}
	var methods []string
	for _, r := range runs {
		if r.Kind != topology.KindMethod {
			continue
		}
		n, err := store.ExploreFrom(ctx, r, topology.ExploreOpts{
			Depth: 1, MaxNodes: 20, Direction: topology.DirectionOutward,
			EdgeKinds: []string{string(topology.EdgeCalls)}, IncludeDerivedCalls: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range n.Edges {
			if e.FromID != r.ID || e.Source != "call-resolver" {
				continue
			}
			for _, x := range n.Nodes {
				if x.ID == e.ToID {
					methods = append(methods, x.Name)
				}
			}
		}
	}
	sort.Strings(methods)
	if !slices.Equal(methods, []string{"go", "stop"}) {
		t.Errorf("A.run and B.run resolve to %v, want [go stop], one each", methods)
	}
}

// TestModuleResolver_TypeScriptAndJavaScript is the same for the JS family: named,
// aliased, namespace and default imports, the ".js" specifier TypeScript's ESM
// convention uses for a .ts source, a CommonJS require from JavaScript into
// TypeScript, `new`, `this`, an arrow function bound to a const, and two classes'
// same-named methods.
func TestModuleResolver_TypeScriptAndJavaScript(t *testing.T) {
	ws := writeTree(t, map[string]string{
		"src/util.ts": "export function helper(): number { return 1 }\n" +
			"export class Store {\n  save() { return this.flush() }\n  flush() { return 2 }\n}\n" +
			"export default function main() {}\n",
		"src/svc.ts": "import { helper as h, Store } from \"./util\";\nimport * as u from \"./util.js\";\n" +
			"import def from \"./util\";\nimport { readFileSync } from \"fs\";\n\n" +
			"export function serve() {\n  h();\n  u.helper();\n  new Store();\n  def();\n  readFileSync(\"x\");\n}\n" +
			"export const run = () => { return u.helper(); };\n" +
			"class A { run() { return this.go(); } go() { return 1; } }\n" +
			"class B { run() { return this.stop(); } stop() { return 2; } }\n",
		"lib/legacy.js": "const util = require(\"../src/util\");\nfunction legacy() { return util.helper(); }\nmodule.exports = { legacy };\n",
	})
	store := openIndexed(t, ws, 3, treesitter.NewTypeScript(), treesitter.NewJavaScript())
	cases := []struct {
		path, sel string
		want      []string
	}{
		{"src/svc.ts", "serve", []string{"Store@src/util.ts", "helper@src/util.ts"}},
		{"src/util.ts", "save", []string{"flush@src/util.ts"}},
		{"lib/legacy.js", "legacy", []string{"helper@src/util.ts"}},
	}
	for _, c := range cases {
		if got := resolvedCallees(t, store, c.path, c.sel); !slices.Equal(got, c.want) {
			t.Errorf("%s#%s calls %v, want %v", c.path, c.sel, got, c.want)
		}
	}
	ctx := context.Background()
	runs, err := store.ResolveNodes(ctx, "run", topology.NodeHint{Path: "src/svc.ts"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range runs {
		n, err := store.ExploreFrom(ctx, r, topology.ExploreOpts{
			Depth: 1, MaxNodes: 20, Direction: topology.DirectionOutward,
			EdgeKinds: []string{string(topology.EdgeCalls)}, IncludeDerivedCalls: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range n.Edges {
			if e.FromID != r.ID || e.Source != "call-resolver" {
				continue
			}
			for _, x := range n.Nodes {
				if x.ID == e.ToID {
					got = append(got, string(r.Kind)+">"+x.Name)
				}
			}
		}
	}
	sort.Strings(got)
	if want := []string{"function>helper", "method>go", "method>stop"}; !slices.Equal(got, want) {
		t.Errorf("the three runs resolve to %v, want %v (the arrow const, A.run, B.run)", got, want)
	}
}
