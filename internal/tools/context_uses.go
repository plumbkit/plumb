package tools

import (
	"context"
	"fmt"
	"regexp"
	"strings"

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
	// contextUsesMax bounds the declarations listed under one body.
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

// goIdentifier matches a Go identifier together with the byte before it, so a
// selector (x.name) and a qualified name (pkg.name) can be told from a bare use.
var goIdentifier = regexp.MustCompile(`(^|[^.\w])([A-Za-z_]\w*)`)

// bareNames are the identifiers text uses on their own, not after a dot.
func bareNames(text string) map[string]bool {
	names := map[string]bool{}
	for _, m := range goIdentifier.FindAllStringSubmatch(text, -1) {
		names[m[2]] = true
	}
	return names
}

// shadows reports whether text declares name itself (name := …, a, name := …,
// var name), so its uses there are of the local and not the package's name.
func shadows(text, name string) bool {
	q := regexp.QuoteMeta(name)
	return regexp.MustCompile(`\b` + q + `\s*(,\s*\w+\s*)*:=|\bvar\s+` + q + `\b`).MatchString(text)
}

// uses lists, in file order, the package-level constants and variables of the
// snapshot that body b names. self is the body's own declaration, never listed
// as its own use; nor is anything declared inside the body's span.
func (s *bodySource) uses(ctx context.Context, b contextBody, seed contextSeed) []usedDecl {
	if seed.Language != "go" || b.State != bodyReady {
		return nil
	}
	nodes, ok := s.freshNodes(ctx)
	if !ok {
		return nil
	}
	named := bareNames(b.Text)
	var out []usedDecl
	seen := map[string]bool{}
	for _, n := range nodes {
		if seen[n.Name] || !usedBy(n, b, seed.Name, named) {
			continue
		}
		d, ok := s.declLines(n)
		if !ok {
			continue
		}
		seen[n.Name] = true
		out = append(out, d)
		if len(out) == contextUsesMax {
			break
		}
	}
	return out
}

// usedBy reports whether n is a package-level constant or variable body b names
// on its own: not b's own declaration (self), not declared inside b's span, and
// not shadowed by a declaration of the same name in b.
func usedBy(n topology.Node, b contextBody, self string, named map[string]bool) bool {
	if n.Kind != topology.KindConstant && n.Kind != topology.KindVariable {
		return false
	}
	if n.Name == "_" || n.Name == self || !named[n.Name] {
		return false
	}
	if n.StartLine >= b.Start && n.StartLine <= b.End {
		return false
	}
	return !shadows(b.Text, n.Name)
}

// declLines takes n's declaration from its name's line on: one line, or as many
// as it takes to close what that line opens (a raw string, a bracket), up to
// contextUsesLines. The index records a value spec's name line only, so the end
// is found in the snapshot itself.
func (s *bodySource) declLines(n topology.Node) (usedDecl, bool) {
	d := usedDecl{Name: n.Name, Kind: n.Kind, Start: n.StartLine, End: n.StartLine}
	first, ok := sliceLines(s.lines, n.StartLine, n.StartLine)
	if !ok || !strings.Contains(first, n.Name) {
		return usedDecl{}, false
	}
	depth, raw := 0, false
	for ln := n.StartLine; ; ln++ {
		line, ok := sliceLines(s.lines, ln, ln)
		if !ok {
			d.Cut = true
			break
		}
		depth, raw = scanOpen(line, depth, raw)
		if depth <= 0 && !raw {
			d.End = ln
			break
		}
		if ln-n.StartLine+1 >= contextUsesLines {
			d.Cut = true
			break
		}
	}
	if d.Cut {
		d.End, d.Text = n.StartLine, first
		return d, true
	}
	d.Text, _ = sliceLines(s.lines, d.Start, d.End)
	return d, true
}

// scanOpen carries the bracket depth and raw-string state across one line of Go.
// Interpreted strings, runes and line comments are skipped so their brackets do
// not count.
func scanOpen(line string, depth int, raw bool) (int, bool) {
	for i := 0; i < len(line); i++ {
		c := line[i]
		if raw {
			if c == '`' {
				raw = false
			}
			continue
		}
		switch c {
		case '`':
			raw = true
		case '"', '\'':
			for i++; i < len(line) && line[i] != c; i++ {
				if line[i] == '\\' {
					i++
				}
			}
		case '/':
			if i+1 < len(line) && line[i+1] == '/' {
				return depth, raw
			}
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		}
	}
	return depth, raw
}

// usesLine is the record that follows a delivered body: the declarations it
// uses, in their own lines when that fits the budget, else their names and line
// numbers. A body the caller holds (have), or one that is not here, gets none.
func (p *contextPack) usesLine(b contextBody, prio, section int) (packLine, bool) {
	if b.State != bodyReady || b.Held || len(b.Uses) == 0 {
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
	head := "    uses, declared in this file: "
	full := head + strings.Join(names, ", ") + "\n" + strings.Join(blocks, "\n")
	lean := head + strings.Join(names, ", ")
	return packLine{text: full, alts: []string{lean}, prio: prio, section: section, class: classUses}, true
}
