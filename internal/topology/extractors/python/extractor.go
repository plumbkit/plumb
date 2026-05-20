// Package python provides a regex-based topology extractor for Python source files.
// Extraction is approximate (heuristic indentation tracking) and confidence on
// containment edges is 0.8 to signal inferred rather than known relationships.
//
// Validation status: unit-tested with fixture files.
package python

import (
	"bufio"
	"bytes"
	"context"
	"strings"

	"github.com/golimpio/plumb/internal/topology"
)

// Extractor extracts Python symbols using line-by-line heuristic scanning.
type Extractor struct{}

// New returns a new Python Extractor.
func New() *Extractor { return &Extractor{} }

func (e *Extractor) Language() string     { return "python" }
func (e *Extractor) Extensions() []string { return []string{".py"} }

// Extract scans src line by line and extracts classes, functions, and imports.
func (e *Extractor) Extract(_ context.Context, relPath string, src []byte) ([]topology.Node, []topology.Edge, error) {
	scanner := bufio.NewScanner(bytes.NewReader(src))
	return scanLines(scanner, relPath)
}

type pyState struct {
	nodes       []topology.Node
	edges       []topology.Edge
	classIdx    int // index of active class node, or -1
	classIndent int
	lineNum     int
}

func newPyState() *pyState {
	return &pyState{classIdx: -1}
}

func scanLines(scanner *bufio.Scanner, relPath string) ([]topology.Node, []topology.Edge, error) {
	st := newPyState()
	for scanner.Scan() {
		st.lineNum++
		line := scanner.Text()
		trimmed := strings.TrimLeft(line, " \t")
		indent := len(line) - len(trimmed)
		st.processLine(trimmed, indent, relPath)
	}
	return st.nodes, st.edges, scanner.Err()
}

func (st *pyState) processLine(trimmed string, indent int, relPath string) {
	if trimmed == "" {
		return // blank lines do not end a class body
	}
	if st.classIdx >= 0 && indent <= st.classIndent {
		st.classIdx = -1
	}
	switch {
	case strings.HasPrefix(trimmed, "class "):
		st.processClass(trimmed, indent, relPath)
	case strings.HasPrefix(trimmed, "def ") || strings.HasPrefix(trimmed, "async def "):
		st.processFunc(trimmed, indent, relPath)
	case strings.HasPrefix(trimmed, "import ") || strings.HasPrefix(trimmed, "from "):
		st.processImport(trimmed, relPath)
	}
}

func (st *pyState) processClass(trimmed string, indent int, relPath string) {
	name := extractPyName(trimmed, "class ")
	if name == "" {
		return
	}
	n := topology.Node{
		Kind:      topology.KindClass,
		Name:      name,
		Qualified: name,
		StartLine: st.lineNum,
		Language:  "python",
		Path:      relPath,
	}
	st.classIdx = len(st.nodes)
	st.classIndent = indent
	st.nodes = append(st.nodes, n)
}

func (st *pyState) processFunc(trimmed string, indent int, relPath string) {
	prefix := "def "
	if strings.HasPrefix(trimmed, "async ") {
		prefix = "async def "
	}
	name := extractPyName(trimmed, prefix)
	if name == "" {
		return
	}
	kind := topology.KindFunction
	if st.classIdx >= 0 && indent > st.classIndent {
		kind = topology.KindMethod
	}
	if isTestName(name) {
		kind = topology.KindTest
	}
	n := topology.Node{
		Kind:      kind,
		Name:      name,
		Qualified: name,
		StartLine: st.lineNum,
		Language:  "python",
		Path:      relPath,
	}
	nodeIdx := len(st.nodes)
	st.nodes = append(st.nodes, n)
	if st.classIdx >= 0 && kind == topology.KindMethod {
		st.edges = append(st.edges, topology.Edge{
			FromID:     int64(st.classIdx),
			ToID:       int64(nodeIdx),
			Kind:       topology.EdgeContains,
			Confidence: 0.8,
			Source:     "heuristic",
		})
	}
}

func (st *pyState) processImport(trimmed, relPath string) {
	name := importName(trimmed)
	if name == "" {
		return
	}
	n := topology.Node{
		Kind:     topology.KindImport,
		Name:     name,
		Language: "python",
		Path:     relPath,
	}
	st.nodes = append(st.nodes, n)
}

func extractPyName(trimmed, prefix string) string {
	rest := strings.TrimPrefix(trimmed, prefix)
	name, _, _ := strings.Cut(rest, "(")
	name, _, _ = strings.Cut(name, ":")
	name = strings.TrimSpace(name)
	if strings.ContainsAny(name, " \t") {
		return ""
	}
	return name
}

func importName(trimmed string) string {
	if strings.HasPrefix(trimmed, "from ") {
		parts := strings.Fields(trimmed)
		if len(parts) >= 2 {
			return parts[1]
		}
		return ""
	}
	rest := strings.TrimPrefix(trimmed, "import ")
	name, _, _ := strings.Cut(rest, " ")
	name, _, _ = strings.Cut(name, ",")
	return strings.TrimSpace(name)
}

func isTestName(name string) bool {
	return strings.HasPrefix(name, "test_") || strings.HasPrefix(name, "Test")
}
