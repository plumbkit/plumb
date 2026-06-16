package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
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

func TestTopologyRoutes_PatternsVapor(t *testing.T) {
	patterns := routePatterns("vapor")
	if len(patterns) == 0 {
		t.Fatal("vapor filter returned no patterns")
	}
	for _, p := range patterns {
		if !strings.Contains(p.name, "vapor.") {
			t.Errorf("vapor filter returned non-vapor pattern: %s", p.name)
		}
	}
}

func TestTopologyRoutes_PatternsArgumentParser(t *testing.T) {
	patterns := routePatterns("argument-parser")
	if len(patterns) == 0 {
		t.Fatal("argument-parser filter returned no patterns")
	}
	for _, p := range patterns {
		if !strings.Contains(p.name, "argument-parser.") {
			t.Errorf("argument-parser filter returned non-argument-parser pattern: %s", p.name)
		}
	}
}

func TestTopologyRoutes_SwiftRoutesCandidateBySignature(t *testing.T) {
	// Verify that a node with "RoutesBuilder" in its signature is matched by the
	// vapor.RouteCollection pattern — the key enabler for Swift/Vapor route detection.
	node := topology.Node{
		Kind:      topology.KindMethod,
		Name:      "boot",
		Path:      "Sources/Routes.swift",
		StartLine: 3,
		Signature: "func boot (routes: RoutesBuilder) throws",
		Language:  "swift",
	}
	for _, p := range routePatterns("vapor") {
		if p.query == "RoutesBuilder" && isRouteCandidate(node, p, "") {
			return
		}
	}
	t.Error("boot(routes: RoutesBuilder) not matched by vapor.RouteCollection pattern")
}

func TestTopologyRoutes_VaporConfigureCandidate(t *testing.T) {
	var pat routePattern
	for _, p := range routePatterns("vapor") {
		if p.name == "vapor.configure" {
			pat = p
			break
		}
	}
	if pat.name == "" {
		t.Fatal("vapor.configure pattern not found")
	}
	if pat.query != ": Application" {
		t.Fatalf("vapor.configure query = %q, want %q", pat.query, ": Application")
	}

	configure := topology.Node{
		Kind:      topology.KindMethod,
		Name:      "configure",
		Path:      "Sources/App.swift",
		StartLine: 3,
		Signature: "func configure(_ app: Application) throws",
		Language:  "swift",
	}
	if !isRouteCandidate(configure, pat, "") {
		t.Error("configure(_ app: Application) not matched by vapor.configure pattern")
	}

	for _, tc := range []topology.Node{
		{
			Kind:      topology.KindMethod,
			Name:      "UIApplication",
			Signature: "func UIApplication()",
			Language:  "swift",
		},
		{
			Kind:      topology.KindMethod,
			Name:      "ApplicationDelegate",
			Signature: "func ApplicationDelegate()",
			Language:  "swift",
		},
		{
			Kind:      topology.KindMethod,
			Name:      "applicationDidFinishLaunching",
			Signature: "func applicationDidFinishLaunching(_ notification: Notification)",
			Language:  "swift",
		},
	} {
		if isRouteCandidate(tc, pat, "") {
			t.Errorf("%s must not match vapor.configure pattern", tc.Name)
		}
	}
}

func TestTopologyRoutes_ArgumentParserRunCandidate(t *testing.T) {
	// The Swift extractor propagates the enclosing type's ParsableCommand
	// conformance onto its methods' signatures. The argument-parser.run pattern
	// must match the run() entry point — and ONLY run(), not sibling methods of
	// the same command type (the nameEquals guard).
	var pat routePattern
	for _, p := range routePatterns("argument-parser") {
		if p.name == "argument-parser.run" {
			pat = p
		}
	}
	if pat.name == "" {
		t.Fatal("argument-parser.run pattern not found")
	}

	run := topology.Node{
		Kind:      topology.KindMethod,
		Name:      "run",
		Path:      "Sources/Hello.swift",
		Signature: "func run () throws : ParsableCommand",
		Language:  "swift",
	}
	if !isRouteCandidate(run, pat, "") {
		t.Error("run() of a ParsableCommand type not matched by argument-parser.run pattern")
	}

	// A sibling method of the same command type carries the same conformance in
	// its signature but must NOT be flagged — only run() is the entry point.
	validate := run
	validate.Name = "validate"
	if isRouteCandidate(validate, pat, "") {
		t.Error("validate() must not match argument-parser.run (nameEquals guard failed)")
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
