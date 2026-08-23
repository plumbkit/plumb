package golang

import (
	"go/ast"
	"go/token"
	"strconv"

	"github.com/plumbkit/plumb/internal/topology"
)

// This file records raw call sites. It is deliberately separate from the edge
// walk in extractor.go: that walk answers "which intra-file call can I turn into
// an edge right now" and drops everything else, while this one answers "what was
// literally called here", including the calls no single-file pass can resolve.
//
// Two shapes the edge walk never sees are captured here on purpose:
//
//   - Package-level sites. callEdgesFor descends fn.Body only, so a call in a
//     package-level initialiser — `var _ = mux.HandleFunc("/x", h)` — is invisible
//     to it. Those are exactly the registration calls a route or command tree is
//     built from.
//   - Composite-literal field values. `&cobra.Command{Use: "serve"}` contains no
//     call at all, yet the command's name lives in it. A table that records only
//     call expressions cannot answer where a route string came from.

// siteCollector accumulates call sites during one file's walk.
type siteCollector struct {
	fset  *token.FileSet
	sites []topology.CallSite
}

// collectBody walks a function body. It takes the CONCRETE *ast.BlockStmt so a
// bodyless declaration — an assembly or cgo stub, `func Sqrt(float64) float64`
// — is caught here: passed as an ast.Node it would be a non-nil interface
// wrapping a nil pointer, sail past a `root == nil` check, and panic inside
// ast.Walk on the first field access.
func (c *siteCollector) collectBody(body *ast.BlockStmt, enclosing int) {
	if body == nil {
		return
	}
	c.collect(body, enclosing)
}

// collect walks root, recording every call expression and composite-literal
// field value under it as belonging to the declaration at node index enclosing
// (-1 when there is none).
func (c *siteCollector) collect(root ast.Node, enclosing int) {
	if root == nil || c == nil {
		return
	}
	ast.Inspect(root, func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.CallExpr:
			c.addCall(e, enclosing)
		case *ast.CompositeLit:
			c.addLiteralFields(e, enclosing)
		}
		return true
	})
}

// collectValueSpec walks a package-level (or local) value specification's
// initialisers. baseIdx is the node index of the spec's first name; names are
// paired with values positionally when the counts agree, and otherwise all
// values are attributed to the first name — `var a, b = f()` has one initialiser
// for two names and no per-name attribution exists to be had.
func (c *siteCollector) collectValueSpec(s *ast.ValueSpec, baseIdx, nameCount int) {
	if c == nil {
		return
	}
	paired := len(s.Values) == nameCount
	for i, v := range s.Values {
		enclosing := baseIdx
		if paired {
			enclosing = baseIdx + i
		}
		c.collect(v, enclosing)
	}
}

func (c *siteCollector) addCall(call *ast.CallExpr, enclosing int) {
	callee, qualifier := calleeParts(call.Fun)
	if callee == "" {
		// A conversion (`[]byte(s)`), a call on a function literal, or a call on
		// an expression with no nameable callee. Recording it with an empty callee
		// would put a row in the table that no resolver could ever match and that
		// no consumer could interpret.
		return
	}
	pos := c.fset.Position(call.Pos())
	site := topology.CallSite{
		EnclosingIdx: enclosing,
		Kind:         topology.CallSiteCall,
		Callee:       callee,
		Qualifier:    qualifier,
		StartByte:    pos.Offset,
		StartLine:    pos.Line,
		ArgCount:     len(call.Args),
		ArgSpread:    call.Ellipsis.IsValid(),
	}
	for _, arg := range call.Args {
		if lit, ok := stringLit(arg); ok && !site.HasStringArg {
			site.FirstStringArg, site.HasStringArg = lit, true
		}
		if name := identText(arg); name != "" && len(site.ArgIdents) < topology.MaxCallSiteArgIdents {
			site.ArgIdents = append(site.ArgIdents, name)
		}
	}
	c.sites = append(c.sites, site)
}

// addLiteralFields records the keyed fields of a composite literal whose type is
// nameable. An unkeyed literal (`[]string{"a"}`) has no field names, and a
// literal with no written type (a nested `{...}` inside a slice of structs) has
// no type text to attribute the field to — both are skipped rather than recorded
// with a blank qualifier that would resolve to anything.
func (c *siteCollector) addLiteralFields(lit *ast.CompositeLit, enclosing int) {
	typeText := exprText(lit.Type)
	if typeText == "" {
		return
	}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		pos := c.fset.Position(kv.Pos())
		site := topology.CallSite{
			EnclosingIdx: enclosing,
			Kind:         topology.CallSiteField,
			Callee:       key.Name,
			Qualifier:    typeText,
			StartByte:    pos.Offset,
			StartLine:    pos.Line,
			ArgCount:     1,
		}
		if s, ok := stringLit(kv.Value); ok {
			site.FirstStringArg, site.HasStringArg = s, true
		}
		if name := identText(kv.Value); name != "" {
			site.ArgIdents = []string{name}
		}
		c.sites = append(c.sites, site)
	}
}

// calleeParts splits a call's function expression into the called name and the
// qualifier to its left. The qualifier is the half calleeIdent throws away, and
// throwing it away is why every `pkg.Fn` in the index is indistinguishable from
// every other `Fn`.
func calleeParts(expr ast.Expr) (callee, qualifier string) {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name, ""
	case *ast.SelectorExpr:
		return e.Sel.Name, exprText(e.X)
	case *ast.ParenExpr:
		return calleeParts(e.X)
	case *ast.IndexExpr:
		// A generic instantiation: `New[T](x)`.
		return calleeParts(e.X)
	case *ast.IndexListExpr:
		return calleeParts(e.X)
	default:
		return "", ""
	}
}

// exprText renders the dotted-name spelling of an expression, or "" when the
// expression is not a plain name chain. A star (`*pkg.T`) is unwrapped so a
// pointer composite literal's type reads the same as a value one's.
func exprText(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		if x := exprText(e.X); x != "" {
			return x + "." + e.Sel.Name
		}
		return ""
	case *ast.StarExpr:
		return exprText(e.X)
	case *ast.ParenExpr:
		return exprText(e.X)
	case *ast.IndexExpr:
		return exprText(e.X)
	case *ast.IndexListExpr:
		return exprText(e.X)
	default:
		return ""
	}
}

// identText is exprText restricted to argument position: a bare or dotted name.
func identText(expr ast.Expr) string {
	switch expr.(type) {
	case *ast.Ident, *ast.SelectorExpr:
		return exprText(expr)
	default:
		return ""
	}
}

// stringLit reports an expression's value when it is a string literal. The
// second result separates "no string literal" from a literal whose value is the
// empty string.
func stringLit(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return v, true
}
