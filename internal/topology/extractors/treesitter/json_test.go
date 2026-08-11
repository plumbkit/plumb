package treesitter

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

// jsonSrc is a package.json-shaped fixture: the document an agent is most
// likely to ask about, and the one whose nested keys (scripts.build,
// dependencies.*) are the actual search targets.
var jsonSrc = []byte(`{
  "name": "plumb-site",
  "version": "1.2.0",
  "private": true,
  "scripts": {
    "build": "vite build",
    "test": "vitest run"
  },
  "dependencies": {
    "svelte": "^5.0.0"
  },
  "files": ["dist", "README.md"],
  "contributors": [
    { "name": "Ada", "email": "ada@example.com" },
    { "name": "Grace", "email": "grace@example.com" }
  ]
}
`)

func jsonExtract(t *testing.T) ([]topology.Node, []topology.Edge) {
	t.Helper()
	nodes, edges, err := NewJSON().Extract(context.Background(), "package.json", jsonSrc)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return nodes, edges
}

func jsonQualified(nodes []topology.Node) map[string]topology.Node {
	out := make(map[string]topology.Node, len(nodes))
	for _, n := range nodes {
		out[n.Qualified] = n
	}
	return out
}

func TestJSON_KeysExtractedWithDottedPaths(t *testing.T) {
	nodes, _ := jsonExtract(t)
	byQ := jsonQualified(nodes)
	for _, want := range []string{
		"name", "version", "private",
		"scripts", "scripts.build", "scripts.test",
		"dependencies", "dependencies.svelte",
		"files", "contributors",
	} {
		if _, ok := byQ[want]; !ok {
			t.Errorf("missing key %q; got %d nodes", want, len(nodes))
		}
	}
	// The dotted path is the whole point: a search for a nested key has to
	// find it without the caller knowing its depth.
	if got := byQ["scripts.build"].Name; got != "build" {
		t.Errorf("scripts.build Name = %q, want \"build\"", got)
	}
}

func TestJSON_NestingBecomesContainment(t *testing.T) {
	nodes, edges := jsonExtract(t)
	idx := map[string]int64{}
	for i, n := range nodes {
		idx[n.Qualified] = int64(i)
	}
	for _, tc := range [][2]string{
		{"scripts", "scripts.build"},
		{"scripts", "scripts.test"},
		{"dependencies", "dependencies.svelte"},
	} {
		parent, child := idx[tc[0]], idx[tc[1]]
		found := false
		for _, e := range edges {
			if e.Kind == topology.EdgeContains && e.FromID == parent && e.ToID == child {
				if e.Confidence != 1.0 || e.Source != "extractor" {
					t.Errorf("%s -> %s edge = %v/%q, want 1.0/extractor", tc[0], tc[1], e.Confidence, e.Source)
				}
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no containment edge from %q to %q", tc[0], tc[1])
		}
	}
}

// An array is transparent: it never becomes a node of its own, and the keys of
// its object elements attach to whatever key held the array. Without that, a
// list of records would bury its keys under an unnameable level.
func TestJSON_ArraysAreTransparent(t *testing.T) {
	nodes, _ := jsonExtract(t)
	byQ := jsonQualified(nodes)
	if _, ok := byQ["contributors.name"]; !ok {
		t.Errorf("an object inside an array should attach its keys to the array's key; got %v", qualifiedList(nodes))
	}
	// A scalar array contributes only its own key, never elements.
	for q := range byQ {
		if strings.HasPrefix(q, "files.") {
			t.Errorf("a scalar array element became a node (%q); elements are unnameable in JSON", q)
		}
	}
}

// The cap exists so one large data file cannot blow out the node count. It must
// bound the walk without dropping the array's own key.
func TestJSON_LargeArrayIsCapped(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{"records": [`)
	for i := range jsonMaxArrayElements * 3 {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"k%d": %d}`, i, i)
	}
	sb.WriteString("]}")

	nodes, _, err := NewJSON().Extract(context.Background(), "big.json", []byte(sb.String()))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	byQ := jsonQualified(nodes)
	if _, ok := byQ["records"]; !ok {
		t.Error("the array's own key must survive the cap")
	}
	// One node for `records` plus at most one per walked element.
	if len(nodes) > jsonMaxArrayElements+1 {
		t.Errorf("got %d nodes for %d array elements; the cap (%d) is not bounding the walk",
			len(nodes), jsonMaxArrayElements*3, jsonMaxArrayElements)
	}
}

func TestJSON_ByteSpanReconstructsTheKey(t *testing.T) {
	nodes, _ := jsonExtract(t)
	n, ok := jsonQualified(nodes)["scripts.build"]
	if !ok {
		t.Fatal("scripts.build missing")
	}
	if !n.HasBytes {
		t.Fatal("HasBytes false; every emitted node must carry its span")
	}
	if got := string(jsonSrc[n.StartByte:n.EndByte]); !strings.Contains(got, "build") {
		t.Errorf("span covers %q, which does not contain the key", got)
	}
}

func TestJSON_EmptyAndScalarDocuments(t *testing.T) {
	for _, src := range []string{"", "{}", "[]", "null", "42", `"a string"`} {
		nodes, edges, err := NewJSON().Extract(context.Background(), "a.json", []byte(src))
		if err != nil {
			t.Errorf("Extract(%q): %v", src, err)
		}
		if len(nodes) != 0 || len(edges) != 0 {
			t.Errorf("Extract(%q) = %d nodes, %d edges; want none", src, len(nodes), len(edges))
		}
	}
}

// JSONC is JSON with comments; the extension is claimed, so a commented
// document must still yield its keys rather than failing to parse.
func TestJSON_HandlesComments(t *testing.T) {
	src := []byte("{\n  // the build entry point\n  \"main\": \"index.js\"\n}\n")
	nodes, _, err := NewJSON().Extract(context.Background(), "tsconfig.jsonc", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if _, ok := jsonQualified(nodes)["main"]; !ok {
		t.Errorf("a commented document lost its keys; got %v", qualifiedList(nodes))
	}
}

func TestJSON_LanguageAndPath(t *testing.T) {
	nodes, _ := jsonExtract(t)
	if len(nodes) == 0 {
		t.Fatal("fixture produced no nodes; the loop below would be vacuous")
	}
	for _, n := range nodes {
		if n.Language != "json" {
			t.Errorf("node %q language = %q, want json", n.Name, n.Language)
		}
		if n.Path != "package.json" {
			t.Errorf("node %q path = %q, want the path passed to Extract", n.Name, n.Path)
		}
	}
}

func TestJSON_Extensions(t *testing.T) {
	got := NewJSON().Extensions()
	for _, want := range []string{".json", ".jsonc"} {
		if !slicesContains(got, want) {
			t.Errorf("Extensions() = %v, missing %q", got, want)
		}
	}
}

func qualifiedList(nodes []topology.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Qualified)
	}
	return out
}
