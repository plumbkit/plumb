package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/golimpio/plumb/internal/topology"
)

func TestTopologyRoutes_NilStore(t *testing.T) {
	tool := NewTopologyRoutes(func() *topology.Store { return nil })
	out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "disabled") {
		t.Errorf("expected 'disabled' message, got: %s", out)
	}
}

func TestTopologyRoutes_Defaults(t *testing.T) {
	a, err := parseTopologyRoutesArgs(json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if a.Limit != 20 {
		t.Errorf("limit default=%d, want 20", a.Limit)
	}
}

func TestTopologyRoutes_FormatEmpty(t *testing.T) {
	a := topologyRoutesArgs{Limit: 20}
	out := formatRoutesResult([]routeEntry{}, a)
	if !strings.Contains(out, "no route patterns matched") {
		t.Errorf("empty result message wrong: %s", out)
	}
}

func TestTopologyRoutes_FormatNilResult(t *testing.T) {
	a := topologyRoutesArgs{Limit: 20}
	out := formatRoutesResult(nil, a)
	if strings.Contains(out, "disabled") {
		t.Errorf("nil routes is a no-results case, not 'disabled'; got: %s", out)
	}
	if !strings.Contains(out, "no route patterns matched") {
		t.Errorf("expected no-results message, got: %s", out)
	}
}

func TestTopologyRoutes_PatternsAllFrameworks(t *testing.T) {
	patterns := routePatterns("")
	if len(patterns) == 0 {
		t.Error("expected patterns for empty framework")
	}
}

func TestTopologyRoutes_PatternsCobra(t *testing.T) {
	patterns := routePatterns("cobra")
	for _, p := range patterns {
		if !strings.Contains(p.name, "cobra") {
			t.Errorf("cobra filter returned non-cobra pattern: %s", p.name)
		}
	}
}

func TestTopologyRoutes_FormatWithEntry(t *testing.T) {
	routes := []routeEntry{
		{
			Node: topology.Node{
				Kind:      topology.KindFunction,
				Name:      "handleUsers",
				Path:      "internal/api/users.go",
				StartLine: 42,
				Signature: "func handleUsers(w http.ResponseWriter, r *http.Request)",
			},
			Pattern:    "http.HandleFunc",
			Confidence: 0.7,
		},
	}
	a := topologyRoutesArgs{Limit: 20}
	out := formatRoutesResult(routes, a)
	if !strings.Contains(out, "handleUsers") {
		t.Errorf("expected handleUsers in output, got: %s", out)
	}
	if !strings.Contains(out, "conf=0.70") {
		t.Errorf("expected confidence annotation, got: %s", out)
	}
}
