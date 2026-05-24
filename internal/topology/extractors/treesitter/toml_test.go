package treesitter

import (
	"context"
	"slices"
	"testing"

	"github.com/golimpio/plumb/internal/topology"
)

var tomlSrc = []byte(`title = "Config"

[server]
host = "0.0.0.0"
port = 8080

[server.tls]
enabled = true
cert = "/etc/cert.pem"

[[database.replica]]
host = "db1"

[[database.replica]]
host = "db2"
`)

func TestTOML_KindsExtracted(t *testing.T) {
	nodes, _, err := NewTOML().Extract(context.Background(), "config.toml", tomlSrc)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	cases := []struct {
		kind topology.NodeKind
		name string
	}{
		{topology.KindField, "title"}, // top-level key
		{topology.KindType, "server"},
		{topology.KindType, "server.tls"},
		{topology.KindType, "database.replica"},
		{topology.KindField, "host"},
		{topology.KindField, "port"},
		{topology.KindField, "enabled"},
	}
	for _, c := range cases {
		if !slices.Contains(names(nodes, c.kind), c.name) {
			t.Errorf("kind=%s name=%q not found; got %v", c.kind, c.name, names(nodes, c.kind))
		}
	}
}

func TestTOML_TableContainsField(t *testing.T) {
	nodes, edges, err := NewTOML().Extract(context.Background(), "config.toml", tomlSrc)
	if err != nil {
		t.Fatal(err)
	}
	var serverIdx, portIdx int64 = -1, -1
	for i, n := range nodes {
		switch {
		case n.Kind == topology.KindType && n.Name == "server":
			serverIdx = int64(i)
		case n.Kind == topology.KindField && n.Name == "port":
			portIdx = int64(i)
		}
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeContains && e.FromID == serverIdx && e.ToID == portIdx {
			if e.Confidence != 1.0 || e.Source != "extractor" {
				t.Errorf("contains edge conf=%v src=%q, want 1.0/extractor", e.Confidence, e.Source)
			}
			return
		}
	}
	t.Errorf("no contains edge server→port; edges=%v", edges)
}

func TestTOML_QualifiedFieldName(t *testing.T) {
	nodes, _, err := NewTOML().Extract(context.Background(), "config.toml", tomlSrc)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.Kind == topology.KindField && n.Name == "port" {
			if n.Qualified != "server.port" {
				t.Errorf("port Qualified=%q, want server.port", n.Qualified)
			}
			return
		}
	}
	t.Fatal("port field not found")
}

func TestTOML_EmptyAndCommentOnly(t *testing.T) {
	for _, src := range [][]byte{[]byte(""), []byte("# just a comment\n# more\n")} {
		nodes, edges, err := NewTOML().Extract(context.Background(), "e.toml", src)
		if err != nil {
			t.Fatalf("Extract: %v", err)
		}
		if len(nodes) != 0 || len(edges) != 0 {
			t.Errorf("src=%q: want 0 nodes/edges, got %d/%d", src, len(nodes), len(edges))
		}
	}
}

func TestTOML_LanguageAndPath(t *testing.T) {
	nodes, _, err := NewTOML().Extract(context.Background(), "cfg/config.toml", tomlSrc)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.Language != "toml" {
			t.Errorf("node %q language=%q, want toml", n.Name, n.Language)
		}
		if n.Path != "cfg/config.toml" {
			t.Errorf("node %q path=%q, want cfg/config.toml", n.Name, n.Path)
		}
	}
}

func TestTOML_Extensions(t *testing.T) {
	if !slices.Contains(NewTOML().Extensions(), ".toml") {
		t.Error(".toml missing from TOML Extensions()")
	}
}
