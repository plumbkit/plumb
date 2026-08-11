package treesitter

import (
	"context"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

var cssSrc = []byte(`@import "base.css";

/* Brand tokens. */
:root {
  --brand: #09f;
  --gap: 8px;
}

.btn, .btn-primary {
  color: red;
  padding: var(--gap);
}

#header > .nav a:hover {
  text-decoration: underline;
}

@media (min-width: 600px) {
  .btn {
    padding: 12px;
  }
}

@supports (display: grid) {
  .grid { display: grid; }
}

@keyframes fade {
  from { opacity: 0; }
  to { opacity: 1; }
}
`)

func cssExtract(t *testing.T) ([]topology.Node, []topology.Edge) {
	t.Helper()
	nodes, edges, err := NewCSS().Extract(context.Background(), "site/app.css", cssSrc)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return nodes, edges
}

func cssNamed(nodes []topology.Node, name string) (topology.Node, bool) {
	for _, n := range nodes {
		if n.Name == name {
			return n, true
		}
	}
	return topology.Node{}, false
}

func TestCSS_LandmarksExtracted(t *testing.T) {
	nodes, _ := cssExtract(t)
	for _, want := range []string{
		"base.css",               // @import
		":root",                  // rule set
		".btn, .btn-primary",     // grouped selector, one node
		"#header > .nav a:hover", // combinator + pseudo-class
		"@media (min-width: 600px)",
		"@supports (display: grid)",
		"fade",    // @keyframes
		"--brand", // custom property
		"--gap",
	} {
		if _, ok := cssNamed(nodes, want); !ok {
			t.Errorf("missing landmark %q; got %v", want, nodeNames(nodes))
		}
	}
}

// The explosion guard. A stylesheet has far more declarations than selectors,
// and they repeat the same handful of property names — emitting them would bury
// the selectors that carry the structure. Only custom properties are exempt.
func TestCSS_OrdinaryDeclarationsAreNotEmitted(t *testing.T) {
	nodes, _ := cssExtract(t)
	for _, unwanted := range []string{"color", "padding", "text-decoration", "display", "opacity"} {
		if _, found := cssNamed(nodes, unwanted); found {
			t.Errorf("ordinary declaration %q was emitted; only selectors, sections, keyframes and custom properties should be", unwanted)
		}
	}
	// Sanity: the fixture has 9 landmarks, not dozens of declaration nodes.
	if len(nodes) > 12 {
		t.Errorf("got %d nodes for a 9-landmark stylesheet: %v", len(nodes), nodeNames(nodes))
	}
}

// A rule that only applies under a query must be reachable through that query,
// not floating at the top level indistinguishable from an unconditional rule.
func TestCSS_MediaBlockContainsItsRules(t *testing.T) {
	nodes, edges := cssExtract(t)
	var media, btn int64 = -1, -1
	for i, n := range nodes {
		switch {
		case n.Name == "@media (min-width: 600px)":
			media = int64(i)
		case n.Name == ".btn" && n.Kind == topology.KindType:
			btn = int64(i)
		}
	}
	if media < 0 || btn < 0 {
		t.Fatalf("fixture nodes missing: media=%d btn=%d (%v)", media, btn, nodeNames(nodes))
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeContains && e.FromID == media && e.ToID == btn {
			if e.Confidence != 1.0 || e.Source != "extractor" {
				t.Errorf("containment edge = %v/%q, want 1.0/extractor", e.Confidence, e.Source)
			}
			// The nested rule's qualified name carries its condition.
			if got := nodes[btn].Qualified; !strings.Contains(got, "@media") {
				t.Errorf("nested rule Qualified = %q, want it to carry the enclosing query", got)
			}
			return
		}
	}
	t.Error("no containment edge from the media query to the rule nested inside it")
}

func TestCSS_CustomPropertiesBelongToTheirRule(t *testing.T) {
	nodes, edges := cssExtract(t)
	var root, brand int64 = -1, -1
	for i, n := range nodes {
		switch n.Name {
		case ":root":
			root = int64(i)
		case "--brand":
			brand = int64(i)
		}
	}
	if root < 0 || brand < 0 {
		t.Fatalf("fixture nodes missing: root=%d brand=%d", root, brand)
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeContains && e.FromID == root && e.ToID == brand {
			return
		}
	}
	t.Error(":root must contain the custom property declared in it")
}

// A grouped selector is one rule with one body. Splitting it would emit several
// nodes sharing an identical span, which reads as duplicate symbols downstream.
func TestCSS_GroupedSelectorIsOneNode(t *testing.T) {
	nodes, _ := cssExtract(t)
	count := 0
	for _, n := range nodes {
		if strings.Contains(n.Name, ".btn-primary") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("got %d nodes mentioning .btn-primary, want exactly 1", count)
	}
}

func TestCSS_ByteSpanReconstructsTheRule(t *testing.T) {
	nodes, _ := cssExtract(t)
	n, ok := cssNamed(nodes, ".btn, .btn-primary")
	if !ok {
		t.Fatal("rule missing")
	}
	if !n.HasBytes {
		t.Fatal("HasBytes false; every emitted node must carry its span")
	}
	got := string(cssSrc[n.StartByte:n.EndByte])
	if !strings.HasPrefix(got, ".btn") || !strings.HasSuffix(strings.TrimSpace(got), "}") {
		t.Errorf("span does not reconstruct the rule:\n%s", got)
	}
}

func TestCSS_DocSpanCoversPrecedingComment(t *testing.T) {
	nodes, _ := cssExtract(t)
	n, ok := cssNamed(nodes, ":root")
	if !ok {
		t.Fatal(":root missing")
	}
	if n.DocStartByte == 0 && n.DocEndByte == 0 {
		t.Fatal("expected a doc span for a rule with a comment directly above it")
	}
	if got := string(cssSrc[n.DocStartByte:n.DocEndByte]); !strings.Contains(got, "Brand tokens") {
		t.Errorf("doc span = %q, want the comment above the rule", got)
	}
}

func TestCSS_EmptyAndCommentOnly(t *testing.T) {
	for _, src := range []string{"", "/* nothing here */\n", "\n\n"} {
		nodes, edges, err := NewCSS().Extract(context.Background(), "a.css", []byte(src))
		if err != nil {
			t.Errorf("Extract(%q): %v", src, err)
		}
		if len(nodes) != 0 || len(edges) != 0 {
			t.Errorf("Extract(%q) = %d nodes, %d edges; want none", src, len(nodes), len(edges))
		}
	}
}

func TestCSS_LanguageAndPath(t *testing.T) {
	nodes, _ := cssExtract(t)
	if len(nodes) == 0 {
		t.Fatal("fixture produced no nodes; the loop below would be vacuous")
	}
	for _, n := range nodes {
		if n.Language != "css" {
			t.Errorf("node %q language = %q, want css", n.Name, n.Language)
		}
		if n.Path != "site/app.css" {
			t.Errorf("node %q path = %q, want the path passed to Extract", n.Name, n.Path)
		}
	}
}

func TestCSS_Extensions(t *testing.T) {
	if got := NewCSS().Extensions(); !slicesContains(got, ".css") {
		t.Errorf("Extensions() = %v, missing .css", got)
	}
}
