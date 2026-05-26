package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/golimpio/plumb/internal/topology"
)

var topologyRoutesSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "framework": {
      "type": "string",
      "description": "Optional framework hint: 'gin', 'chi', 'mux', 'echo', 'cobra', 'fastapi', 'flask'. Omit to scan all known patterns."
    },
    "path_prefix": {
      "type": "string",
      "description": "Optional path prefix filter for route handlers (e.g. '/api/')."
    },
    "limit": {
      "type": "integer",
      "description": "Maximum number of route entries to return. Default 20.",
      "default": 20
    }
  },
  "additionalProperties": false
}`)

// TopologyRoutes scans topology nodes to identify HTTP/CLI entry points.
//
// Concurrency: Execute is safe for concurrent use.
type TopologyRoutes struct {
	storeFn func() *topology.Store
}

// NewTopologyRoutes returns a new TopologyRoutes tool.
func NewTopologyRoutes(storeFn func() *topology.Store) *TopologyRoutes {
	return &TopologyRoutes{storeFn: storeFn}
}

func (*TopologyRoutes) Name() string                 { return "topology_routes" }
func (*TopologyRoutes) InputSchema() json.RawMessage { return topologyRoutesSchema }
func (*TopologyRoutes) Description() string {
	return "Scans topology nodes to identify HTTP handler and CLI entry-point functions. " +
		"Matches Go patterns (http.HandleFunc, r.GET/POST, mux.Handle, Cobra cmd.Run/RunE) and " +
		"Python patterns (@app.route, @router.get, FastAPI path decorators). " +
		"Results carry confidence annotations — these are pattern-matched, not type-resolved. " +
		"Returns a clear message when no routes match or topology is disabled."
}

// routeEntry is a matched route/entry-point candidate.
type routeEntry struct {
	Node       topology.Node
	Pattern    string // matched pattern name
	Confidence float64
}

type topologyRoutesArgs struct {
	Framework  string `json:"framework"`
	PathPrefix string `json:"path_prefix"`
	Limit      int    `json:"limit"`
}

func (t *TopologyRoutes) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	a, err := parseTopologyRoutesArgs(raw)
	if err != nil {
		return "", err
	}
	store := t.storeFn()
	if store == nil {
		return topologyDisabledMessage(), nil
	}
	routes, runErr := t.run(ctx, store, a)
	if runErr != nil {
		return "", runErr
	}
	return formatRoutesResult(routes, a), nil
}

func parseTopologyRoutesArgs(raw json.RawMessage) (topologyRoutesArgs, error) {
	var a topologyRoutesArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("topology_routes: invalid arguments: %w", err)
	}
	if a.Limit <= 0 {
		a.Limit = 20
	}
	return a, nil
}

func (t *TopologyRoutes) run(ctx context.Context, store *topology.Store, a topologyRoutesArgs) ([]routeEntry, error) {
	if store == nil {
		return nil, nil
	}
	patterns := routePatterns(a.Framework)
	seen := map[int64]bool{}
	var routes []routeEntry

	for _, p := range patterns {
		results, err := store.Search(ctx, p.query, topology.SearchOpts{
			Kinds: []string{"function", "method"},
			Limit: a.Limit * 2,
		})
		if err != nil {
			return nil, fmt.Errorf("topology_routes: search: %w", err)
		}
		for _, r := range results {
			if seen[r.Node.ID] {
				continue
			}
			if !isRouteCandidate(r.Node, p, a.PathPrefix) {
				continue
			}
			seen[r.Node.ID] = true
			routes = append(routes, routeEntry{
				Node:       r.Node,
				Pattern:    p.name,
				Confidence: p.confidence,
			})
			if len(routes) >= a.Limit {
				return routes, nil
			}
		}
	}
	return routes, nil
}

// routePattern is a single named pattern to search for.
type routePattern struct {
	query      string
	name       string
	confidence float64
}

// routePatterns returns the patterns relevant to the given framework hint.
// An empty framework returns all patterns.
func routePatterns(framework string) []routePattern {
	all := []routePattern{
		// Go HTTP patterns
		{query: "HandleFunc", name: "http.HandleFunc", confidence: 0.7},
		{query: "Handle", name: "mux.Handle", confidence: 0.7},
		{query: "GET", name: "r.GET", confidence: 0.65},
		{query: "POST", name: "r.POST", confidence: 0.65},
		{query: "PUT", name: "r.PUT", confidence: 0.65},
		{query: "DELETE", name: "r.DELETE", confidence: 0.65},
		{query: "RunE", name: "cobra.RunE", confidence: 0.75},
		{query: "Run", name: "cobra.Run", confidence: 0.65},
		// Python HTTP patterns
		{query: "route", name: "@app.route", confidence: 0.7},
		{query: "get", name: "@router.get", confidence: 0.65},
		{query: "post", name: "@router.post", confidence: 0.65},
	}
	if framework == "" {
		return all
	}
	fw := strings.ToLower(framework)
	var filtered []routePattern
	for _, p := range all {
		if matchesFramework(p.name, fw) {
			filtered = append(filtered, p)
		}
	}
	if len(filtered) == 0 {
		return all // unknown framework: return all
	}
	return filtered
}

func matchesFramework(patternName, framework string) bool {
	switch framework {
	case "cobra":
		return strings.Contains(patternName, "cobra")
	case "gin", "chi", "echo":
		return strings.Contains(patternName, "r.")
	case "mux":
		return strings.Contains(patternName, "mux") || strings.Contains(patternName, "HandleFunc")
	case "fastapi", "flask":
		return strings.Contains(patternName, "@")
	default:
		return true
	}
}

func isRouteCandidate(n topology.Node, p routePattern, pathPrefix string) bool {
	if pathPrefix != "" && !strings.Contains(n.Signature, pathPrefix) &&
		!strings.Contains(n.Name, strings.Trim(pathPrefix, "/")) {
		return false
	}
	sig := strings.ToLower(n.Signature + " " + n.Name)
	return strings.Contains(sig, strings.ToLower(p.query))
}

func formatRoutesResult(routes []routeEntry, a topologyRoutesArgs) string {
	if len(routes) == 0 {
		msg := "topology_routes: no route patterns matched"
		if a.Framework != "" {
			msg += fmt.Sprintf(" (framework=%q)", a.Framework)
		}
		msg += "\nNote: route detection is pattern-matched (heuristic); confidence reflects approximate accuracy."
		return msg
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "topology routes: %d entry point(s) found (source=topology, heuristic)\n\n", len(routes))
	for _, r := range routes {
		fmt.Fprintf(&sb, "  %s %s\n", string(r.Node.Kind), r.Node.Name)
		fmt.Fprintf(&sb, "    path:    %s", r.Node.Path)
		if r.Node.StartLine > 0 {
			fmt.Fprintf(&sb, " L%d", r.Node.StartLine)
		}
		sb.WriteString("\n")
		if r.Node.Signature != "" {
			fmt.Fprintf(&sb, "    sig:     %s\n", r.Node.Signature)
		}
		fmt.Fprintf(&sb, "    pattern: %s  conf=%.2f\n", r.Pattern, r.Confidence)
		sb.WriteString("\n")
	}
	sb.WriteString("Note: results are pattern-matched against function names/signatures — not type-resolved.\n")
	return strings.TrimRight(sb.String(), "\n")
}
