package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/golimpio/plumb/internal/topology"
)

// structuralQueries is the curated, named query set. The tool deliberately does
// NOT expose raw tree-sitter S-expression queries: an LLM cannot reliably name
// per-grammar node types, so the surface is a small set of vetted checks that
// run over the topology index (the structural map). Add a query by extending
// this set and the dispatch in run().
var structuralQueries = map[string]string{
	"undocumented-exports": "exported symbols (functions, methods, types, constants) with no doc comment",
	"long-functions":       "functions/methods longer than min_lines (default 80) — decomposition candidates",
	"unused-context":       "Go functions taking a context.Context parameter whose body never references it",
}

var structuralQuerySchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "Which structural check to run: \"undocumented-exports\", \"long-functions\", or \"unused-context\".",
      "enum": ["undocumented-exports", "long-functions", "unused-context"]
    },
    "language": {
      "type": "string",
      "description": "Optional filter by language (e.g. 'go', 'python'). unused-context is Go-only regardless."
    },
    "min_lines": {
      "type": "integer",
      "description": "For long-functions: minimum line span to flag. Default 80."
    },
    "limit": {
      "type": "integer",
      "description": "Maximum number of findings to return. Default 50."
    }
  },
  "required": ["query"],
  "additionalProperties": false
}`)

// StructuralQuery runs a small curated set of named structural checks over the
// topology index. It complements topology_search (find by name) with find-by-
// shape audits useful for review and refactor preparation.
//
// Concurrency: Execute is safe for concurrent use.
type StructuralQuery struct {
	storeFn func() *topology.Store
	ws      WorkspaceFn
}

// NewStructuralQuery returns the tool. storeFn returns the session's topology
// store (nil when disabled); ws returns the workspace root, used to read
// function bodies for the unused-context check.
func NewStructuralQuery(storeFn func() *topology.Store, ws WorkspaceFn) *StructuralQuery {
	return &StructuralQuery{storeFn: storeFn, ws: ws}
}

func (*StructuralQuery) Name() string                 { return "structural_query" }
func (*StructuralQuery) InputSchema() json.RawMessage { return structuralQuerySchema }
func (*StructuralQuery) Description() string {
	return "Run a curated structural check over the topology index — find symbols by SHAPE, not name. " +
		"Complements topology_search (find by name) and search_in_files (find by text) with audits useful " +
		"for review and refactor prep. Named queries (no raw tree-sitter queries are exposed): " +
		"\"undocumented-exports\" (exported functions/methods/types/constants with no doc comment), " +
		"\"long-functions\" (functions over min_lines, default 80), " +
		"\"unused-context\" (Go functions taking context.Context whose body never references it). " +
		"Results are approximate (source=topology) and confidence-labelled where the check is heuristic. " +
		"Returns a clear message when the index is disabled or empty."
}

type structuralQueryArgs struct {
	Query    string `json:"query"`
	Language string `json:"language"`
	MinLines int    `json:"min_lines"`
	Limit    int    `json:"limit"`
}

// structFinding is one flagged symbol.
type structFinding struct {
	path   string
	line   int
	kind   string
	name   string
	detail string
}

func (t *StructuralQuery) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	a, err := parseStructuralQueryArgs(raw)
	if err != nil {
		return "", err
	}
	store := t.storeFn()
	if store == nil {
		return topologyDisabledMessage(), nil
	}
	findings, runErr := t.run(ctx, store, a)
	if runErr != nil {
		return "", runErr
	}
	return formatStructuralFindings(a, findings), nil
}

func parseStructuralQueryArgs(raw json.RawMessage) (structuralQueryArgs, error) {
	var a structuralQueryArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("structural_query: invalid arguments: %w", err)
	}
	if _, ok := structuralQueries[a.Query]; !ok {
		return a, fmt.Errorf("structural_query: unknown query %q; choose one of: undocumented-exports, long-functions, unused-context", a.Query)
	}
	if a.MinLines <= 0 {
		a.MinLines = 80
	}
	if a.Limit <= 0 {
		a.Limit = 50
	}
	return a, nil
}

func (t *StructuralQuery) run(ctx context.Context, store *topology.Store, a structuralQueryArgs) ([]structFinding, error) {
	switch a.Query {
	case "undocumented-exports":
		return queryUndocumentedExports(ctx, store, a)
	case "long-functions":
		return queryLongFunctions(ctx, store, a)
	case "unused-context":
		return t.queryUnusedContext(ctx, store, a)
	default:
		return nil, nil
	}
}

// matchesLang reports whether n passes the optional language filter.
func matchesLang(n topology.Node, lang string) bool {
	return lang == "" || strings.EqualFold(n.Language, lang)
}

// isExported reports whether name is exported under its language's convention:
// Python (and similar) treat a leading underscore as private; everything else
// uses the leading-uppercase rule (Go, Java, Kotlin, Rust pub-by-case, …).
func isExported(name, language string) bool {
	if name == "" {
		return false
	}
	if strings.EqualFold(language, "python") {
		return !strings.HasPrefix(name, "_")
	}
	return unicode.IsUpper([]rune(name)[0])
}

func queryUndocumentedExports(ctx context.Context, store *topology.Store, a structuralQueryArgs) ([]structFinding, error) {
	nodes, err := store.NodesByKind(ctx, topology.KindFunction, topology.KindMethod,
		topology.KindType, topology.KindClass, topology.KindConstant)
	if err != nil {
		return nil, err
	}
	var out []structFinding
	for _, n := range nodes {
		if !matchesLang(n, a.Language) || !isExported(n.Name, n.Language) {
			continue
		}
		if strings.TrimSpace(n.Docstring) != "" {
			continue
		}
		out = append(out, structFinding{
			path: n.Path, line: n.StartLine, kind: string(n.Kind), name: n.Name,
			detail: "no doc comment",
		})
	}
	return out, nil
}

func queryLongFunctions(ctx context.Context, store *topology.Store, a structuralQueryArgs) ([]structFinding, error) {
	nodes, err := store.NodesByKind(ctx, topology.KindFunction, topology.KindMethod)
	if err != nil {
		return nil, err
	}
	var out []structFinding
	for _, n := range nodes {
		span := n.EndLine - n.StartLine + 1
		if !matchesLang(n, a.Language) || span < a.MinLines {
			continue
		}
		out = append(out, structFinding{
			path: n.Path, line: n.StartLine, kind: string(n.Kind), name: n.Name,
			detail: fmt.Sprintf("%d lines", span),
		})
	}
	// Longest first — the strongest decomposition candidates lead.
	sort.SliceStable(out, func(i, j int) bool { return spanOf(out[i]) > spanOf(out[j]) })
	return out, nil
}

// spanOf parses the "<n> lines" detail back to an int for sorting.
func spanOf(f structFinding) int {
	n := 0
	_, _ = fmt.Sscanf(f.detail, "%d lines", &n)
	return n
}

func (t *StructuralQuery) queryUnusedContext(ctx context.Context, store *topology.Store, a structuralQueryArgs) ([]structFinding, error) {
	root := ""
	if t.ws != nil {
		root = t.ws()
	}
	if root == "" {
		return nil, fmt.Errorf("structural_query: unused-context needs a resolved workspace to read function bodies")
	}
	nodes, err := store.NodesByKind(ctx, topology.KindFunction, topology.KindMethod)
	if err != nil {
		return nil, err
	}
	var out []structFinding
	for _, n := range nodes {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if !matchesLang(n, a.Language) {
			continue
		}
		if !strings.Contains(n.Signature, "context.Context") {
			continue // Go-specific; a context param is the trigger
		}
		param := ctxParamName(n.Signature)
		if param == "" || param == "_" {
			continue // grouped/anonymous param — can't attribute usage, skip rather than false-flag
		}
		used, ok := ctxParamUsedInBody(root, n.Path, n.StartLine, n.EndLine, param)
		if !ok || used {
			continue // unreadable body, or the param is referenced → not a finding
		}
		out = append(out, structFinding{
			path: n.Path, line: n.StartLine, kind: string(n.Kind), name: n.Name,
			detail: fmt.Sprintf("context param %q never referenced in body", param),
		})
	}
	return out, nil
}

// ctxParamName extracts the parameter name immediately preceding the first
// "context.Context" in a Go signature, e.g. "func F(ctx context.Context) ..."
// → "ctx". Returns "" when it cannot be determined unambiguously (grouped
// params like "a, b context.Context", or no identifier before the type).
func ctxParamName(sig string) string {
	idx := strings.Index(sig, "context.Context")
	if idx < 0 {
		return ""
	}
	before := strings.TrimRight(sig[:idx], " \t")
	// The token just before the type is the parameter name; bail on a grouped
	// list (trailing comma) since the name no longer adjoins the type.
	if strings.HasSuffix(before, ",") {
		return ""
	}
	i := len(before)
	for i > 0 && isIdentRune(rune(before[i-1])) {
		i--
	}
	name := before[i:]
	if name == "" || !isIdentStart(rune(name[0])) {
		return ""
	}
	// A grouped list ("a, b context.Context") makes both params the context
	// type, so the name no longer maps 1:1 to the type — skip it.
	if prefix := strings.TrimRight(before[:i], " \t"); strings.HasSuffix(prefix, ",") {
		return ""
	}
	return name
}

func isIdentRune(r rune) bool  { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }
func isIdentStart(r rune) bool { return r == '_' || unicode.IsLetter(r) }

// ctxParamUsedInBody reports whether param appears as a whole word in the body
// of the symbol at relPath (workspace-relative) over lines start..end, excluding
// the signature line itself. ok is false when the file cannot be read or the
// line range is invalid, so callers skip rather than false-flag.
func ctxParamUsedInBody(root, relPath string, start, end int, param string) (used, ok bool) {
	if start <= 0 || end < start {
		return false, false
	}
	data, err := os.ReadFile(filepath.Join(root, relPath))
	if err != nil {
		return false, false
	}
	lines := strings.Split(string(data), "\n")
	if end > len(lines) {
		end = len(lines)
	}
	// Body = lines after the signature line. lines is 0-based, start/end 1-based,
	// so lines[start:end] is the original lines (start+1)..end — the signature at
	// line `start` is excluded.
	if start >= end {
		return false, true // single-line body — nothing after the signature
	}
	body := strings.Join(lines[start:end], "\n")
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(param) + `\b`)
	return re.MatchString(body), true
}

func formatStructuralFindings(a structuralQueryArgs, findings []structFinding) string {
	if len(findings) == 0 {
		return fmt.Sprintf("structural_query %q: no findings.", a.Query)
	}
	total := len(findings)
	truncated := false
	if total > a.Limit {
		findings = findings[:a.Limit]
		truncated = true
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "structural_query %q: %d finding(s) (source=topology, mode=structural)\n\n", a.Query, total)
	for _, f := range findings {
		loc := f.path
		if f.line > 0 {
			loc += fmt.Sprintf(":%d", f.line)
		}
		fmt.Fprintf(&sb, "  %s %s\n    %s — %s\n", f.kind, f.name, loc, f.detail)
	}
	if truncated {
		fmt.Fprintf(&sb, "\n… showing first %d of %d (raise limit to see more).", a.Limit, total)
	}
	return strings.TrimRight(sb.String(), "\n")
}
