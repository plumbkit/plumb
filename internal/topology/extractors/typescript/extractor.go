package typescript

import (
	"bufio"
	"bytes"
	"context"
	"regexp"
	"strings"

	"github.com/plumbkit/plumb/internal/topology"
)

// Extractor extracts TSX/JSX symbols using line-by-line heuristic scanning.
// Plain JavaScript (.js/.mjs/.cjs) is handled by the tree-sitter javascript
// extractor and TypeScript (.ts) by the tree-sitter TypeScript extractor; only
// .tsx/.jsx remain here because gotreesitter v0.19.1's TSX grammar still
// cascades on typed arrow parameters even with the regenerated TSX lex-states
// (see docs/internal/treesitter-plan.md).
type Extractor struct{}

// New returns a new TSX/JSX Extractor.
func New() *Extractor { return &Extractor{} }

func (e *Extractor) Language() string { return "typescript" }
func (e *Extractor) Extensions() []string {
	return []string{".tsx", ".jsx"}
}

// Pattern constants for symbol detection.
var (
	reFunction    = regexp.MustCompile(`^(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s+(\w+)`)
	reArrowFunc   = regexp.MustCompile(`^(?:export\s+)?(?:const|let|var)\s+(\w+)\s*=\s*(?:async\s+)?\(`)
	reArrowSimple = regexp.MustCompile(`^(?:export\s+)?(?:const|let|var)\s+(\w+)\s*=\s*(?:async\s+)?\w+\s*=>`)
	reClass       = regexp.MustCompile(`^(?:export\s+)?(?:abstract\s+)?class\s+(\w+)`)
	reInterface   = regexp.MustCompile(`^(?:export\s+)?interface\s+(\w+)`)
	reTypeAlias   = regexp.MustCompile(`^(?:export\s+)?type\s+(\w+)\s*=`)
	reMethodDef   = regexp.MustCompile(`^(?:(?:public|private|protected|static|async|override|get|set|readonly)\s+)+(\w+)\s*\(|^(\w+)\s*\(`)
	reImportES    = regexp.MustCompile(`^import\s+.*?from\s+['"]([^'"]+)['"]`)
	reImportCJS   = regexp.MustCompile(`^(?:const|let|var)\s+\w.*?=\s*require\(['"]([^'"]+)['"]\)`)
	// Express/router routes
	reExpressRoute = regexp.MustCompile(`\b(?:app|router)\.(get|post|put|delete|patch|use)\s*\(`)
	// Jest/Mocha/Vitest test blocks
	reTestBlock = regexp.MustCompile(`^(?:describe|it|test)\s*\(`)
	// Skip minified/generated files
	reMinifiedName = regexp.MustCompile(`(?:\.min\.|\.bundle\.)`)
)

// tsState tracks scanning state for one file.
type tsState struct {
	nodes      []topology.Node
	edges      []topology.Edge
	classIdx   int
	classDepth int // brace depth at which the class was opened
	braceDepth int
	lineNum    int
}

func newTSState() *tsState {
	return &tsState{classIdx: -1}
}

// Extract scans src line by line and extracts TypeScript/JavaScript symbols.
func (e *Extractor) Extract(_ context.Context, relPath string, src []byte) ([]topology.Node, []topology.Edge, error) {
	if reMinifiedName.MatchString(relPath) {
		return nil, nil, nil
	}
	scanner := bufio.NewScanner(bytes.NewReader(src))
	return tsScanLines(scanner, relPath)
}

func tsScanLines(scanner *bufio.Scanner, relPath string) ([]topology.Node, []topology.Edge, error) {
	st := newTSState()
	for scanner.Scan() {
		st.lineNum++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		st.braceDepth += countChar(line, '{') - countChar(line, '}')
		st.processLine(trimmed, relPath)
	}
	return st.nodes, st.edges, scanner.Err()
}

func (st *tsState) processLine(trimmed, relPath string) {
	if trimmed == "" || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
		return
	}
	st.updateClassContext()
	st.dispatchLine(trimmed, relPath)
}

// updateClassContext resets the active class when brace depth drops below where
// the class was opened. < (not <=) keeps classIdx active while still at the
// class's own brace level (e.g. after a method closes but before the class does).
func (st *tsState) updateClassContext() {
	if st.classIdx >= 0 && st.braceDepth < st.classDepth {
		st.classIdx = -1
	}
}

func (st *tsState) dispatchLine(trimmed, relPath string) {
	switch {
	case reClass.MatchString(trimmed):
		st.processClass(trimmed, relPath)
	case reInterface.MatchString(trimmed):
		st.processInterface(trimmed, relPath)
	case reTypeAlias.MatchString(trimmed):
		st.processTypeAlias(trimmed, relPath)
	case reFunction.MatchString(trimmed):
		st.processFunction(trimmed, relPath, false)
	case reArrowFunc.MatchString(trimmed) || reArrowSimple.MatchString(trimmed):
		st.processArrow(trimmed, relPath)
	case st.classIdx >= 0 && reMethodDef.MatchString(trimmed):
		st.processMethod(trimmed, relPath)
	case reTestBlock.MatchString(trimmed):
		st.processTestBlock(trimmed, relPath)
	case reExpressRoute.MatchString(trimmed):
		st.processExpressRoute(trimmed, relPath)
	case reImportES.MatchString(trimmed) || reImportCJS.MatchString(trimmed):
		st.processImport(trimmed, relPath)
	}
}

func (st *tsState) processClass(trimmed, relPath string) {
	m := reClass.FindStringSubmatch(trimmed)
	if len(m) < 2 {
		return
	}
	kind := topology.KindClass
	n := topology.Node{Kind: kind, Name: m[1], Qualified: m[1], StartLine: st.lineNum, Language: "typescript", Path: relPath}
	st.classIdx = len(st.nodes)
	st.classDepth = st.braceDepth
	st.nodes = append(st.nodes, n)
}

func (st *tsState) processInterface(trimmed, relPath string) {
	m := reInterface.FindStringSubmatch(trimmed)
	if len(m) < 2 {
		return
	}
	n := topology.Node{Kind: topology.KindType, Name: m[1], Qualified: m[1], StartLine: st.lineNum, Language: "typescript", Path: relPath}
	st.nodes = append(st.nodes, n)
}

func (st *tsState) processTypeAlias(trimmed, relPath string) {
	m := reTypeAlias.FindStringSubmatch(trimmed)
	if len(m) < 2 {
		return
	}
	n := topology.Node{Kind: topology.KindType, Name: m[1], Qualified: m[1], StartLine: st.lineNum, Language: "typescript", Path: relPath}
	st.nodes = append(st.nodes, n)
}

func (st *tsState) processFunction(trimmed, relPath string, isTest bool) {
	m := reFunction.FindStringSubmatch(trimmed)
	if len(m) < 2 {
		return
	}
	kind := topology.KindFunction
	if isTest || isTestFuncName(m[1]) {
		kind = topology.KindTest
	}
	n := topology.Node{Kind: kind, Name: m[1], Qualified: m[1], StartLine: st.lineNum, Language: "typescript", Path: relPath}
	st.nodes = append(st.nodes, n)
}

func (st *tsState) processArrow(trimmed, relPath string) {
	m := reArrowFunc.FindStringSubmatch(trimmed)
	if len(m) < 2 {
		m = reArrowSimple.FindStringSubmatch(trimmed)
	}
	if len(m) < 2 {
		return
	}
	name := m[1]
	kind := topology.KindFunction
	if isPascalCase(name) {
		kind = topology.KindFunction // React component — still a function kind
	}
	if isTestFuncName(name) {
		kind = topology.KindTest
	}
	n := topology.Node{Kind: kind, Name: name, Qualified: name, StartLine: st.lineNum, Language: "typescript", Path: relPath}
	st.nodes = append(st.nodes, n)
}

func (st *tsState) processMethod(trimmed, relPath string) {
	m := reMethodDef.FindStringSubmatch(trimmed)
	if len(m) < 2 {
		return
	}
	// The regex has two alternatives; pick the non-empty capture group.
	name := m[1]
	if name == "" && len(m) > 2 {
		name = m[2]
	}
	if name == "" || isKeyword(name) {
		return
	}
	nodeIdx := len(st.nodes)
	n := topology.Node{Kind: topology.KindMethod, Name: name, Qualified: name, StartLine: st.lineNum, Language: "typescript", Path: relPath}
	st.nodes = append(st.nodes, n)
	st.edges = append(st.edges, topology.Edge{
		FromID:     int64(st.classIdx),
		ToID:       int64(nodeIdx),
		Kind:       topology.EdgeContains,
		Confidence: 0.7,
		Source:     "heuristic",
	})
}

func (st *tsState) processTestBlock(trimmed, relPath string) {
	// describe("name", ...) or it("name", ...) or test("name", ...)
	start := strings.IndexByte(trimmed, '"')
	if start < 0 {
		start = strings.IndexByte(trimmed, '\'')
	}
	if start < 0 {
		start = strings.IndexByte(trimmed, '`')
	}
	name := "test"
	if start >= 0 && start+1 < len(trimmed) {
		rest := trimmed[start+1:]
		end := strings.IndexAny(rest, "\"'`")
		if end >= 0 {
			name = rest[:end]
		}
	}
	n := topology.Node{Kind: topology.KindTest, Name: name, Qualified: name, StartLine: st.lineNum, Language: "typescript", Path: relPath}
	st.nodes = append(st.nodes, n)
}

func (st *tsState) processExpressRoute(trimmed, relPath string) {
	m := reExpressRoute.FindStringSubmatch(trimmed)
	if len(m) < 2 {
		return
	}
	// Synthesise a function node named after the HTTP method + rough description.
	name := strings.ToUpper(m[1]) + "_route_L" + itoa(st.lineNum)
	n := topology.Node{
		Kind:      topology.KindFunction,
		Name:      name,
		Qualified: name,
		Signature: strings.TrimSpace(trimmed),
		StartLine: st.lineNum,
		Language:  "typescript",
		Path:      relPath,
	}
	st.nodes = append(st.nodes, n)
}

func (st *tsState) processImport(trimmed, relPath string) {
	m := reImportES.FindStringSubmatch(trimmed)
	if len(m) < 2 {
		m = reImportCJS.FindStringSubmatch(trimmed)
	}
	if len(m) < 2 {
		return
	}
	name := m[1]
	n := topology.Node{Kind: topology.KindImport, Name: name, Qualified: name, Language: "typescript", Path: relPath}
	st.nodes = append(st.nodes, n)
}

func countChar(s string, ch byte) int {
	count := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ch {
			count++
		}
	}
	return count
}

func isPascalCase(s string) bool {
	return len(s) > 0 && s[0] >= 'A' && s[0] <= 'Z'
}

func isTestFuncName(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasPrefix(lower, "test") || strings.HasPrefix(lower, "spec")
}

// isKeyword returns true for method-like keywords that are not real method names.
func isKeyword(s string) bool {
	switch s {
	case "if", "for", "while", "switch", "return", "const", "let", "var",
		"new", "typeof", "instanceof", "throw", "catch", "finally":
		return true
	}
	return false
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := make([]byte, 0, 10)
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	return string(buf)
}
