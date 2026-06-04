package treesitter

import (
	"context"
	"slices"
	"testing"

	"github.com/golimpio/plumb/internal/topology"
)

var htmlSrc = []byte(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <link rel="stylesheet" href="/css/app.css">
  <script src="/js/app.js"></script>
  <style>.muted { color: gray; }</style>
</head>
<body>
  <h1>Welcome</h1>
  <section id="intro">
    <h2>Getting Started</h2>
    <p>Some <strong>bold</strong> intro text.</p>
    <my-widget id="w1" value="42"></my-widget>
  </section>
  <nav id="main-nav">
    <a href="#intro">Intro</a>
  </nav>
  <user-card></user-card>
  <img id="logo" src="/img/logo.png">
</body>
</html>
`)

func TestHTML_KindsExtracted(t *testing.T) {
	nodes, _, err := NewHTML().Extract(context.Background(), "site/index.html", htmlSrc)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	cases := []struct {
		kind topology.NodeKind
		name string
	}{
		{topology.KindSection, "Welcome"},         // h1
		{topology.KindSection, "Getting Started"}, // h2 nested in <section>
		{topology.KindConstant, "intro"},          // <section id>
		{topology.KindConstant, "main-nav"},       // <nav id>
		{topology.KindConstant, "w1"},             // <my-widget id> — id wins over custom-element
		{topology.KindConstant, "logo"},           // void <img id>
		{topology.KindImport, "/js/app.js"},       // <script src>
		{topology.KindImport, "/css/app.css"},     // <link href>
		{topology.KindClass, "user-card"},         // custom element without id
	}
	for _, c := range cases {
		if !slices.Contains(names(nodes, c.kind), c.name) {
			t.Errorf("kind=%s name=%q not found; got %v", c.kind, c.name, names(nodes, c.kind))
		}
	}
}

func TestHTML_HeadingNameCollapsesInlineMarkup(t *testing.T) {
	src := []byte("<h1>Hello <em>there</em>  world</h1>\n")
	nodes, _, err := NewHTML().Extract(context.Background(), "x.html", src)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(names(nodes, topology.KindSection), "Hello there world") {
		t.Errorf("heading text not collapsed; got %v", names(nodes, topology.KindSection))
	}
}

func TestHTML_DOMContainment(t *testing.T) {
	nodes, edges, err := NewHTML().Extract(context.Background(), "index.html", htmlSrc)
	if err != nil {
		t.Fatal(err)
	}
	idxOf := func(name string) int64 {
		for i, n := range nodes {
			if n.Name == name {
				return int64(i)
			}
		}
		return -1
	}
	// <section id="intro"> contains both the nested <h2> and <my-widget id="w1">.
	wantEdges := [][2]int64{
		{idxOf("intro"), idxOf("Getting Started")},
		{idxOf("intro"), idxOf("w1")},
	}
	for _, we := range wantEdges {
		found := false
		for _, e := range edges {
			if e.Kind == topology.EdgeContains && e.FromID == we[0] && e.ToID == we[1] {
				if e.Confidence != 1.0 || e.Source != "extractor" {
					t.Errorf("edge %v conf=%v src=%q, want 1.0/extractor", we, e.Confidence, e.Source)
				}
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing contains edge %v; edges=%v", we, edges)
		}
	}
}

func TestHTML_TopLevelHeadingHasNoParent(t *testing.T) {
	nodes, edges, err := NewHTML().Extract(context.Background(), "index.html", htmlSrc)
	if err != nil {
		t.Fatal(err)
	}
	var welcome int64 = -1
	for i, n := range nodes {
		if n.Name == "Welcome" {
			welcome = int64(i)
		}
	}
	if welcome < 0 {
		t.Fatal("Welcome heading not extracted")
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeContains && e.ToID == welcome {
			t.Errorf("top-level <h1> should have no containment parent; got edge %v", e)
		}
	}
}

func TestHTML_EmptyAndTextOnly(t *testing.T) {
	for _, src := range [][]byte{[]byte(""), []byte("<p>just a paragraph, no landmarks</p>\n")} {
		nodes, edges, err := NewHTML().Extract(context.Background(), "e.html", src)
		if err != nil {
			t.Fatalf("Extract: %v", err)
		}
		if len(nodes) != 0 || len(edges) != 0 {
			t.Errorf("src=%q: want 0 nodes/edges, got %d/%d", src, len(nodes), len(edges))
		}
	}
}

func TestHTML_LanguageAndPath(t *testing.T) {
	nodes, _, err := NewHTML().Extract(context.Background(), "site/index.html", htmlSrc)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.Language != "html" {
			t.Errorf("node %q language=%q, want html", n.Name, n.Language)
		}
		if n.Path != "site/index.html" {
			t.Errorf("node %q path=%q, want site/index.html", n.Name, n.Path)
		}
	}
}

func TestHTML_Extensions(t *testing.T) {
	exts := NewHTML().Extensions()
	for _, want := range []string{".html", ".htm"} {
		if !slices.Contains(exts, want) {
			t.Errorf("%s missing from HTML Extensions()", want)
		}
	}
}
