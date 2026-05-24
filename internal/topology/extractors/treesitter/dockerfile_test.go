package treesitter

import (
	"context"
	"slices"
	"testing"

	"github.com/golimpio/plumb/internal/topology"
)

var dockerfileSrc = []byte(`FROM golang:1.22 AS builder
ARG VERSION=dev
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /app ./cmd/server

FROM alpine:3.19
ENV PORT=8080
ENV LOG_LEVEL=info
COPY --from=builder /app /app
EXPOSE 8080
ENTRYPOINT ["/app"]
`)

func TestDockerfile_KindsExtracted(t *testing.T) {
	nodes, _, err := NewDockerfile().Extract(context.Background(), "Dockerfile", dockerfileSrc)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	cases := []struct {
		kind topology.NodeKind
		name string
	}{
		{topology.KindType, "builder"}, // FROM … AS builder
		{topology.KindType, "alpine"},  // FROM alpine:3.19 (no alias → image name)
		{topology.KindVariable, "VERSION"},
		{topology.KindVariable, "PORT"},
		{topology.KindVariable, "LOG_LEVEL"},
	}
	for _, c := range cases {
		if !slices.Contains(names(nodes, c.kind), c.name) {
			t.Errorf("kind=%s name=%q not found; got %v", c.kind, c.name, names(nodes, c.kind))
		}
	}
}

func TestDockerfile_StageContainsEnv(t *testing.T) {
	nodes, edges, err := NewDockerfile().Extract(context.Background(), "Dockerfile", dockerfileSrc)
	if err != nil {
		t.Fatal(err)
	}
	var alpineIdx, portIdx int64 = -1, -1
	for i, n := range nodes {
		switch {
		case n.Kind == topology.KindType && n.Name == "alpine":
			alpineIdx = int64(i)
		case n.Kind == topology.KindVariable && n.Name == "PORT":
			portIdx = int64(i)
		}
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeContains && e.FromID == alpineIdx && e.ToID == portIdx {
			if e.Confidence != 1.0 || e.Source != "extractor" {
				t.Errorf("contains edge conf=%v src=%q, want 1.0/extractor", e.Confidence, e.Source)
			}
			return
		}
	}
	t.Errorf("no contains edge alpine→PORT; edges=%v", edges)
}

func TestDockerfile_ArgAttachedToFirstStage(t *testing.T) {
	nodes, edges, err := NewDockerfile().Extract(context.Background(), "Dockerfile", dockerfileSrc)
	if err != nil {
		t.Fatal(err)
	}
	var builderIdx, versionIdx int64 = -1, -1
	for i, n := range nodes {
		switch {
		case n.Kind == topology.KindType && n.Name == "builder":
			builderIdx = int64(i)
		case n.Kind == topology.KindVariable && n.Name == "VERSION":
			versionIdx = int64(i)
		}
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeContains && e.FromID == builderIdx && e.ToID == versionIdx {
			return
		}
	}
	t.Errorf("no contains edge builder→VERSION; edges=%v", edges)
}

func TestDockerfile_EmptyAndCommentOnly(t *testing.T) {
	for _, src := range [][]byte{[]byte(""), []byte("# just a comment\n# more\n")} {
		nodes, edges, err := NewDockerfile().Extract(context.Background(), "Dockerfile", src)
		if err != nil {
			t.Fatalf("Extract: %v", err)
		}
		if len(nodes) != 0 || len(edges) != 0 {
			t.Errorf("src=%q: want 0 nodes/edges, got %d/%d", src, len(nodes), len(edges))
		}
	}
}

func TestDockerfile_LanguageAndPath(t *testing.T) {
	nodes, _, err := NewDockerfile().Extract(context.Background(), "build/Dockerfile.prod", dockerfileSrc)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.Language != "dockerfile" {
			t.Errorf("node %q language=%q, want dockerfile", n.Name, n.Language)
		}
		if n.Path != "build/Dockerfile.prod" {
			t.Errorf("node %q path=%q, want build/Dockerfile.prod", n.Name, n.Path)
		}
	}
}

func TestDockerfile_Extensions(t *testing.T) {
	exts := NewDockerfile().Extensions()
	for _, want := range []string{"dockerfile", "containerfile"} {
		if !slices.Contains(exts, want) {
			t.Errorf("%s missing from Dockerfile Extensions()", want)
		}
	}
}
