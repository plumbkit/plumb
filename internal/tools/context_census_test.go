package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

// TestCouldCall pins which open calls of a name could be calls of a declaration
// in internal/lib. Each false row is a call a "complete" claim may ignore; each
// true row is one it must not.
func TestCouldCall(t *testing.T) {
	const dir = "internal/lib"
	call := func(p, kind string, dot bool) topology.OpenCall {
		return topology.OpenCall{Path: p, Kind: kind, DotImport: dot}
	}
	for _, c := range []struct {
		name             string
		o                topology.OpenCall
		method, exported bool
		want             bool
	}{
		{"bare call of a function, own package", call("internal/lib/a.go", "bare", false), false, true, true},
		{"bare call of a function, another package", call("internal/app/a.go", "bare", false), false, true, false},
		{"bare call through a dot import", call("internal/app/a.go", "bare", true), false, true, true},
		{"dot import cannot reach an unexported name", call("internal/app/a.go", "bare", true), false, false, false},
		{"bare call is never a method", call("internal/lib/a.go", "bare", false), true, true, false},
		{"block-scoped local may still be the function", call("internal/lib/a.go", "local", false), false, false, true},
		{"receiver call of a method", call("internal/app/a.go", "receiver", false), true, true, true},
		{"receiver call is never a function", call("internal/app/a.go", "receiver", false), false, true, false},
		{"unexported method from another package", call("internal/app/a.go", "receiver", false), true, false, false},
		{"package value's method (unmatched)", call("internal/app/a.go", "unmatched", false), true, true, true},
		{"top-level call could be either", call("internal/app/a.go", "top-level", false), false, true, true},
		{"same-file ambiguity could be either", call("internal/lib/a.go", "same-file", false), true, false, true},
		{"unread call could be either", call("internal/app/a.go", "unresolved", false), true, true, true},
		{"stale outcome could be either", call("internal/app/a.go", "stale", false), true, true, true},
		{"external call naming the seed's package", topology.OpenCall{Path: "internal/app/a.go", Kind: "external", Qualifier: "lib"}, false, true, true},
		{"external call naming another package", topology.OpenCall{Path: "internal/app/a.go", Kind: "external", Qualifier: "fmt"}, false, true, false},
		{"external call is never a method", topology.OpenCall{Path: "internal/app/a.go", Kind: "external", Qualifier: "lib"}, true, true, false},
	} {
		if got := couldCall(c.o, censusSeed{method: c.method, exported: c.exported, dir: dir, pkg: "lib"}); got != c.want {
			t.Errorf("%s: couldCall = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestContextForTask_CensusCoversAFileSeedsDeclarations: agents mostly pass files,
// so a file seed's top declarations get the census too, with the shared caveat
// said once. Control: the open calls of Total are named, not claimed complete.
func TestContextForTask_CensusCoversAFileSeedsDeclarations(t *testing.T) {
	s := newShop(t)
	out, err := s.run(t, map[string]any{"files": []string{"cart/cart.go"}, "max_bytes": 32000})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "callers of (*Cart).Total: 0 in the index") ||
		!strings.Contains(out, "unresolved call(s) of Total may also be it: api/checkout.go:13 (receiver)") {
		t.Errorf("a file seed's declaration has no census line:\n%s", out)
	}
	if n := strings.Count(out, labelCensus); n != 1 {
		t.Errorf("the census caveat appears %d times, want once:\n%s", n, out)
	}
	if strings.Contains(out, "callers of (*Cart).Total: complete") {
		t.Errorf("open calls of Total were claimed complete:\n%s", out)
	}
}

// TestCallerCounts: a neighbourhood cut at the per-hop ceiling gives no count at
// all, never a short one; the declaration itself (recursion) is not its caller;
// callers the scope leaves out are counted apart.
func TestCallerCounts(t *testing.T) {
	in := func(p string) bool { return strings.HasPrefix(p, "a/") }
	nb := &topology.Neighbourhood{Nodes: []topology.Node{{ID: 1, Path: "a/x.go"}, {ID: 2, Path: "a/y.go"}, {ID: 3, Path: "b/z.go"}, {ID: 9, Path: "a/self.go"}}}
	if i, o, ok := callerCounts(nb, 9, in); !ok || i != 2 || o != 1 {
		t.Errorf("callerCounts = %d in, %d out, ok %v; want 2, 1, true", i, o, ok)
	}
	nb.Truncated = true
	if _, _, ok := callerCounts(nb, 9, in); ok {
		t.Error("a truncated neighbourhood gave a count")
	}
	if i, o, ok := callerCounts(nil, 9, in); !ok || i+o != 0 {
		t.Errorf("no neighbourhood: %d, %d, %v; want 0, 0, true", i, o, ok)
	}
}

// TestNoteOpen: two calls on one line are one place, the named places
// stop at the cap, and "and k more" counts only the calls no named place holds.
func TestNoteOpen(t *testing.T) {
	var c callerNote
	noteOpen(&c, "a.go:1 (receiver)")
	noteOpen(&c, "a.go:1 (receiver)")
	for i := range contextCensusSites {
		noteOpen(&c, fmt.Sprintf("b.go:%d (receiver)", i+2))
	}
	if c.Open != contextCensusSites+2 || len(c.OpenAt) != contextCensusSites || c.OpenShown != contextCensusSites+1 {
		t.Errorf("Open %d, places %d, shown %d; want %d, %d, %d", c.Open, len(c.OpenAt), c.OpenShown, contextCensusSites+2, contextCensusSites, contextCensusSites+1)
	}
	c.Census, c.Name, c.Selector = true, "M", "(*T).M"
	if txt := c.censusText(); !strings.Contains(txt, " and 1 more") || strings.Count(txt, "a.go:1") != 1 {
		t.Errorf("a place is listed twice, or the rest is miscounted: %s", txt)
	}
}

const censusModule = "module example.com/c\n\ngo 1.22\n"

// TestContextForTask_CensusRespectsTheScope (review B1): an open call the scope
// leaves out is counted, never named, and the list is not called complete while
// one exists. Control: without the scope the call is named.
func TestContextForTask_CensusRespectsTheScope(t *testing.T) {
	s := newModuleWorkspace(t, map[string]string{
		"go.mod":   censusModule,
		"p/s.go":   "package p\n\n// S is seeded.\ntype S struct{}\n\n// Seed is called from q through an interface value.\nfunc (s *S) Seed() {}\n",
		"q/use.go": "package q\n\n// Use calls Seed on a value no declaration types.\nfunc Use(x interface{ Seed() }) {\n\tx.Seed()\n}\n",
	})
	args := map[string]any{"symbols": []string{"p/s.go#S.Seed"}}
	whole, err := s.run(t, args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(whole, "1 unresolved call(s) of Seed may also be it: q/use.go:5 (receiver)") {
		t.Fatalf("control: the open call is not named without a scope:\n%s", whole)
	}
	args["within"] = []string{"p"}
	scoped, err := s.run(t, args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(scoped, "1 unresolved call(s) of Seed outside the scope are not listed, so the list is not complete") ||
		strings.Contains(scoped, "callers of (*S).Seed: complete") || strings.Contains(scoped, "q/use.go") {
		t.Errorf("an open call outside the scope must be counted, not named, and must stop \"complete\":\n%s", scoped)
	}
}

// TestContextForTask_CensusNeedsEveryGoFileRead (review S1): a Go file the index
// failed to read (here an unreadable one) hides its calls, so no list is called
// complete while one exists. Control: the same workspace without it says complete.
// A file with a syntax error is not such a file: the Go extractor records what it
// parses, and its calls with it. A file over the size cap is never recorded at all,
// which only the caveat (labelCensus: "in the files the index holds") covers.
func TestContextForTask_CensusNeedsEveryGoFileRead(t *testing.T) {
	files := map[string]string{
		"go.mod":     censusModule,
		"p/lone.go":  "package p\n\n// Lone is called once, from Caller.\nfunc Lone() int { return 1 }\n",
		"p/other.go": "package p\n\n// Caller calls Lone.\nfunc Caller() int { return Lone() }\n",
	}
	open := func(files map[string]string, unreadable string) shopTool {
		root := t.TempDir()
		writeFiles(t, root, files)
		if unreadable != "" {
			p := filepath.Join(root, filepath.FromSlash(unreadable))
			if err := os.Chmod(p, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(p, 0o644) })
			if _, err := os.ReadFile(p); err == nil {
				t.Skip("running as a user who can read a mode-0 file (root): the case cannot be built")
			}
		}
		return newShopTool(t, openContextStore(t, root, 2), root)
	}
	args := map[string]any{"symbols": []string{"p/lone.go#Lone"}}
	clean, err := open(files, "").run(t, args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(clean, "callers of Lone: complete (every recorded call of Lone resolves): 1 in the index") {
		t.Fatalf("control: a fully read workspace should be complete:\n%s", clean)
	}
	files["p/hidden.go"] = "package p\n\n// Hidden calls Lone, but the index cannot read it.\nfunc Hidden() int { return Lone() }\n"
	big, err := open(files, "p/hidden.go").run(t, args)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(big, "callers of Lone: complete") || !strings.Contains(big, "not claimed complete: 1 Go file(s) could not be read") {
		t.Errorf("an unread Go file must stop \"complete\":\n%s", big)
	}
}
