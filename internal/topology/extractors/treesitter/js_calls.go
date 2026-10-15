package treesitter

// js_calls.go — the call sites and import bindings the module resolver
// (internal/topology, indexer_calls_modules.go) needs to follow a TypeScript or
// JavaScript call across a file boundary, shared by both walkers. Text only, as
// for Python: what each call names (`f()`, `m.f()`, `this.f()`, `new C()`), and
// what each import binds (`import {f as g} from "./a"` binds g to f in "./a").

import (
	"strings"

	tsg "github.com/odvcencio/gotreesitter"

	"github.com/plumbkit/plumb/internal/topology"
)

// jsSites collects one file's sites. Scopes are found by the declaration's start
// byte, not its name, so two classes' same-named methods keep their own calls.
type jsSites struct {
	lang    *tsg.Language
	src     []byte
	byStart map[uint32]int64
	sites   []topology.CallSite
}

func newJSSites(lang *tsg.Language, src []byte) *jsSites {
	return &jsSites{lang: lang, src: src, byStart: map[uint32]int64{}}
}

// register records that the declaration starting at n is node idx.
func (s *jsSites) register(n *tsg.Node, idx int64) { s.byStart[n.StartByte()] = idx }

// enclosing is walkCallSites' scope function: a function, a method, or a
// declarator bound to a function value opens the scope of the node it was
// registered as.
func (s *jsSites) enclosing(n *tsg.Node, cur int64) int64 {
	switch n.Type(s.lang) {
	case "function_declaration", "generator_function_declaration", "method_definition", "variable_declarator",
		"field_definition", "public_field_definition":
		if idx, ok := s.byStart[n.StartByte()]; ok {
			return idx
		}
	}
	return cur
}

// addCall records a call or a `new` inside a function; one outside any function
// has no caller node and is not recorded.
func (s *jsSites) addCall(n *tsg.Node, cur int64) {
	if cur < 0 {
		return
	}
	var fn *tsg.Node
	switch n.Type(s.lang) {
	case "call_expression":
		fn = n.ChildByFieldName("function", s.lang)
	case "new_expression":
		fn = n.ChildByFieldName("constructor", s.lang)
	default:
		return
	}
	if fn == nil {
		return
	}
	site := topology.CallSite{
		EnclosingIdx: int(cur), Kind: topology.CallSiteCall,
		StartByte: clampU32(n.StartByte()), StartLine: line(n.StartPoint()),
	}
	switch fn.Type(s.lang) {
	case "identifier":
		site.Callee = fn.Text(s.src)
	case "member_expression":
		prop := fn.ChildByFieldName("property", s.lang)
		if prop == nil {
			return
		}
		site.Callee = prop.Text(s.src)
		site.Qualifier = s.chain(fn.ChildByFieldName("object", s.lang))
	default:
		return
	}
	if site.Callee != "" {
		s.sites = append(s.sites, site)
	}
}

// chain is the text of a plain name chain (`this`, `api.users`), or
// topology.QualifierOpaque for anything else.
func (s *jsSites) chain(n *tsg.Node) string {
	if n == nil {
		return topology.QualifierOpaque
	}
	switch n.Type(s.lang) {
	case "identifier", "this":
		return n.Text(s.src)
	case "member_expression":
		obj := s.chain(n.ChildByFieldName("object", s.lang))
		prop := n.ChildByFieldName("property", s.lang)
		if obj == topology.QualifierOpaque || prop == nil {
			return topology.QualifierOpaque
		}
		return obj + "." + prop.Text(s.src)
	}
	return topology.QualifierOpaque
}

// addImportBindings records what an import statement binds from spec: a default
// import (imported "default"), a namespace import (imported "*") and each named
// specifier (imported its exported name, bound to its alias when it has one).
func (s *jsSites) addImportBindings(stmt *tsg.Node, spec string) {
	clause := childByType(stmt, "import_clause", s.lang)
	if clause == nil || spec == "" {
		return
	}
	for _, c := range clause.Children() {
		switch c.Type(s.lang) {
		case "identifier":
			s.addBinding(stmt, c.Text(s.src), spec, "default")
		case "namespace_import":
			if id := childByType(c, "identifier", s.lang); id != nil {
				s.addBinding(stmt, id.Text(s.src), spec, "*")
			}
		case "named_imports":
			for _, sp := range c.Children() {
				if sp.Type(s.lang) != "import_specifier" {
					continue
				}
				name := sp.ChildByFieldName("name", s.lang)
				if name == nil {
					continue
				}
				local := name
				if al := sp.ChildByFieldName("alias", s.lang); al != nil {
					local = al
				}
				s.addBinding(stmt, local.Text(s.src), spec, name.Text(s.src))
			}
		}
	}
}

// addRequireBinding records `const m = require("./a")`, a whole-module binding.
func (s *jsSites) addRequireBinding(declarator *tsg.Node, local, spec string) {
	s.addBinding(declarator, local, spec, "")
}

func (s *jsSites) addBinding(at *tsg.Node, local, spec, imported string) {
	local, spec = strings.TrimSpace(local), strings.TrimSpace(spec)
	if local == "" || spec == "" {
		return
	}
	s.sites = append(s.sites, topology.CallSite{
		EnclosingIdx: -1, Kind: topology.CallSiteBinding, Callee: local, Qualifier: spec,
		StartByte: clampU32(at.StartByte()), StartLine: line(at.StartPoint()),
		FirstStringArg: imported, HasStringArg: imported != "",
	})
}
