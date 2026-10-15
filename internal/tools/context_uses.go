package tools

import (
	"context"
	"fmt"
	"go/scanner"
	"go/token"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/plumbkit/plumb/internal/topology"
)

// context_uses.go — the package-level constants and variables a delivered body
// names, when they are declared in the body's own file.
//
// A body that reads `len(oldData) > maxFindReplaceDiffBytes` or
// `r.db.Prepare(prevStateSQL)` sends its reader looking for the value, and the
// pack held only the use. The declarations sit in the file the body was sliced
// from, so they come from the same snapshot, with no second read and no index
// span to trust. Go only: the Go extractor emits a node for each package-level
// const and var name and none for locals, so a name it lists is never a local
// of some other function.

const (
	// contextUsesMax bounds the declarations listed under one body; the rest are
	// counted.
	contextUsesMax = 8
	// contextUsesLines bounds the lines one declaration may span. A longer one
	// (a large table, a long query) is shown by its first line and a handoff.
	contextUsesLines = 12
)

// usedDecl is one same-file constant or variable a body names.
type usedDecl struct {
	Name       string
	Kind       topology.NodeKind // KindConstant or KindVariable
	Start, End int               // 1-based inclusive lines in the snapshot
	Text       string            // the declaration's lines, newline-terminated; only the first when Cut
	Cut        bool              // the declaration runs past contextUsesLines
}

// goIdentifier matches a Go (or Python) identifier together with the byte before
// it, so a selector (x.name) and a qualified name (pkg.name) can be told from a
// bare use. jsIdentifier is the same for TypeScript and JavaScript, whose
// identifiers may hold a $.
var (
	goIdentifier = regexp.MustCompile(`(^|[^.\w])([A-Za-z_]\w*)`)
	jsIdentifier = regexp.MustCompile(`(^|[^.\w$])([A-Za-z_$][\w$]*)`)
)

// usesLanguage reports whether the uses record serves lang: Go, and the
// languages the module resolver serves.
func usesLanguage(lang string) bool {
	switch lang {
	case "go", "python", "typescript", "javascript":
		return true
	}
	return false
}

// bareNames are the identifiers text uses on their own, not after a dot.
func bareNames(text, lang string) map[string]bool {
	re := goIdentifier
	if lang == "typescript" || lang == "javascript" {
		re = jsIdentifier
	}
	names := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(text, -1) {
		names[m[2]] = true
	}
	return names
}

// shadows reports whether text declares name itself, so its uses there are of
// the local and not the file's name. Go: := (name := …, a, name := …) or var.
// Python: an assignment statement to it (name = …, name: T = …, at the start of
// a line, so a keyword argument f(name=…) is not one) or a for target. TypeScript
// and JavaScript: let, const or var. Parameters and destructuring are not
// detected.
func shadows(text, name, lang string) bool {
	q := regexp.QuoteMeta(name)
	var pattern string
	switch lang {
	case "python":
		if pythonAssigns(text, q) {
			return true
		}
		pattern = `\bfor\s+` + q + `\b`
	case "typescript", "javascript":
		pattern = `\b(let|const|var)\s+` + q + `\b`
	default:
		pattern = `\b` + q + `\s*(,\s*\w+\s*)*:=|\bvar\s+` + q + `\b`
	}
	return regexp.MustCompile(pattern).MatchString(text)
}

// pythonAssigns reports whether text has an assignment statement to the quoted
// name: one at the start of a line outside any bracket, so a keyword argument
// (f(name=…), or a call split over lines in Black's style) is not one. Brackets
// inside strings are not told apart; a string with an unbalanced bracket can
// only hide an assignment, which keeps a use listed, never drop one wrongly.
func pythonAssigns(text, quoted string) bool {
	assign := regexp.MustCompile(`^[ \t]*` + quoted + `\s*(:[^=\n]*)?=[^=]`)
	depth := 0
	for _, line := range strings.Split(text, "\n") {
		if depth == 0 && assign.MatchString(line) {
			return true
		}
		for _, r := range line {
			switch r {
			case '(', '[', '{':
				depth++
			case ')', ']', '}':
				if depth > 0 {
					depth--
				}
			}
		}
	}
	return false
}

// uses lists, in file order, at most contextUsesMax package-level constants and
// variables of the snapshot that body b names, other than those declared inside
// the body's own span (a constant seed is not its own use), and counts the rest.
func (s *bodySource) uses(ctx context.Context, b contextBody, seed contextSeed) (out []usedDecl, more int) {
	if !usesLanguage(seed.Language) || b.State != bodyReady {
		return nil, 0
	}
	nodes, ok := s.freshNodes(ctx)
	if !ok {
		return nil, 0
	}
	named := bareNames(b.Text, seed.Language)
	members := memberSpans(nodes, seed.Language)
	// One file declares a package-level name once (the blank _ aside, which
	// usedBy excludes), so no name can be listed twice.
	for _, n := range nodes {
		if !usedBy(n, b, named, seed.Language) || insideSpan(n, members) {
			continue
		}
		if len(out) == contextUsesMax {
			more++
			continue
		}
		if d, ok := s.declLines(n, seed.Language); ok {
			out = append(out, d)
		}
	}
	return out, more
}

// memberSpans are the line spans of the classes and types in a Python or
// TypeScript/JavaScript file: a binding inside one is a class attribute or an enum
// member, named through its owner, not a file-level declaration. Go records no
// such bindings, so it has none.
func memberSpans(nodes []topology.Node, lang string) [][2]int {
	if lang == "go" {
		return nil
	}
	var spans [][2]int
	for _, n := range nodes {
		if n.Kind == topology.KindClass || n.Kind == topology.KindType {
			spans = append(spans, [2]int{n.StartLine, n.EndLine})
		}
	}
	return spans
}

func insideSpan(n topology.Node, spans [][2]int) bool {
	for _, s := range spans {
		if n.StartLine > s[0] && n.StartLine <= s[1] {
			return true
		}
	}
	return false
}

// usedBy reports whether n is a package-level constant or variable body b names
// on its own: not declared inside b's span, and not shadowed by a declaration of
// the same name in b.
func usedBy(n topology.Node, b contextBody, named map[string]bool, lang string) bool {
	if n.Kind != topology.KindConstant && n.Kind != topology.KindVariable {
		return false
	}
	if n.Name == "_" || !named[n.Name] {
		return false
	}
	if n.StartLine >= b.Start && n.StartLine <= b.End {
		return false
	}
	return !shadows(b.Text, n.Name, lang)
}

// declLines takes n's declaration from its name's line to the end of its spec.
// For Go the index records a value spec's name line only, so the end is found in
// the snapshot itself (declEnd); the other languages' extractors record the
// whole statement, so its end is the node's own. A spec longer than
// contextUsesLines is its first line, marked Cut; one that is not valid UTF-8 is
// not offered at all, as a body is not.
func (s *bodySource) declLines(n topology.Node, lang string) (usedDecl, bool) {
	d := usedDecl{Name: n.Name, Kind: n.Kind, Start: n.StartLine, End: n.StartLine}
	switch {
	case lang != "go" && n.EndLine >= n.StartLine && n.EndLine-n.StartLine < contextUsesLines:
		d.End = n.EndLine
	case lang != "go":
		d.Cut = true
	default:
		if end, ok := declEnd(s.lines, n.StartLine); ok {
			d.End = end
		} else {
			d.Cut = true
		}
	}
	text, ok := sliceLines(s.lines, d.Start, d.End)
	if !ok || !utf8.ValidString(text) {
		return usedDecl{}, false
	}
	d.Text = text
	return d, true
}

// declEnd is the line on which the Go value spec that starts on line start ends:
// the first statement end (an explicit or inserted semicolon) outside brackets,
// as Go's own scanner finds it, so a continuation (a trailing operator or comma),
// a raw string, a rune and a comment are all read as Go reads them. ok is false
// when the spec does not end within contextUsesLines lines.
func declEnd(lines []string, start int) (int, bool) {
	if start < 1 || start > len(lines) {
		return 0, false
	}
	stop := min(len(lines), start-1+contextUsesLines)
	src := []byte(strings.Join(lines[start-1:stop], "\n") + "\n")
	file := token.NewFileSet().AddFile("", -1, len(src))
	var sc scanner.Scanner
	broken := false // e.g. a raw string the window cuts: the scanner still ends it with a semicolon
	sc.Init(file, src, func(token.Position, string) { broken = true }, 0)
	depth := 0
	for {
		pos, tok, _ := sc.Scan()
		if broken {
			return 0, false
		}
		switch tok {
		case token.EOF:
			return 0, false
		case token.LPAREN, token.LBRACK, token.LBRACE:
			depth++
		case token.RPAREN, token.RBRACK, token.RBRACE:
			if depth--; depth < 0 {
				// The spec's enclosing block closes on its own line: `x = 1)`.
				return start - 1 + file.Line(pos), true
			}
		case token.SEMICOLON:
			if depth == 0 {
				return start - 1 + file.Line(pos), true
			}
		}
	}
}

// usesLine is the record that follows a delivered body: the declarations it
// uses, in their own lines when that fits the budget, else their names and line
// numbers. It ranks below every best-ranked related declaration, and it needs
// its body (body): the packer drops it unless that body is delivered complete.
// A body the caller holds (have), or one that is not here, gets none.
func (p *contextPack) usesLine(b contextBody, body *bodyUnit, section int) (packLine, bool) {
	if b.State != bodyReady || b.Held || len(b.Uses) == 0 || body == nil {
		return packLine{}, false
	}
	names := make([]string, len(b.Uses))
	var blocks []string
	for i, d := range b.Uses {
		lines := fmt.Sprintf("line %d", d.Start)
		if d.End > d.Start {
			lines = fmt.Sprintf("lines %d–%d", d.Start, d.End)
		}
		names[i] = fmt.Sprintf("%s (%s, %s)", d.Name, d.Kind, lines)
		block := strings.TrimSuffix(withLineGutter(d.Text, d.Start), "\n")
		if d.Cut {
			block += fmt.Sprintf("\n    … longer than %d lines: %s", contextUsesLines,
				callText("read_symbol", "path", p.Files[b.File].Abs, "name", d.Name))
		}
		blocks = append(blocks, block)
	}
	head := "    uses, declared in this file: " + strings.Join(names, ", ")
	if b.UsesMore > 0 {
		head += fmt.Sprintf(", and %d more not listed", b.UsesMore)
	}
	full := head + "\n" + strings.Join(blocks, "\n")
	return packLine{text: full, alts: []string{head}, prio: prioUses, section: section, class: classUses, needs: body}, true
}
