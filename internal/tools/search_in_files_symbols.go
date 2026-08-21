package tools

import (
	"context"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

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

// searchMaxOutputBytes bounds the rendered result. search_in_files was the one
// search tool with no total-output budget: its size was max_results ×
// (2·context_lines + 1) lines with no de-duplication of overlapping context
// windows, so raising the context_lines ceiling without this would have let a
// single call emit an unbounded response. read_file's search mode already
// bounds itself the same way (matchCollector.budget).
const searchMaxOutputBytes = 200 * 1024

// runeSafeCut returns the largest offset <= n that does not split a UTF-8 rune.
// Slicing a matched line by raw byte count to fit the output budget would emit
// an invalid byte sequence whenever the cut landed mid-rune — which any
// non-ASCII source line makes likely.
func runeSafeCut(s string, n int) int {
	if n >= len(s) {
		return len(s)
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return n
}

// renderSearchFiles writes each file's hits into sb, stopping once the output
// budget is spent. Returns how many files were actually shown and whether the
// budget cut the rendering short. Split out of formatSearchOutput to keep that
// function under the complexity gate.
func renderSearchFiles(sb *strings.Builder, results []*searchFileMatch, ann map[string]map[int]string) (filesShown int, budgetHit bool) {
files:
	for _, fm := range results {
		if sb.Len() >= searchMaxOutputBytes {
			budgetHit = true
			break
		}
		sb.WriteString(fm.relPath)
		sb.WriteByte('\n')
		fileAnn := ann[fm.absPath] // nil when feature off or no symbols
		hitIdx, linesShown := 0, 0
		for _, l := range fm.lines {
			// Bound the LINE against the remaining budget. Checking only BETWEEN
			// lines made the cap advisory: searchMaxLineBytes allows a 1 MiB line,
			// so a single long match passed every check and carried total output to
			// ~4.5x the 200 KiB the schema documents as absolute.
			remaining := searchMaxOutputBytes - sb.Len()
			if remaining <= 0 {
				budgetHit = true
				// Count this file only if some of it was actually shown, so the
				// "K of N file(s)" tally never credits a file whose header was
				// written and whose content was not.
				if linesShown > 0 {
					filesShown++
				}
				break files
			}
			if len(l) > remaining {
				sb.WriteString(l[:runeSafeCut(l, remaining)])
				sb.WriteString("…\n")
				budgetHit = true
				filesShown++
				break files
			}
			sb.WriteString(l)
			sb.WriteByte('\n')
			linesShown++
			// After a hit line (marker ":> "), append the enclosing symbol.
			if fileAnn != nil && strings.Contains(l, ":> ") && hitIdx < len(fm.hitLineNums) {
				lineNo := fm.hitLineNums[hitIdx]
				hitIdx++
				if name, ok := fileAnn[lineNo]; ok {
					fmt.Fprintf(sb, "  [in: %s]\n", name)
				}
			}
		}
		filesShown++
		sb.WriteByte('\n')
	}
	return filesShown, budgetHit
}

func formatSearchOutput(results []*searchFileMatch, ann map[string]map[int]string, a searchInFilesArgs, timedOut, truncated bool, totalLines, totalSkipped int) string {
	var sb strings.Builder
	filesShown, budgetHit := renderSearchFiles(&sb, results, ann)

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
	if budgetHit {
		// Say "also" when the max_results cap already reported a truncation, so the
		// two lines read as one story rather than as contradicting counts.
		also := ""
		if truncated {
			also = "also "
		}
		summary += fmt.Sprintf("\n⚠ output %struncated at %d KiB after %d of %d file(s) — lower context_lines or max_results, or narrow with glob/path.",
			also, searchMaxOutputBytes/1024, filesShown, len(results))
	}
	sb.WriteString(summary)

	// The summary stays at the bottom — it is a tally, and a tally belongs after
	// what it counts. But a CUT is not a tally: it means hits exist that are not
	// below, and a reader who concludes "only these N places match" from a
	// truncated list is simply wrong. That has to be said before the list.
	var banner string
	switch {
	case timedOut:
		banner = fmt.Sprintf("the search timed out after %s and did NOT finish. Matches "+
			"may exist that are not listed below. Narrow with path/glob before concluding "+
			"anything from these results.", searchDefaultDeadline)
	case truncated && budgetHit:
		banner = fmt.Sprintf("cut twice — at max_results=%d AND at the %d KiB output budget "+
			"(%d of %d files shown). Matches are missing below.",
			a.MaxResults, searchMaxOutputBytes/1024, filesShown, len(results))
	case truncated:
		banner = fmt.Sprintf("showing the first %d hit(s) only — more matches exist. "+
			"Pass max_results=N to raise the cap, or narrow with glob/path.", a.MaxResults)
	case budgetHit:
		banner = fmt.Sprintf("output hit the %d KiB budget after %d of %d file(s). The "+
			"remaining files' matches are missing below.",
			searchMaxOutputBytes/1024, filesShown, len(results))
	}
	return withTruncationBanner(sb.String(), banner)
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
	s := deepestEnclosingDocSymbol(syms, line)
	if s == nil {
		return ""
	}
	return fmt.Sprintf("%s (%s)", s.Name, symbolKindName(s.Kind))
}

// deepestEnclosingDocSymbol returns a pointer to the innermost symbol whose
// range contains the given 0-based line number, or nil when none matches. The
// pointer aliases an element of syms (or its Children), which the caller must
// keep alive while using it.
func deepestEnclosingDocSymbol(syms []protocol.DocumentSymbol, line uint32) *protocol.DocumentSymbol {
	var best *protocol.DocumentSymbol
	bestSize := uint32(0)
	var walk func([]protocol.DocumentSymbol, uint32)
	walk = func(ss []protocol.DocumentSymbol, depth uint32) {
		for i := range ss {
			s := &ss[i]
			if s.Range.Start.Line > line || s.Range.End.Line < line {
				continue
			}
			size := s.Range.End.Line - s.Range.Start.Line
			if best == nil || size < bestSize || (size == bestSize && depth > 0) {
				best = s
				bestSize = size
			}
			walk(s.Children, depth+1)
		}
	}
	walk(syms, 0)
	return best
}
