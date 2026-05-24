package treesitter

import (
	"context"
	"slices"
	"testing"

	"github.com/golimpio/plumb/internal/topology"
)

var markdownSrc = []byte("# Title\n\n" +
	"Intro paragraph with **bold** text.\n\n" +
	"## Section One\n\n" +
	"- item one\n- item two\n\n" +
	"### Subsection\n\n" +
	"```go\nfunc main() {}\n```\n\n" +
	"## Section Two\n\n" +
	"Some closing text.\n")

func TestMarkdown_HeadingsExtracted(t *testing.T) {
	nodes, _, err := NewMarkdown().Extract(context.Background(), "docs/guide.md", markdownSrc)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, want := range []string{"Title", "Section One", "Subsection", "Section Two"} {
		if !slices.Contains(names(nodes, topology.KindSection), want) {
			t.Errorf("section %q not found; got %v", want, names(nodes, topology.KindSection))
		}
	}
}

func TestMarkdown_HeadingNesting(t *testing.T) {
	nodes, edges, err := NewMarkdown().Extract(context.Background(), "guide.md", markdownSrc)
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
	// Title (h1) contains Section One (h2); Section One contains Subsection (h3).
	wantEdges := [][2]int64{
		{idxOf("Title"), idxOf("Section One")},
		{idxOf("Section One"), idxOf("Subsection")},
		{idxOf("Title"), idxOf("Section Two")},
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

func TestMarkdown_EmptyAndProseOnly(t *testing.T) {
	for _, src := range [][]byte{[]byte(""), []byte("just a paragraph, no headings\n")} {
		nodes, edges, err := NewMarkdown().Extract(context.Background(), "e.md", src)
		if err != nil {
			t.Fatalf("Extract: %v", err)
		}
		if len(nodes) != 0 || len(edges) != 0 {
			t.Errorf("src=%q: want 0 nodes/edges, got %d/%d", src, len(nodes), len(edges))
		}
	}
}

func TestMarkdown_LanguageAndPath(t *testing.T) {
	nodes, _, err := NewMarkdown().Extract(context.Background(), "docs/guide.md", markdownSrc)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.Language != "markdown" {
			t.Errorf("node %q language=%q, want markdown", n.Name, n.Language)
		}
		if n.Path != "docs/guide.md" {
			t.Errorf("node %q path=%q, want docs/guide.md", n.Name, n.Path)
		}
	}
}

func TestMarkdown_Extensions(t *testing.T) {
	exts := NewMarkdown().Extensions()
	for _, want := range []string{".md", ".markdown"} {
		if !slices.Contains(exts, want) {
			t.Errorf("%s missing from Markdown Extensions()", want)
		}
	}
}
