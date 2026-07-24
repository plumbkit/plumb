package minchange

import (
	"regexp"
	"strings"
)

// addedSymbol is a Go declaration (function, method, or type) introduced by the
// diff — a candidate for the abstraction checks. It is detected on '+' lines
// only, so it represents genuinely new code, not a moved or edited region.
type addedSymbol struct {
	Name string
	Kind string // "function" | "method" | "type"
	File string
	Line int // 1-based new-side line of the declaration

	// The fields below are populated only when the whole declaration lives in a
	// contiguous run of added lines (fullyAdded), so the body can be analysed
	// for thin-forwarding. They are empty otherwise.
	fullyAdded bool
	signature  string   // the func signature up to the opening brace, joined
	params     []string // parameter names, in order
	bodyStmt   string   // the single body statement, when the body has exactly one
}

var (
	// funcHeaderRe matches the start of a Go func declaration on one line,
	// capturing an optional receiver, the name, and everything after the name.
	funcHeaderRe = regexp.MustCompile(`^func\s+(?:\((?P<recv>[^)]*)\)\s+)?(?P<name>[A-Za-z_]\w*)\s*(?P<rest>.*)$`)
	// typeHeaderRe matches a single type declaration (not a `type (` group).
	typeHeaderRe = regexp.MustCompile(`^type\s+(?P<name>[A-Za-z_]\w*)\b`)
)

// collectAddedSymbols scans every Go file in the diff for newly-added function,
// method, and type declarations. Test files are included for name-similarity
// context but the checks decide per-kind whether to act on them.
func collectAddedSymbols(diff *Diff) []addedSymbol {
	var out []addedSymbol
	for i := range diff.Files {
		f := &diff.Files[i]
		if f.IsBinary || f.IsDelete || !isGoFile(f.Path) {
			continue
		}
		for h := range f.Hunks {
			out = append(out, symbolsInHunk(f.Path, &f.Hunks[h])...)
		}
	}
	return out
}

// symbolsInHunk walks a hunk's lines and returns the declarations that begin on
// an added line. For a func whose whole body is also within the added run, the
// body is captured for thin-wrapper analysis.
func symbolsInHunk(path string, h *Hunk) []addedSymbol {
	var out []addedSymbol
	for i := 0; i < len(h.Lines); i++ {
		ln := h.Lines[i]
		if ln.Kind != Added {
			continue
		}
		text := strings.TrimSpace(ln.Text)
		if m := funcHeaderRe.FindStringSubmatch(text); m != nil {
			sym := addedSymbol{
				Name: m[funcHeaderRe.SubexpIndex("name")],
				Kind: "function",
				File: path,
				Line: ln.NewLineNo,
			}
			if strings.TrimSpace(m[funcHeaderRe.SubexpIndex("recv")]) != "" {
				sym.Kind = "method"
			}
			fillFuncBody(&sym, h.Lines, i)
			out = append(out, sym)
			continue
		}
		if m := typeHeaderRe.FindStringSubmatch(text); m != nil {
			out = append(out, addedSymbol{
				Name: m[typeHeaderRe.SubexpIndex("name")],
				Kind: "type",
				File: path,
				Line: ln.NewLineNo,
			})
		}
	}
	return out
}

// fillFuncBody, given the index of a func's header line, gathers the contiguous
// added lines of the whole declaration (signature through the closing brace at
// the header's indentation). It sets fullyAdded and, when the body is a single
// statement, bodyStmt — the input to the thin-wrapper check. A non-added line
// encountered before the closing brace means the declaration is only partly new
// (an edit, not a fresh function), so the body is left unanalysed.
func fillFuncBody(sym *addedSymbol, lines []Line, start int) {
	var block []string
	openSeen := false
	depth := 0
	complete := false
	for i := start; i < len(lines); i++ {
		if lines[i].Kind != Added {
			return // body not fully within the added run — leave unanalysed
		}
		text := lines[i].Text
		block = append(block, text)
		for _, r := range text {
			switch r {
			case '{':
				depth++
				openSeen = true
			case '}':
				depth--
			}
		}
		if openSeen && depth <= 0 {
			complete = true
			break
		}
	}
	if !complete {
		return
	}
	sym.fullyAdded = true
	sym.signature, sym.params = splitSignature(block)
	sym.bodyStmt = singleBodyStatement(block)
}

// splitSignature joins the block up to the first opening brace and extracts the
// parameter names from the outermost parameter list. Returns the joined
// signature and the ordered parameter names.
func splitSignature(block []string) (sig string, params []string) {
	joined := strings.Join(block, " ")
	brace := strings.IndexByte(joined, '{')
	if brace < 0 {
		return strings.TrimSpace(joined), nil
	}
	sig = strings.TrimSpace(joined[:brace])
	return sig, paramNames(sig)
}

// paramNames extracts the parameter names from a func signature. It finds the
// first top-level parenthesised group (the parameter list, after any receiver
// which the header regex already separated) and returns the leading identifier
// of each comma-separated segment. Grouped params ("a, b int") and variadics
// ("xs ...T") resolve to their names; "_" is preserved.
func paramNames(sig string) []string {
	open := strings.IndexByte(sig, '(')
	if open < 0 {
		return nil
	}
	depth := 0
	end := -1
	for i := open; i < len(sig); i++ {
		switch sig[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				end = i
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		return nil
	}
	inner := sig[open+1 : end]
	if strings.TrimSpace(inner) == "" {
		return nil
	}
	var names []string
	for _, seg := range splitTopLevel(inner) {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		names = append(names, firstIdent(seg))
	}
	return names
}

// singleBodyStatement returns the sole statement of a function body, or "" when
// the body is empty or has more than one statement. Comments and blank lines
// are ignored. The body is the content between the first '{' and the final '}'.
func singleBodyStatement(block []string) string {
	joined := strings.Join(block, "\n")
	open := strings.IndexByte(joined, '{')
	closeIdx := strings.LastIndexByte(joined, '}')
	if open < 0 || closeIdx <= open {
		return ""
	}
	body := joined[open+1 : closeIdx]
	var stmts []string
	for _, raw := range strings.Split(body, "\n") {
		s := stripLineComment(strings.TrimSpace(raw))
		if s == "" {
			continue
		}
		stmts = append(stmts, s)
	}
	if len(stmts) != 1 {
		return ""
	}
	return stmts[0]
}

// splitTopLevel splits s on commas that sit outside any bracket nesting, so a
// generic type argument or a func-typed parameter is not split mid-type.
func splitTopLevel(s string) []string {
	var out []string
	depth := 0
	last := 0
	for i, r := range s {
		switch r {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[last:i])
				last = i + 1
			}
		}
	}
	out = append(out, s[last:])
	return out
}

// firstIdent returns the leading identifier of a param segment ("ctx" from
// "ctx context.Context", "xs" from "xs ...T").
func firstIdent(seg string) string {
	for i, r := range seg {
		if r == ' ' || r == '\t' {
			return seg[:i]
		}
	}
	return seg
}

// stripLineComment removes a trailing // comment when it is not inside a string
// literal. Conservative: a // preceded by an unclosed quote is left alone.
func stripLineComment(s string) string {
	inStr := false
	var quote rune
	for i, r := range s {
		if inStr {
			if r == quote {
				inStr = false
			}
			continue
		}
		switch r {
		case '"', '\'', '`':
			inStr = true
			quote = r
		case '/':
			if i+1 < len(s) && s[i+1] == '/' {
				return strings.TrimSpace(s[:i])
			}
		}
	}
	return s
}

// isGoFile reports whether path is a Go source file.
func isGoFile(path string) bool { return strings.HasSuffix(path, ".go") }

// isGoTestFile reports whether path is a Go test file.
func isGoTestFile(path string) bool { return strings.HasSuffix(path, "_test.go") }
