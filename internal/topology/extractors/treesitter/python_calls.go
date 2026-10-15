package treesitter

// python_calls.go — the call sites and import bindings the module resolver
// (internal/topology, indexer_calls_modules.go) needs to follow a Python call
// across a file boundary. The extractor sees one file, so it records text only:
// what each call names (`helper()`, `mod.helper()`, `self.helper()`), and what
// each import binds (`from .a import f as g` binds g to f in module .a).

import (
	"strings"

	tsg "github.com/odvcencio/gotreesitter"

	"github.com/plumbkit/plumb/internal/topology"
)

// scopeByStart is the enclosing function of a call, found by the definition's
// start byte rather than its name, so two methods that share a name in one file
// (A.run and B.run) each keep their own calls.
func (w *pyWalk) scopeByStart(n *tsg.Node, cur int64) int64 {
	if n.Type(w.lang) != "function_definition" {
		return cur
	}
	if idx, ok := w.funcByStart[n.StartByte()]; ok {
		return idx
	}
	return cur
}

// addCallSite records one call inside a function. A call outside any function
// (module level) has no caller node, so it is not recorded.
func (w *pyWalk) addCallSite(call *tsg.Node, curFunc int64) {
	if curFunc < 0 {
		return
	}
	fn := call.ChildByFieldName("function", w.lang)
	if fn == nil {
		return
	}
	site := topology.CallSite{
		EnclosingIdx: int(curFunc),
		Kind:         topology.CallSiteCall,
		StartByte:    clampU32(call.StartByte()),
		StartLine:    line(call.StartPoint()),
	}
	switch fn.Type(w.lang) {
	case "identifier":
		site.Callee = fn.Text(w.src)
	case "attribute":
		attr := fn.ChildByFieldName("attribute", w.lang)
		if attr == nil {
			return
		}
		site.Callee = attr.Text(w.src)
		site.Qualifier = w.dottedChain(fn.ChildByFieldName("object", w.lang))
	default:
		return
	}
	if site.Callee == "" {
		return
	}
	w.sites = append(w.sites, site)
}

// dottedChain is the text of a plain name chain (`self`, `pkg.mod`), or
// topology.QualifierOpaque for anything else (a call result, a subscript).
func (w *pyWalk) dottedChain(n *tsg.Node) string {
	if n == nil {
		return topology.QualifierOpaque
	}
	switch n.Type(w.lang) {
	case "identifier":
		return n.Text(w.src)
	case "attribute":
		obj := w.dottedChain(n.ChildByFieldName("object", w.lang))
		attr := n.ChildByFieldName("attribute", w.lang)
		if obj == topology.QualifierOpaque || attr == nil {
			return topology.QualifierOpaque
		}
		return obj + "." + attr.Text(w.src)
	}
	return topology.QualifierOpaque
}

// addBindings records what an import statement binds, as CallSiteBinding sites
// (Callee = the local name, Qualifier = the module as written, FirstStringArg =
// the imported name for a from-import). `import a.b` binds the chain "a.b", which
// is how a call through it is qualified; `import a.b as m` binds m; a wildcard
// binds nothing nameable and is skipped.
func (w *pyWalk) addBindings(n *tsg.Node) {
	switch n.Type(w.lang) {
	case "import_statement":
		for i, c := range n.Children() {
			if n.FieldNameForChild(i, w.lang) != "name" {
				continue
			}
			local, module := w.importedName(c)
			w.addBinding(n, local, module, "")
		}
	case "import_from_statement":
		mod := n.ChildByFieldName("module_name", w.lang)
		if mod == nil {
			return
		}
		module := strings.TrimSpace(mod.Text(w.src))
		for i, c := range n.Children() {
			if n.FieldNameForChild(i, w.lang) != "name" {
				continue
			}
			local, name := w.importedName(c)
			w.addBinding(n, local, module, name)
		}
	}
}

// importedName reads a `name` or `name as alias` entry: the local name it binds
// and the name it imports.
func (w *pyWalk) importedName(c *tsg.Node) (local, name string) {
	switch c.Type(w.lang) {
	case "dotted_name":
		name = strings.TrimSpace(c.Text(w.src))
		return name, name
	case "aliased_import":
		if nm := c.ChildByFieldName("name", w.lang); nm != nil {
			name = strings.TrimSpace(nm.Text(w.src))
		}
		if al := c.ChildByFieldName("alias", w.lang); al != nil {
			return strings.TrimSpace(al.Text(w.src)), name
		}
	}
	return "", ""
}

func (w *pyWalk) addBinding(stmt *tsg.Node, local, module, imported string) {
	if local == "" || module == "" {
		return
	}
	w.sites = append(w.sites, topology.CallSite{
		EnclosingIdx:   -1,
		Kind:           topology.CallSiteBinding,
		Callee:         local,
		Qualifier:      module,
		StartByte:      clampU32(stmt.StartByte()),
		StartLine:      line(stmt.StartPoint()),
		FirstStringArg: imported,
		HasStringArg:   imported != "",
	})
}
