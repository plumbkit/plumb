package treesitter

import (
	"context"
	"slices"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

var yamlSrc = []byte(`version: "3.8"

services:
  web:
    image: nginx:latest
    ports:
      - "80:80"
  db:
    image: postgres:16
    environment:
      POSTGRES_PASSWORD: secret
`)

func TestYAML_KindsExtracted(t *testing.T) {
	nodes, _, err := NewYAML().Extract(context.Background(), "docker-compose.yaml", yamlSrc)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, want := range []string{"version", "services", "web", "db", "image", "ports", "environment"} {
		if !slices.Contains(names(nodes, topology.KindField), want) {
			t.Errorf("field %q not found; got %v", want, names(nodes, topology.KindField))
		}
	}
}

func TestYAML_NestingContainment(t *testing.T) {
	nodes, edges, err := NewYAML().Extract(context.Background(), "docker-compose.yaml", yamlSrc)
	if err != nil {
		t.Fatal(err)
	}
	// services → web (the first `web` field, nested under services).
	var servicesIdx, webIdx int64 = -1, -1
	for i, n := range nodes {
		switch {
		case n.Name == "services":
			servicesIdx = int64(i)
		case n.Name == "web" && webIdx == -1:
			webIdx = int64(i)
		}
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeContains && e.FromID == servicesIdx && e.ToID == webIdx {
			if e.Confidence != 1.0 || e.Source != "extractor" {
				t.Errorf("contains edge conf=%v src=%q, want 1.0/extractor", e.Confidence, e.Source)
			}
			return
		}
	}
	t.Errorf("no contains edge services→web; edges=%v", edges)
}

func TestYAML_TopLevelKeyHasNoParent(t *testing.T) {
	nodes, edges, err := NewYAML().Extract(context.Background(), "docker-compose.yaml", yamlSrc)
	if err != nil {
		t.Fatal(err)
	}
	var versionIdx int64 = -1
	for i, n := range nodes {
		if n.Name == "version" {
			versionIdx = int64(i)
		}
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeContains && e.ToID == versionIdx {
			t.Errorf("top-level key 'version' should have no containment parent; got edge %+v", e)
		}
	}
}

func TestYAML_EmptyAndCommentOnly(t *testing.T) {
	for _, src := range [][]byte{[]byte(""), []byte("# just a comment\n# more\n")} {
		nodes, edges, err := NewYAML().Extract(context.Background(), "e.yaml", src)
		if err != nil {
			t.Fatalf("Extract: %v", err)
		}
		if len(nodes) != 0 || len(edges) != 0 {
			t.Errorf("src=%q: want 0 nodes/edges, got %d/%d", src, len(nodes), len(edges))
		}
	}
}

func TestYAML_LanguageAndPath(t *testing.T) {
	nodes, _, err := NewYAML().Extract(context.Background(), "deploy/compose.yml", yamlSrc)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.Language != "yaml" {
			t.Errorf("node %q language=%q, want yaml", n.Name, n.Language)
		}
		if n.Path != "deploy/compose.yml" {
			t.Errorf("node %q path=%q, want deploy/compose.yml", n.Name, n.Path)
		}
	}
}

func TestYAML_Extensions(t *testing.T) {
	exts := NewYAML().Extensions()
	for _, want := range []string{".yaml", ".yml"} {
		if !slices.Contains(exts, want) {
			t.Errorf("%s missing from YAML Extensions()", want)
		}
	}
}

func TestYAML_QualifiedDottedPath(t *testing.T) {
	src := []byte("services:\n  web:\n    image: nginx\n")
	nodes, _, err := NewYAML().Extract(context.Background(), "c.yaml", src)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range nodes {
		if n.Name == "image" {
			found = true
			if n.Qualified != "services.web.image" {
				t.Errorf("image Qualified=%q, want services.web.image", n.Qualified)
			}
		}
	}
	if !found {
		t.Fatal("image key not found")
	}
}
