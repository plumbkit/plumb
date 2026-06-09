package tools

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

func (t *SearchInFiles) annotateWithSymbols(ctx context.Context, a searchInFilesArgs, results []*searchFileMatch) map[string]map[int]string {
	if !a.IncludeEnclosingSymbol || t.client == nil {
		return nil
	}
	fileAnnotations := make(map[string]map[int]string)
	for _, fm := range results {
		uri := protocol.FileURI(fm.absPath)
		syms := t.docSymbolsCached(ctx, uri)
		if len(syms) == 0 {
			continue
		}
		m := make(map[int]string, len(fm.hitLineNums))
		for _, lineNo := range fm.hitLineNums {
			ln := lineNo - 1
			if ln < 0 || ln > math.MaxUint32 {
				continue
			}
			if sym := deepestEnclosingSymbol(syms, uint32(ln)); sym != "" {
				m[lineNo] = sym
			}
		}
		if len(m) > 0 {
			fileAnnotations[fm.absPath] = m
		}
	}
	return fileAnnotations
}

func formatSearchOutput(results []*searchFileMatch, ann map[string]map[int]string, a searchInFilesArgs, timedOut, truncated bool, totalLines, totalSkipped int) string {
	var sb strings.Builder
	for _, fm := range results {
		sb.WriteString(fm.relPath)
		sb.WriteByte('\n')
		fileAnn := ann[fm.absPath] // nil when feature off or no symbols
		hitIdx := 0
		for _, l := range fm.lines {
			sb.WriteString(l)
			sb.WriteByte('\n')
			// After a hit line (marker ":> "), append the enclosing symbol.
			if fileAnn != nil && strings.Contains(l, ":> ") && hitIdx < len(fm.hitLineNums) {
				lineNo := fm.hitLineNums[hitIdx]
				hitIdx++
				if name, ok := fileAnn[lineNo]; ok {
					fmt.Fprintf(&sb, "  [in: %s]\n", name)
				}
			}
		}
		sb.WriteByte('\n')
	}

	var summary string
	switch {
	case timedOut:
		summary = fmt.Sprintf("Showing %d hit(s) across %d file(s) — partial (search timed out after %s; narrow with path/glob or set a tighter pattern).", totalLines, len(results), searchDefaultDeadline)
	case truncated:
		summary = fmt.Sprintf("Showing first %d hit(s) across %d file(s) — limit reached (pass max_results=N to raise, or narrow with glob/path/pattern).", a.MaxResults, len(results))
	default:
		summary = fmt.Sprintf("%d hit(s) across %d file(s).", totalLines, len(results))
	}
	if totalSkipped > 0 {
		summary += fmt.Sprintf(" (%d oversized line(s) skipped)", totalSkipped)
	}
	sb.WriteString(summary)
	return sb.String()
}

// docSymbolsCached returns DocumentSymbols for uri, consulting t.symCache first.
// Returns nil when the LSP call fails; callers treat nil as "no annotation".
func (t *SearchInFiles) docSymbolsCached(ctx context.Context, uri string) []protocol.DocumentSymbol {
	key := uri + ":docSymbols"
	if t.symCache != nil {
		if v, ok := t.symCache.Get(key); ok {
			return v.([]protocol.DocumentSymbol)
		}
	}
	syms, err := t.client.DocumentSymbols(ctx, protocol.DocumentSymbolParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
	})
	if err != nil {
		return nil
	}
	if t.symCache != nil {
		t.symCache.Set(key, syms, t.cacheTTL)
	}
	return syms
}

// deepestEnclosingSymbol returns "Name (kind)" for the innermost symbol whose
// range contains the given 0-based line number, or "" when none matches.
func deepestEnclosingSymbol(syms []protocol.DocumentSymbol, line uint32) string {
	best := ""
	bestSize := uint32(0)
	var walk func([]protocol.DocumentSymbol, uint32)
	walk = func(ss []protocol.DocumentSymbol, depth uint32) {
		for _, s := range ss {
			if s.Range.Start.Line > line || s.Range.End.Line < line {
				continue
			}
			size := s.Range.End.Line - s.Range.Start.Line
			if best == "" || size < bestSize || (size == bestSize && depth > 0) {
				best = fmt.Sprintf("%s (%s)", s.Name, symbolKindName(s.Kind))
				bestSize = size
			}
			walk(s.Children, depth+1)
		}
	}
	walk(syms, 0)
	return best
}
