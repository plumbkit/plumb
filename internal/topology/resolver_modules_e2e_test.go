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
			"    def again(self):\n        return self.again()\n\n" +
			"    @classmethod\n    def make(cls):\n        return cls.flush()\n",
		"pkg/svc.py": "import pkg.util\nfrom pkg import util\nfrom pkg.util import Store\nfrom . import util as u2\n\n" +
			"def serve():\n    pkg.util.helper()\n    util.helper()\n    Store.save(None)\n    u2.helper()\n    unknown()\n\n" +
			"def via_submodule():\n    return util.helper()\n\n" +
			"def via_alias():\n    return u2.helper()\n\n" +
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
		{"pkg/svc.py", "via_submodule", []string{"helper@pkg/util.py"}},
		{"pkg/svc.py", "via_alias", []string{"helper@pkg/util.py"}},
		{"pkg/util.py", "again", nil},
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
			"export function viaNamespace() { return u.helper(); }\n" +
			"export function viaDefault() { return def(); }\n" +
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
		{"src/svc.ts", "viaNamespace", []string{"helper@src/util.ts"}},
		{"src/svc.ts", "viaDefault", nil},
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

// An absolute Python import resolves to the one indexed module whose path ends
// that way inside a real package, and to none when several do: a guessed module
// is never an edge. A repository file that only shares a module's name is not
// that module: app/json.py is not the standard library's json. A single-file
// top-level module resolves at the root.
func TestModuleResolver_PythonAbsoluteImportsBySuffix(t *testing.T) {
	ws := writeTree(t, map[string]string{
		"src/only/__init__.py": "",
		"src/only/mod.py":      "def f():\n    return 1\n",
		"src/a/__init__.py":    "",
		"src/a/mod.py":         "def g():\n    return 1\n",
		"lib/a/__init__.py":    "",
		"lib/a/mod.py":         "def g():\n    return 2\n",
		"loose/b/mod.py":       "def h():\n    return 3\n",
		"app/json.py":          "def dumps(x):\n    return x\n",
		"util.py":              "def tool():\n    return 4\n",
		"app/main.py": "import only.mod\nimport a.mod\nimport b.mod\nimport json\nimport util\n\n" +
			"def unique():\n    return only.mod.f()\n\ndef ambiguous():\n    return a.mod.g()\n\n" +
			"def not_a_package():\n    return b.mod.h()\n\ndef stdlib():\n    return json.dumps(1)\n\n" +
			"def root_module():\n    return util.tool()\n",
	})
	store := openIndexed(t, ws, 10, treesitter.NewPython())
	for sel, want := range map[string][]string{"not_a_package": nil, "stdlib": nil, "root_module": {"tool@util.py"}} {
		if got := resolvedCallees(t, store, "app/main.py", sel); !slices.Equal(got, want) {
			t.Errorf("%s calls %v, want %v", sel, got, want)
		}
	}
	if got := resolvedCallees(t, store, "app/main.py", "unique"); !slices.Equal(got, []string{"f@src/only/mod.py"}) {
		t.Errorf("unique calls %v, want [f@src/only/mod.py]", got)
	}
	if got := resolvedCallees(t, store, "app/main.py", "ambiguous"); len(got) != 0 {
		t.Errorf("ambiguous calls %v, want none: two modules end in a/mod.py", got)
	}
}

// qualifiedCallees lists the qualified names of the call-resolver targets of the
// one declaration sel names in path.
func qualifiedCallees(t *testing.T, store *topology.Store, path, sel string) []string {
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
	var out []string
	for _, e := range n.Edges {
		if e.Source != "call-resolver" || e.FromID != nodes[0].ID {
			continue
		}
		for _, x := range n.Nodes {
			if x.ID == e.ToID {
				out = append(out, x.Qualified)
			}
		}
	}
	sort.Strings(out)
	return out
}

// reindex rewrites rel and waits until the index holds its new content.
func reindex(t *testing.T, store *topology.Store, ws, rel, src string) {
	t.Helper()
	before, _, _ := store.IndexedContentHash(context.Background(), rel)
	if err := os.WriteFile(filepath.Join(ws, rel), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	store.Enqueue(filepath.Join(ws, rel))
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		h, ok, _ := store.IndexedContentHash(context.Background(), rel)
		if ok && h != before && store.Status().IndexerState == "idle" {
			return
		}
	}
	t.Fatalf("%s was not re-indexed", rel)
}

// A scoped re-index of a callee file keeps each importer's edge on the one
// declaration it named (A.run, never B.run, which shares the name), and gives an
// importer in another directory the edge to a declaration the callee gained,
// without waiting for a full rebuild.
func TestModuleResolver_ScopedReindexOfACallee(t *testing.T) {
	py := "class A:\n    def run(self):\n        return 1\n\n\nclass B:\n    def run(self):\n        return 2\n"
	ts := "export class A { run() { return 1; } }\nexport class B { run() { return 2; } }\n"
	ws := writeTree(t, map[string]string{
		"pkg/__init__.py": "",
		"pkg/m.py":        py,
		"pkg/util.py":     "def other():\n    return 0\n",
		"app/main.py":     "from pkg.m import A, fresh\n\ndef caller():\n    return A.run(None)\n\ndef uses_fresh():\n    return fresh()\n",
		// Imports only the submodule, so nothing else puts it in the scope.
		"tools/only_util.py": "from pkg import util\n\ndef via_util():\n    return util.later()\n",
		"src/m.ts":           ts,
		"web/main.ts":        "import { A, fresh } from \"../src/m\";\nexport function caller() { return A.run(); }\nexport function usesFresh() { return fresh(); }\n",
	})
	store := openIndexed(t, ws, 7, treesitter.NewPython(), treesitter.NewTypeScript())
	check := func(when string) {
		t.Helper()
		if got := qualifiedCallees(t, store, "app/main.py", "caller"); !slices.Equal(got, []string{"A.run"}) {
			t.Errorf("%s: python caller resolves to %v, want [A.run]", when, got)
		}
		if got := qualifiedCallees(t, store, "web/main.ts", "caller"); !slices.Equal(got, []string{"A.run"}) {
			t.Errorf("%s: typescript caller resolves to %v, want [A.run]", when, got)
		}
	}
	check("before")
	if got := qualifiedCallees(t, store, "app/main.py", "uses_fresh"); len(got) != 0 {
		t.Fatalf("control: fresh does not exist yet, but uses_fresh resolves to %v", got)
	}
	if got := qualifiedCallees(t, store, "tools/only_util.py", "via_util"); len(got) != 0 {
		t.Fatalf("control: util.later does not exist yet, but via_util resolves to %v", got)
	}
	reindex(t, store, ws, "pkg/m.py", py+"\n\ndef fresh():\n    return 3\n")
	// A submodule a from-import named (from pkg import util) makes its importer
	// part of the scope too.
	reindex(t, store, ws, "pkg/util.py", "def other():\n    return 0\n\n\ndef later():\n    return 5\n")
	if got := qualifiedCallees(t, store, "tools/only_util.py", "via_util"); !slices.Equal(got, []string{"later"}) {
		t.Errorf("python via_util resolves to %v after util.later was added, want [later]", got)
	}
	reindex(t, store, ws, "src/m.ts", ts+"export function fresh() { return 3; }\n")
	check("after the callees were re-indexed")
	if got := qualifiedCallees(t, store, "app/main.py", "uses_fresh"); !slices.Equal(got, []string{"fresh"}) {
		t.Errorf("python uses_fresh resolves to %v after fresh was added, want [fresh]", got)
	}
	if got := qualifiedCallees(t, store, "web/main.ts", "usesFresh"); !slices.Equal(got, []string{"fresh"}) {
		t.Errorf("typescript usesFresh resolves to %v after fresh was added, want [fresh]", got)
	}
}

// A name declared twice at a module's top level is no answer; a directory import
// finds its index file; a parent-relative import climbs; a name bound twice to
// different modules (try/except ImportError) is no answer either.
func TestModuleResolver_AmbiguityIndexAndParentImports(t *testing.T) {
	ws := writeTree(t, map[string]string{
		"pkg/__init__.py":     "",
		"pkg/dup.py":          "def dup():\n    return 1\n\n\ndef dup():\n    return 2\n",
		"pkg/util.py":         "def up():\n    return 1\n",
		"pkg/fast.py":         "def f():\n    return 1\n",
		"pkg/nest.py":         "def outer():\n    def helper():\n        return 1\n    return helper()\n\n\ndef helper():\n    return 2\n",
		"pkg/slow.py":         "def f():\n    return 2\n",
		"pkg/sub/__init__.py": "",
		"pkg/sub/mod.py": "from ..dup import dup\nfrom ..util import up\nfrom ..nest import helper\ntry:\n    from ..fast import f\nexcept ImportError:\n    from ..slow import f\n\n" +
			"def ambiguous():\n    return dup()\n\ndef climbs():\n    return up()\n\ndef rebound():\n    return f()\n\n" +
			"def top_level_only():\n    return helper()\n",
		"src/lib/index.ts": "export function fromIndex() { return 1; }\n",
		"src/app.ts":       "import { fromIndex } from \"./lib\";\nexport function viaIndex() { return fromIndex(); }\n",
	})
	store := openIndexed(t, ws, 10, treesitter.NewPython(), treesitter.NewTypeScript())
	for _, c := range []struct {
		path, sel string
		want      []string
	}{
		{"pkg/sub/mod.py", "ambiguous", nil},
		{"pkg/sub/mod.py", "climbs", []string{"up@pkg/util.py"}},
		{"pkg/sub/mod.py", "rebound", nil},
		// outer's nested helper is no import target, so the top-level one is the one.
		{"pkg/sub/mod.py", "top_level_only", []string{"helper@pkg/nest.py"}},
		{"src/app.ts", "viaIndex", []string{"fromIndex@src/lib/index.ts"}},
	} {
		if got := resolvedCallees(t, store, c.path, c.sel); !slices.Equal(got, c.want) {
			t.Errorf("%s#%s calls %v, want %v", c.path, c.sel, got, c.want)
		}
	}
}
