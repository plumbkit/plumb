package treesitter

import (
	"context"
	"slices"
	"testing"

	"github.com/golimpio/plumb/internal/topology"
)

var bashSrc = []byte(`#!/usr/bin/env bash
set -euo pipefail

source ./lib/common.sh

readonly CONFIG_DIR="/etc/app"
VERSION="1.0.0"

log() {
    echo "$*" >&2
}

deploy() {
    local target="$1"
    log "deploying ${target}"
    cleanup
}

function cleanup {
    rm -rf /tmp/build
}

main() {
    deploy "/opt/app"
}

main "$@"
`)

func TestBash_KindsExtracted(t *testing.T) {
	nodes, _, err := NewBash().Extract(context.Background(), "scripts/deploy.sh", bashSrc)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	cases := []struct {
		kind topology.NodeKind
		name string
	}{
		{topology.KindImport, "./lib/common.sh"},
		{topology.KindConstant, "CONFIG_DIR"},
		{topology.KindVariable, "VERSION"},
		{topology.KindFunction, "log"},
		{topology.KindFunction, "deploy"},
		{topology.KindFunction, "cleanup"},
		{topology.KindFunction, "main"},
	}
	for _, c := range cases {
		if !slices.Contains(names(nodes, c.kind), c.name) {
			t.Errorf("kind=%s name=%q not found; got %v", c.kind, c.name, names(nodes, c.kind))
		}
	}
}

func TestBash_ConstVsVar(t *testing.T) {
	nodes, _, err := NewBash().Extract(context.Background(), "deploy.sh", bashSrc)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(names(nodes, topology.KindVariable), "CONFIG_DIR") {
		t.Error("CONFIG_DIR is readonly → should be a constant, not a variable")
	}
	if slices.Contains(names(nodes, topology.KindConstant), "VERSION") {
		t.Error("VERSION is a plain assignment → should be a variable, not a constant")
	}
	// A function-local `local target=…` must not surface as a module-level var.
	if slices.Contains(names(nodes, topology.KindVariable), "target") {
		t.Error("function-local 'target' should not be extracted as a top-level variable")
	}
}

func TestBash_CallEdgeIntraFile(t *testing.T) {
	nodes, edges, err := NewBash().Extract(context.Background(), "deploy.sh", bashSrc)
	if err != nil {
		t.Fatal(err)
	}
	var deployIdx, logIdx int64 = -1, -1
	for i, n := range nodes {
		switch n.Name {
		case "deploy":
			deployIdx = int64(i)
		case "log":
			logIdx = int64(i)
		}
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeCalls && e.FromID == deployIdx && e.ToID == logIdx {
			if e.Confidence != 0.8 || e.Source != "heuristic" {
				t.Errorf("call edge conf=%v src=%q, want 0.8/heuristic", e.Confidence, e.Source)
			}
			return
		}
	}
	t.Errorf("no call edge deploy→log; edges=%v", edges)
}

func TestBash_EndLineRecorded(t *testing.T) {
	nodes, _, err := NewBash().Extract(context.Background(), "deploy.sh", bashSrc)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.Kind == topology.KindFunction && n.Name == "deploy" {
			if n.EndLine <= n.StartLine {
				t.Errorf("deploy EndLine=%d should exceed StartLine=%d", n.EndLine, n.StartLine)
			}
			return
		}
	}
	t.Fatal("deploy function node not found")
}

func TestBash_EmptyAndCommentOnly(t *testing.T) {
	for _, src := range [][]byte{[]byte(""), []byte("# just a comment\n# more\n")} {
		nodes, edges, err := NewBash().Extract(context.Background(), "e.sh", src)
		if err != nil {
			t.Fatalf("Extract: %v", err)
		}
		if len(nodes) != 0 || len(edges) != 0 {
			t.Errorf("src=%q: want 0 nodes/edges, got %d/%d", src, len(nodes), len(edges))
		}
	}
}

func TestBash_LanguageAndPath(t *testing.T) {
	nodes, _, err := NewBash().Extract(context.Background(), "scripts/deploy.sh", bashSrc)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.Language != "bash" {
			t.Errorf("node %q language=%q, want bash", n.Name, n.Language)
		}
		if n.Path != "scripts/deploy.sh" {
			t.Errorf("node %q path=%q, want scripts/deploy.sh", n.Name, n.Path)
		}
	}
}

func TestBash_Extensions(t *testing.T) {
	exts := NewBash().Extensions()
	for _, want := range []string{".sh", ".bash"} {
		if !slices.Contains(exts, want) {
			t.Errorf("%s missing from Bash Extensions()", want)
		}
	}
}
