package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/plumbkit/plumb/internal/topology"
)

var topologyRoutesSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "framework": {
      "type": "string",
      "description": "One framework: 'net/http', 'mux' (net/http + gorilla), 'chi', 'gin', 'echo', 'cobra', 'flask', 'fastapi', or name-match-only 'vapor' / 'argument-parser'. Omit for all."
    },
    "path_prefix": {
      "type": "string",
      "description": "Route prefix ('/api') or Cobra command path ('plumb config'); name-match candidates filter by symbol name instead."
    },
    "limit": {
      "type": "integer",
      "description": "Maximum routes and maximum commands listed (default 50 each).",
      "default": 50
    }
  },
  "additionalProperties": false
}`)

// TopologyRoutes recovers route registrations (route string -> handler) and Cobra
// command trees from the call sites recorded in the topology index, falling back
// to name/signature pattern candidates only where no registration site matched.
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
	return "Entry points recovered from REGISTRATION sites in the topology index: route -> handler for Go net/http, gorilla/mux, chi, gin and echo and Python Flask/FastAPI decorators, plus the Cobra command tree (Use -> Run/RunE via AddCommand). A site counts only when its file imports that framework. Handlers are labelled resolved, same-package, decorated, name-match, ambiguous, external or unresolved; none is type-checked, and group prefixes are not composed. Frameworks with no recovered site (Swift Vapor, ArgumentParser) fall back to name-match candidates, which are guesses."
}

// routeEntry is a name-match candidate: a symbol whose name or signature looks
// like an entry-point idiom, with no registration site behind it.
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
	fws, siteCovered := siteFrameworks(a.Framework)
	rep := &topology.RouteReport{}
	if siteCovered {
		if rep, err = store.Routes(ctx, topology.RouteOpts{Frameworks: fws}); err != nil {
			return "", withIndexHealthErr(store, fmt.Errorf("topology_routes: %w", err))
		}
	}
	candidates, err := t.run(ctx, store, a, fallbackPatterns(a.Framework, siteCovered, rep))
	if err != nil {
		return "", withIndexHealthErr(store, err)
	}
	return withIndexHealth(store, formatRoutesReport(rep, candidates, a)), nil
}

func parseTopologyRoutesArgs(raw json.RawMessage) (topologyRoutesArgs, error) {
	var a topologyRoutesArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("topology_routes: invalid arguments: %w", err)
	}
	if a.Limit <= 0 {
		a.Limit = 50
	}
	return a, nil
}

// siteFrameworks maps the framework argument to the families route recovery
// reads from registration sites. siteCovered is false for a framework only the
// name-match fallback knows (Swift has no recorded sites). An unknown name means
// "all", as it always has.
func siteFrameworks(framework string) (fws []topology.RouteFramework, siteCovered bool) {
	switch strings.ToLower(strings.TrimSpace(framework)) {
	case "net/http", "http", "nethttp":
		return []topology.RouteFramework{topology.FrameworkNetHTTP}, true
	case "mux":
		return []topology.RouteFramework{topology.FrameworkNetHTTP, topology.FrameworkGorilla}, true
	case "gorilla", "gorilla/mux":
		return []topology.RouteFramework{topology.FrameworkGorilla}, true
	case "chi":
		return []topology.RouteFramework{topology.FrameworkChi}, true
	case "gin":
		return []topology.RouteFramework{topology.FrameworkGin}, true
	case "echo":
		return []topology.RouteFramework{topology.FrameworkEcho}, true
	case "cobra":
		return []topology.RouteFramework{topology.FrameworkCobra}, true
	case "flask":
		return []topology.RouteFramework{topology.FrameworkFlask}, true
	case "fastapi":
		return []topology.RouteFramework{topology.FrameworkFastAPI}, true
	case "vapor", "argument-parser":
		return nil, false
	default:
		return nil, true
	}
}

// fallbackPatterns picks the name-match patterns to run. Swift idioms always
// run when asked for (no Swift site is recorded); the other patterns run only
// when recovery found nothing, so a recovered binding is never shadowed by, or
// listed beside, a guess about the same code.
func fallbackPatterns(framework string, siteCovered bool, rep *topology.RouteReport) []routePattern {
	if !siteCovered {
		return routePatterns(framework)
	}
	recovered := len(rep.Bindings) > 0 || rep.CommandCount > 0
	if strings.TrimSpace(framework) != "" {
		if recovered {
			return nil
		}
		return routePatterns(framework)
	}
	if !recovered {
		return routePatterns("")
	}
	var swift []routePattern
	for _, p := range routePatterns("") {
		if matchesFramework(p.name, "vapor") || matchesFramework(p.name, "argument-parser") {
			swift = append(swift, p)
		}
	}
	return swift
}

func (t *TopologyRoutes) run(ctx context.Context, store *topology.Store, a topologyRoutesArgs, patterns []routePattern) ([]routeEntry, error) {
	if store == nil {
		return nil, nil
	}
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
	nameEquals string // when set, the candidate's symbol name must equal this exactly
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
		// Swift/Vapor patterns — match against function signatures stored by the
		// Swift tree-sitter extractor (e.g. "RoutesBuilder" in boot(routes:)).
		{query: "RoutesBuilder", name: "vapor.RouteCollection", confidence: 0.75},
		{query: ": Application", name: "vapor.configure", confidence: 0.65},
		// Swift ArgumentParser — ParsableCommand conformance is propagated onto the
		// type's methods by the Swift extractor; the entry point is the run() method.
		{query: "ParsableCommand", name: "argument-parser.run", confidence: 0.70, nameEquals: "run"},
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
	case "mux", "net/http", "http", "nethttp", "gorilla", "gorilla/mux":
		return strings.Contains(patternName, "mux") || strings.Contains(patternName, "HandleFunc")
	case "fastapi", "flask":
		return strings.Contains(patternName, "@")
	case "vapor":
		return strings.Contains(patternName, "vapor.")
	case "argument-parser":
		return strings.Contains(patternName, "argument-parser.")
	default:
		return true
	}
}

func isRouteCandidate(n topology.Node, p routePattern, pathPrefix string) bool {
	if p.nameEquals != "" && n.Name != p.nameEquals {
		return false
	}
	if pathPrefix != "" && !strings.Contains(n.Signature, pathPrefix) &&
		!strings.Contains(n.Name, strings.Trim(pathPrefix, "/")) {
		return false
	}
	sig := strings.ToLower(n.Signature + " " + n.Name)
	return strings.Contains(sig, strings.ToLower(p.query))
}

// formatRoutesReport renders recovered bindings, the Cobra tree, and any
// name-match candidates — counts first, each list capped at a.Limit.
func formatRoutesReport(rep *topology.RouteReport, candidates []routeEntry, a topologyRoutesArgs) string {
	bindings := filterBindings(rep.Bindings, a.PathPrefix)
	var sb strings.Builder
	if len(bindings) == 0 && rep.CommandCount == 0 && len(candidates) == 0 {
		sb.WriteString("topology_routes: no registration sites and no name-match candidates found")
		if a.Framework != "" {
			fmt.Fprintf(&sb, " (framework=%q)", a.Framework)
		}
		sb.WriteString("\nNote: recovery reads Go and Python registration sites; a framework the registering file does not import is not recognised.")
		return sb.String()
	}

	if len(bindings) > 0 || rep.CommandCount > 0 {
		fmt.Fprintf(&sb, "topology routes: %d HTTP route(s), %d Cobra command(s) recovered from registration sites (source=topology call sites)\n",
			len(bindings), rep.CommandCount)
		writeRouteCounts(&sb, bindings, rep)
	}
	if len(bindings) > 0 {
		sb.WriteString("\nHTTP routes:\n")
		for i, b := range bindings {
			if i == a.Limit {
				fmt.Fprintf(&sb, "  … %d more route(s) omitted (raise limit)\n", len(bindings)-i)
				break
			}
			method := b.Method
			if method == "" {
				method = "*"
			}
			fmt.Fprintf(&sb, "  %s %s [%s] -> %s\n      registered %s:%d\n",
				method, routeText(b), b.Framework, handlerText(b.Handler), b.Path, b.Line)
		}
	}
	if rep.CommandCount > 0 {
		writeCommandTree(&sb, rep, a)
	}
	if len(candidates) > 0 {
		fmt.Fprintf(&sb, "\nname-match candidates: %d (no registration site; a name/signature pattern, not a binding)\n", len(candidates))
		for _, r := range candidates {
			fmt.Fprintf(&sb, "  %s %s  %s", string(r.Node.Kind), r.Node.Name, r.Node.Path)
			if r.Node.StartLine > 0 {
				fmt.Fprintf(&sb, " L%d", r.Node.StartLine)
			}
			fmt.Fprintf(&sb, "\n    pattern: %s  conf=%.2f (name-match)\n", r.Pattern, r.Confidence)
		}
	}
	sb.WriteString("\nNote: handlers are tied by import and package scope, not type-checked; router group/mount prefixes are not composed.")
	return strings.TrimRight(sb.String(), "\n")
}

func filterBindings(bs []topology.RouteBinding, prefix string) []topology.RouteBinding {
	if prefix == "" {
		return bs
	}
	var out []topology.RouteBinding
	for _, b := range bs {
		if !b.Dynamic && strings.HasPrefix(b.Route, prefix) {
			out = append(out, b)
		}
	}
	return out
}

func routeText(b topology.RouteBinding) string {
	switch {
	case !b.Dynamic:
		return b.Route
	case b.Route != "":
		return "<dynamic: " + b.Route + ">"
	default:
		return "<dynamic>"
	}
}

func handlerText(h topology.RouteHandler) string {
	text := h.Text
	if text == "" {
		text = "<inline or expression>"
	}
	switch {
	case h.Node != nil:
		return fmt.Sprintf("%s (%s, %s:%d)", text, h.Confidence, h.Node.Path, h.Node.StartLine)
	case h.Confidence == topology.HandlerAmbiguous:
		return fmt.Sprintf("%s (ambiguous: %d candidates)", text, h.Candidates)
	default:
		return fmt.Sprintf("%s (%s)", text, h.Confidence)
	}
}

// writeRouteCounts prints per-framework and per-confidence tallies.
func writeRouteCounts(sb *strings.Builder, bindings []topology.RouteBinding, rep *topology.RouteReport) {
	byFw := map[topology.RouteFramework]int{}
	byConf := map[topology.HandlerConfidence]int{}
	for _, b := range bindings {
		byFw[b.Framework]++
		byConf[b.Handler.Confidence]++
	}
	groups := 0
	byFw[topology.FrameworkCobra] += rep.CommandCount
	visitCommands(rep.Commands, func(c *topology.Command, _ int) {
		if c.Handler.Confidence == "" {
			groups++
			return
		}
		byConf[c.Handler.Confidence]++
	})
	var fw []string
	for _, f := range topology.AllRouteFrameworks {
		if byFw[f] > 0 {
			fw = append(fw, fmt.Sprintf("%s %d", f, byFw[f]))
		}
	}
	fmt.Fprintf(sb, "  frameworks: %s\n", strings.Join(fw, ", "))
	var conf []string
	for _, c := range []topology.HandlerConfidence{
		topology.HandlerResolved, topology.HandlerSamePackage,
		topology.HandlerDecorated, topology.HandlerNameMatch, topology.HandlerAmbiguous,
		topology.HandlerExternal, topology.HandlerUnresolved,
	} {
		if byConf[c] > 0 {
			conf = append(conf, fmt.Sprintf("%s %d", c, byConf[c]))
		}
	}
	if groups > 0 {
		conf = append(conf, fmt.Sprintf("group commands (no Run) %d", groups))
	}
	fmt.Fprintf(sb, "  handlers:   %s\n", strings.Join(conf, ", "))
}

// visitCommands walks the recovered command forest depth-first, once per
// command even if it is registered under two parents or in a cycle.
func visitCommands(roots []*topology.Command, fn func(c *topology.Command, depth int)) {
	seen := map[*topology.Command]bool{}
	var walk func(c *topology.Command, depth int)
	walk = func(c *topology.Command, depth int) {
		if seen[c] {
			return
		}
		seen[c] = true
		fn(c, depth)
		kids := append([]*topology.Command(nil), c.Children...)
		sort.SliceStable(kids, func(i, j int) bool { return kids[i].Name() < kids[j].Name() })
		for _, k := range kids {
			walk(k, depth+1)
		}
	}
	for _, r := range roots {
		walk(r, 0)
	}
}

func writeCommandTree(sb *strings.Builder, rep *topology.RouteReport, a topologyRoutesArgs) {
	sb.WriteString("\nCobra commands:\n")
	printed, matched := 0, 0
	var names []string
	visitCommands(rep.Commands, func(c *topology.Command, depth int) {
		names = append(names[:depth], commandName(c))
		full := strings.Join(names, " ")
		if a.PathPrefix != "" && !strings.HasPrefix(full, a.PathPrefix) {
			return
		}
		matched++
		if printed >= a.Limit {
			return
		}
		printed++
		label, indent := commandName(c), strings.Repeat("  ", depth+1)
		if a.PathPrefix != "" {
			label, indent = full, "  "
		}
		fmt.Fprintf(sb, "%s%s", indent, label)
		if c.Handler.Confidence != "" {
			fmt.Fprintf(sb, " -> %s", handlerText(c.Handler))
		}
		fmt.Fprintf(sb, "  [%s %s:%d]\n", c.Decl, c.Path, c.Line)
	})
	if matched > printed {
		fmt.Fprintf(sb, "  … %d more command(s) omitted (raise limit)\n", matched-printed)
	}
	if rep.UnlinkedChildArgs > 0 {
		fmt.Fprintf(sb, "  %d AddCommand argument(s) not linked (a factory call or expression, an unknown name, or an unrecovered parent); their commands, if recovered, are listed at top level\n",
			rep.UnlinkedChildArgs)
	}
}

func commandName(c *topology.Command) string {
	if c.Dynamic {
		return "<dynamic Use>"
	}
	return c.Name()
}
