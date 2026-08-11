package treesitter

import (
	"context"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

var luaSrc = []byte(`local json = require("json")
local http = require "http"

-- Greet a person by name.
function greet(name)
  return "hi " .. name
end

local function helper(a, b)
  local tmp = a
  local anon = function(x) return x end
  return tmp
end

local M = {}

--[[ Build a widget from opts. ]]
function M.build(opts)
  return helper(opts, 1)
end

function Account:deposit(amount)
  self.balance = self.balance + amount
end

local Util = {
  slug = function(s) return s end,
  name = "util",
}

describe("greet", function()
  it("says hi", function()
    greet("world")
  end)
end)

function trailing_helper()
  return 1
end
`)

func luaExtract(t *testing.T) ([]topology.Node, []topology.Edge) {
	t.Helper()
	nodes, edges, err := NewLua().Extract(context.Background(), "m.lua", luaSrc)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return nodes, edges
}

func luaNamed(nodes []topology.Node, name string) (topology.Node, bool) {
	for _, n := range nodes {
		if n.Name == name {
			return n, true
		}
	}
	return topology.Node{}, false
}

// Lua declares a function four ways and an extractor that handles only the
// first misses most of a real module.
func TestLua_AllDeclarationFormsExtracted(t *testing.T) {
	nodes, _ := luaExtract(t)

	want := []struct {
		name      string
		qualified string
		kind      topology.NodeKind
	}{
		{"greet", "greet", topology.KindFunction},           // global
		{"helper", "helper", topology.KindFunction},         // local function
		{"build", "M.build", topology.KindFunction},         // field on a module table
		{"deposit", "Account:deposit", topology.KindMethod}, // method, implicit self
		{"slug", "Util.slug", topology.KindFunction},        // table literal
		{"anon", "anon", topology.KindFunction},             // bound anonymous function
		{"trailing_helper", "trailing_helper", topology.KindFunction},
	}
	for _, w := range want {
		n, ok := luaNamed(nodes, w.name)
		if !ok {
			t.Errorf("missing %q", w.name)
			continue
		}
		if n.Kind != w.kind {
			t.Errorf("%q: kind = %q, want %q", w.name, n.Kind, w.kind)
		}
		if n.Qualified != w.qualified {
			t.Errorf("%q: qualified = %q, want %q", w.name, n.Qualified, w.qualified)
		}
	}
}

// The colon form receives an implicit self and the dot form does not. Lua's own
// syntax draws that line, so the qualified name keeps the punctuation.
func TestLua_ColonFormIsAMethodAndKeepsItsPunctuation(t *testing.T) {
	nodes, _ := luaExtract(t)

	m, ok := luaNamed(nodes, "deposit")
	if !ok {
		t.Fatal("Account:deposit not extracted")
	}
	if m.Kind != topology.KindMethod {
		t.Errorf("kind = %q, want %q", m.Kind, topology.KindMethod)
	}
	if !strings.Contains(m.Qualified, ":") {
		t.Errorf("qualified = %q; the colon is what tells a reader it takes self", m.Qualified)
	}

	f, ok := luaNamed(nodes, "build")
	if !ok {
		t.Fatal("M.build not extracted")
	}
	if f.Kind != topology.KindFunction {
		t.Errorf("dot form kind = %q, want %q", f.Kind, topology.KindFunction)
	}
}

// A Lua file is mostly locals; they carry no structure and would bury the
// functions.
func TestLua_PlainLocalsAreSuppressed(t *testing.T) {
	nodes, _ := luaExtract(t)

	for _, name := range []string{"tmp", "M", "Util", "a", "b"} {
		if _, ok := luaNamed(nodes, name); ok {
			t.Errorf("%q is a plain variable and should not be emitted", name)
		}
	}
	// But a function bound to a local name is still a callable.
	if _, ok := luaNamed(nodes, "anon"); !ok {
		t.Error("a function bound to a local name should be emitted")
	}
}

// `return { … }` is one of the two standard module shapes and the table is
// never bound to a name, so nothing else in the walk reaches it. Missing this
// cost 2.8 points of function recall on a real 1,501-file corpus.
func TestLua_ReturnedTableModuleIsExtracted(t *testing.T) {
	src := []byte("return {\n  go = function(x) return x end,\n  stop = function() end,\n}\n")

	nodes, _, err := NewLua().Extract(context.Background(), "mod.lua", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, want := range []string{"go", "stop"} {
		n, ok := luaNamed(nodes, want)
		if !ok {
			t.Errorf("%q missing from a returned-table module", want)
			continue
		}
		if n.Kind != topology.KindFunction {
			t.Errorf("%q: kind = %q, want %q", want, n.Kind, topology.KindFunction)
		}
	}
}

// A table of tables is how a plugin groups related helpers, and the nesting
// belongs in the qualified name.
func TestLua_NestedTableFunctionsKeepTheirPath(t *testing.T) {
	src := []byte("local M = {\n  sub = {\n    deep = function() end,\n  },\n}\nreturn M\n")

	nodes, _, err := NewLua().Extract(context.Background(), "a.lua", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	n, ok := luaNamed(nodes, "deep")
	if !ok {
		t.Fatal("a function nested in a table of tables was lost")
	}
	if got, want := n.Qualified, "M.sub.deep"; got != want {
		t.Errorf("qualified = %q, want %q", got, want)
	}
}

func TestLua_RequireBecomesAnImport(t *testing.T) {
	nodes, _ := luaExtract(t)

	for _, mod := range []string{"json", "http"} {
		n, ok := luaNamed(nodes, mod)
		if !ok {
			t.Errorf("require(%q) was not recorded", mod)
			continue
		}
		if n.Kind != topology.KindImport {
			t.Errorf("%q: kind = %q, want %q", mod, n.Kind, topology.KindImport)
		}
	}
}

// A module required inside a function body is still a dependency, and Lua code
// requires lazily all the time.
func TestLua_RequireInsideAFunctionIsStillAnImport(t *testing.T) {
	src := []byte("local function load()\n  local m = require(\"deep.mod\")\n  return m\nend\n")

	nodes, _, err := NewLua().Extract(context.Background(), "a.lua", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if _, ok := luaNamed(nodes, "deep.mod"); !ok {
		t.Error("a lazily-required module should still be recorded as an import")
	}
}

func TestLua_CallEdgeLinksCallerToCallee(t *testing.T) {
	nodes, edges := luaExtract(t)

	var from, to int64 = -1, -1
	for i, n := range nodes {
		switch n.Name {
		case "build":
			from = int64(i)
		case "helper":
			to = int64(i)
		}
	}
	if from < 0 || to < 0 {
		t.Fatalf("caller/callee not extracted: from=%d to=%d", from, to)
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeCalls && e.FromID == from && e.ToID == to {
			if e.Confidence <= 0 || e.Source == "" {
				t.Errorf("call edge is unannotated: %+v", e)
			}
			return
		}
	}
	t.Error("no call edge from M.build to helper")
}

// `require` is a call, but it is already recorded as an import; a call edge to
// it would be noise.
func TestLua_RequireIsNotAlsoACallEdge(t *testing.T) {
	_, edges := luaExtract(t)
	nodes, _ := luaExtract(t)

	for _, e := range edges {
		if e.Kind != topology.EdgeCalls {
			continue
		}
		if int(e.ToID) < len(nodes) && nodes[e.ToID].Kind == topology.KindImport {
			t.Errorf("call edge points at an import: %+v", e)
		}
	}
}

func TestLua_BustedSuiteBecomesSectionAndCases(t *testing.T) {
	nodes, edges := luaExtract(t)

	group, ok := luaNamed(nodes, "greet")
	_ = group
	if !ok {
		t.Fatal("describe block not extracted")
	}
	tc, ok := luaNamed(nodes, "says hi")
	if !ok {
		t.Fatal("it case not extracted")
	}
	if tc.Kind != topology.KindTest {
		t.Errorf("case kind = %q, want %q", tc.Kind, topology.KindTest)
	}

	// The case hangs off its describe block.
	var groupIdx, caseIdx int64 = -1, -1
	for i, n := range nodes {
		if n.Kind == topology.KindSection && n.Name == "greet" {
			groupIdx = int64(i)
		}
		if n.Name == "says hi" {
			caseIdx = int64(i)
		}
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeContains && e.FromID == groupIdx && e.ToID == caseIdx {
			return
		}
	}
	t.Error("a test case should be contained by its describe block")
}

func TestLua_XUnitStyleTestFunctionIsATest(t *testing.T) {
	src := []byte("function test_adds()\n  return 1\nend\n\nfunction TestSubtracts()\n  return 2\nend\n")

	nodes, _, err := NewLua().Extract(context.Background(), "spec.lua", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, name := range []string{"test_adds", "TestSubtracts"} {
		n, ok := luaNamed(nodes, name)
		if !ok {
			t.Errorf("%q not extracted", name)
			continue
		}
		if n.Kind != topology.KindTest {
			t.Errorf("%q: kind = %q, want %q", name, n.Kind, topology.KindTest)
		}
	}
}

func TestLua_DocSpanCoversBothCommentSyntaxes(t *testing.T) {
	nodes, _ := luaExtract(t)

	line, ok := luaNamed(nodes, "greet")
	if !ok {
		t.Fatal("greet not extracted")
	}
	if got := string(luaSrc[line.DocStartByte:line.DocEndByte]); !strings.Contains(got, "Greet a person") {
		t.Errorf("doc span = %q, want the -- comment above the function", got)
	}

	block, ok := luaNamed(nodes, "build")
	if !ok {
		t.Fatal("M.build not extracted")
	}
	if got := string(luaSrc[block.DocStartByte:block.DocEndByte]); !strings.Contains(got, "Build a widget") {
		t.Errorf("doc span = %q, want the --[[ ]] comment above the function", got)
	}
}

func TestLua_ByteSpansReconstructTheSource(t *testing.T) {
	nodes, _ := luaExtract(t)

	if len(nodes) == 0 {
		t.Fatal("no nodes extracted")
	}
	for _, n := range nodes {
		if n.StartByte >= n.EndByte {
			t.Errorf("%q: inverted or empty span %d..%d", n.Name, n.StartByte, n.EndByte)
			continue
		}
		if n.EndByte > len(luaSrc) {
			t.Errorf("%q: span %d..%d runs past the source (%d bytes)", n.Name, n.StartByte, n.EndByte, len(luaSrc))
		}
	}

	last, ok := luaNamed(nodes, "trailing_helper")
	if !ok {
		t.Fatal("the declaration nearest EOF was lost, which is what a parse cascade looks like")
	}
	got := string(luaSrc[last.StartByte:last.EndByte])
	if !strings.HasPrefix(got, "function trailing_helper") || !strings.HasSuffix(strings.TrimSpace(got), "end") {
		t.Errorf("span does not reconstruct the function: %q", got)
	}
}

func TestLua_MalformedInputDoesNotPanic(t *testing.T) {
	for _, src := range []string{
		"",
		"   \n\n",
		"-- just a comment\n",
		"function",
		"function f(",
		"local",
		"local x =",
		"end end end",
		"require(",
		"function M.() end",
		"function :m() end",
		strings.Repeat("function f() ", 200),
	} {
		if _, _, err := NewLua().Extract(context.Background(), "a.lua", []byte(src)); err != nil {
			t.Errorf("Extract(%q): unexpected error %v", src, err)
		}
	}
}

func TestLua_LanguageAndPath(t *testing.T) {
	nodes, _ := luaExtract(t)
	if len(nodes) == 0 {
		t.Fatal("no nodes extracted")
	}
	for _, n := range nodes {
		if n.Language != "lua" {
			t.Errorf("%q: language = %q, want lua", n.Name, n.Language)
		}
		if n.Path != "m.lua" {
			t.Errorf("%q: path = %q, want m.lua", n.Name, n.Path)
		}
	}
}

func TestLua_Extensions(t *testing.T) {
	e := NewLua()
	if got := e.Language(); got != "lua" {
		t.Errorf("Language() = %q, want lua", got)
	}
	if got := e.Extensions(); len(got) != 1 || got[0] != ".lua" {
		t.Errorf("Extensions() = %v, want [.lua]", got)
	}
}
