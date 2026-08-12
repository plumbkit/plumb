package treesitter

import (
	"context"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

// rbSrc is the shared fixture: idiomatic Ruby covering every construct the walk
// claims to handle, including the ones that only look like declarations
// (attr_accessor, require, RSpec blocks are all ordinary method calls
// syntactically). trailing_helper sits last so fidelityCases can use it to
// detect a grammar cascade that swallows everything after a break.
var rbSrc = []byte(`require 'json'
require_relative '../helper'

# Billing groups the invoicing types.
module Billing
  RATE = 0.1

  # An invoice for a customer.
  class Invoice < Base
    attr_reader :total
    attr_accessor :customer, :issued_at

    def initialize(total)
      @total = total
      subtotal = total * 2
    end

    # Ruby 3.0 endless methods: no parameter list and no body_statement, so the
    # name has to come from the grammar's name field rather than a positional
    # scan that stops at one of those.
    def rate = RATE
    def doubled = formatted(@total)
    def tripled(n) = n * 3

    def self.default = new(0)

    # A def body can talk about another object; nothing it declares there
    # belongs to this class.
    def self.included(base)
      base.attr_accessor :sneaky
    end

    def self.build(total)
      new(total)
    end

    def to_s
      formatted(@total)
    end

    def formatted(value)
      value.round(2).to_s
    end

    def ==(other)
      total == other.total
    end
  end
end

class ReportTest < Minitest::Test
  def test_totals
    assert true
  end

  def not_a_test
    1
  end
end

RSpec.describe Billing::Invoice do
  context "with a total" do
    it "formats the total" do
      expect(1).to eq(1)
    end
  end
end

def trailing_helper(a, b = 1)
  a + b
end
`)

func rbExtract(t *testing.T) ([]topology.Node, []topology.Edge) {
	t.Helper()
	nodes, edges, err := NewRuby().Extract(context.Background(), "app/billing.rb", rbSrc)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return nodes, edges
}

func rbFind(t *testing.T, nodes []topology.Node, kind topology.NodeKind, name string) topology.Node {
	t.Helper()
	for _, n := range nodes {
		if n.Kind == kind && n.Name == name {
			return n
		}
	}
	t.Fatalf("no %s named %q among %d nodes", kind, name, len(nodes))
	return topology.Node{}
}

func TestRuby_KindsExtracted(t *testing.T) {
	nodes, _ := rbExtract(t)
	want := []struct {
		kind topology.NodeKind
		name string
	}{
		{topology.KindType, "Billing"},
		{topology.KindType, "Invoice"},
		{topology.KindConstant, "RATE"},
		// attr_reader publishes no writer (immutable), attr_accessor does
		// (mutable). KindField is reserved for keys of a data-format file.
		{topology.KindConstant, "total"},
		{topology.KindVariable, "customer"},
		{topology.KindVariable, "issued_at"},
		{topology.KindMethod, "initialize"},
		// Ruby 3.0 endless methods, named by themselves and not by their body.
		{topology.KindMethod, "rate"},
		{topology.KindMethod, "doubled"},
		{topology.KindMethod, "tripled"},
		{topology.KindMethod, "default"},
		{topology.KindMethod, "build"},
		{topology.KindMethod, "formatted"},
		{topology.KindMethod, "=="}, // operator methods parse as an operator node, not an identifier
		{topology.KindImport, "json"},
		{topology.KindImport, "../helper"},
		{topology.KindFunction, "trailing_helper"},
	}
	for _, w := range want {
		rbFind(t, nodes, w.kind, w.name)
	}
}

// Ruby marks a constant by capitalisation alone and the grammar gives locals and
// constants the same `assignment` node, so a walk that does not track method
// bodies emits every local variable as a symbol.
//
// `subtotal` alone is a weak witness: it is rejected twice over, by the inMethod
// flag AND by addConstant's node-type check, so deleting the flag leaves it
// green and the guard untested. `sneaky` is the witness that discriminates —
// `base.attr_accessor :sneaky` inside `def self.included` is valid, idiomatic
// Ruby that declares an accessor on *another* object, and only the inMethod flag
// keeps it out of the enclosing class.
func TestRuby_LocalsInsideMethodsAreSuppressed(t *testing.T) {
	nodes, _ := rbExtract(t)
	for _, n := range nodes {
		switch n.Name {
		case "subtotal":
			t.Errorf("local assignment inside a method leaked into the index as %s %q", n.Kind, n.Qualified)
		case "sneaky":
			t.Errorf("attr_accessor inside a method body leaked into the enclosing class as %s %q", n.Kind, n.Qualified)
		}
	}
}

// A constant assignment inside a def is a SyntaxError in Ruby proper (dynamic
// constant assignment), but tree-sitter parses malformed source happily and the
// indexer must not turn one into a symbol. That path is only reachable through
// the inMethod flag, so it is asserted on its own source rather than on rbSrc.
func TestRuby_ConstantAssignmentInsideMethodNotSurfaced(t *testing.T) {
	nodes, _, err := NewRuby().Extract(context.Background(), "c.rb",
		[]byte("class C\n  def m\n    INNER_CONST = 1\n  end\nend\n"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, n := range nodes {
		if n.Name == "INNER_CONST" {
			t.Errorf("constant assignment inside a method leaked into the index as %s %q", n.Kind, n.Qualified)
		}
	}
	// Anti-vacuity: the enclosing declarations are still there.
	rbFind(t, nodes, topology.KindType, "C")
	rbFind(t, nodes, topology.KindMethod, "m")
}

// TestRuby_EndlessMethodNamedByItself pins Ruby 3.0's endless method forms. A
// positional name scan that stopped at the parameter list or the body ran past
// the end of a parameterless endless def and named the method after the first
// token of its body: `def endless = helper_call` was indexed as a method named
// `helper_call` — a symbol that is actually a *call* — while the real `endless`
// never appeared at all, and funcIdx aimed intra-file call edges at the phantom.
//
// The parenthesised form `def f(a) = ...` was never broken, only accidentally
// safe, so it is pinned here too.
func TestRuby_EndlessMethodNamedByItself(t *testing.T) {
	src := []byte(`class C
  RED = 1
  def endless = helper_call
  def color = RED
  def endless2(a) = a + 1
  def self.built = helper_call
  def helper_call
    2
  end
end
`)
	nodes, _, err := NewRuby().Extract(context.Background(), "c.rb", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, want := range []struct{ name, qualified string }{
		{"endless", "C#endless"},
		{"color", "C#color"},
		{"endless2", "C#endless2"},
		{"built", "C.built"},
		{"helper_call", "C#helper_call"},
	} {
		if got := rbFind(t, nodes, topology.KindMethod, want.name).Qualified; got != want.qualified {
			t.Errorf("method %s qualified = %q, want %q", want.name, got, want.qualified)
		}
	}
	// The phantom: exactly one `helper_call` method, the real def. Two endless
	// bodies mention it, and the buggy walk emitted a method for each.
	if n := countNamed(nodes, "helper_call"); n != 1 {
		t.Errorf("helper_call appears %d times; the endless bodies are being named as methods; nodes=%v", n, nodeNames(nodes))
	}
	// `RED` is the class constant, not a method named after `def color`'s body.
	if got := rbFind(t, nodes, topology.KindConstant, "RED").Qualified; got != "C::RED" {
		t.Errorf("RED qualified = %q, want C::RED", got)
	}
	for _, n := range nodes {
		if n.Name == "RED" && n.Kind == topology.KindMethod {
			t.Error("the body of `def color = RED` was emitted as a method")
		}
	}
}

func countNamed(nodes []topology.Node, name string) int {
	n := 0
	for _, node := range nodes {
		if node.Name == name {
			n++
		}
	}
	return n
}

func TestRuby_QualifiedNamesUseRubyNotation(t *testing.T) {
	nodes, _ := rbExtract(t)
	for _, tc := range []struct {
		kind topology.NodeKind
		name string
		want string
	}{
		{topology.KindType, "Invoice", "Billing::Invoice"},
		{topology.KindConstant, "RATE", "Billing::RATE"},
		// An instance method takes '#', a singleton method '.', which is what a
		// Ruby backtrace prints and therefore what a search for it contains.
		{topology.KindMethod, "formatted", "Billing::Invoice#formatted"},
		{topology.KindMethod, "build", "Billing::Invoice.build"},
		{topology.KindVariable, "customer", "Billing::Invoice#customer"},
		{topology.KindMethod, "rate", "Billing::Invoice#rate"},
		{topology.KindMethod, "default", "Billing::Invoice.default"},
	} {
		if got := rbFind(t, nodes, tc.kind, tc.name).Qualified; got != tc.want {
			t.Errorf("%s %s qualified = %q, want %q", tc.kind, tc.name, got, tc.want)
		}
	}
}

func TestRuby_ContainmentEdgeCertain(t *testing.T) {
	nodes, edges := rbExtract(t)
	idxOf := func(kind topology.NodeKind, name string) int64 {
		for i, n := range nodes {
			if n.Kind == kind && n.Name == name {
				return int64(i)
			}
		}
		t.Fatalf("missing %s %q", kind, name)
		return -1
	}
	invoice, formatted := idxOf(topology.KindType, "Invoice"), idxOf(topology.KindMethod, "formatted")
	for _, e := range edges {
		if e.Kind == topology.EdgeContains && e.FromID == invoice && e.ToID == formatted {
			if e.Confidence != 1.0 {
				t.Errorf("containment confidence = %v, want 1.0 (syntactically certain)", e.Confidence)
			}
			if e.Source != "extractor" {
				t.Errorf("containment source = %q, want \"extractor\"", e.Source)
			}
			return
		}
	}
	t.Error("no containment edge from class Invoice to its method formatted")
}

// Nesting must survive a level: Billing contains Invoice, not just its methods.
func TestRuby_NestedModuleContainsClass(t *testing.T) {
	nodes, edges := rbExtract(t)
	var billing, invoice int64 = -1, -1
	for i, n := range nodes {
		if n.Kind == topology.KindType && n.Name == "Billing" {
			billing = int64(i)
		}
		if n.Kind == topology.KindType && n.Name == "Invoice" {
			invoice = int64(i)
		}
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeContains && e.FromID == billing && e.ToID == invoice {
			return
		}
	}
	t.Error("module Billing must contain class Invoice")
}

func TestRuby_CallEdgeIntraFile(t *testing.T) {
	nodes, edges := rbExtract(t)
	var toS, formatted int64 = -1, -1
	for i, n := range nodes {
		switch {
		case n.Kind == topology.KindMethod && n.Name == "to_s":
			toS = int64(i)
		case n.Kind == topology.KindMethod && n.Name == "formatted":
			formatted = int64(i)
		}
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeCalls && e.FromID == toS && e.ToID == formatted {
			if e.Confidence <= 0 || e.Confidence > 0.8 {
				t.Errorf("call confidence = %v; a name-resolved edge should be heuristic (<= 0.8)", e.Confidence)
			}
			return
		}
	}
	t.Error("no call edge from to_s to formatted")
}

// Both test conventions have to be recognised, and they arrive by completely
// different routes: minitest's is a `def` whose name starts with test_, RSpec's
// is a method call carrying a block whose description is a string argument.
func TestRuby_DetectsBothTestConventions(t *testing.T) {
	nodes, _ := rbExtract(t)
	tests := map[string]bool{}
	for _, n := range nodes {
		if n.Kind == topology.KindTest {
			tests[n.Name] = true
		}
	}
	for _, want := range []string{"test_totals", "with a total", "formats the total"} {
		if !tests[want] {
			t.Errorf("expected a test node named %q; got %v", want, tests)
		}
	}
	if tests["not_a_test"] {
		t.Error("not_a_test is an ordinary method and must not be recorded as a test")
	}
}

func TestRuby_SignaturesCarryStructure(t *testing.T) {
	nodes, _ := rbExtract(t)
	if got := rbFind(t, nodes, topology.KindType, "Invoice").Signature; !strings.Contains(got, "< Base") {
		t.Errorf("class signature = %q, want it to keep the superclass", got)
	}
	if got := rbFind(t, nodes, topology.KindMethod, "build").Signature; !strings.Contains(got, "self.build") {
		t.Errorf("singleton signature = %q, want it to show self.", got)
	}
	if got := rbFind(t, nodes, topology.KindFunction, "trailing_helper").Signature; !strings.Contains(got, "b = 1") {
		t.Errorf("function signature = %q, want it to keep the default argument", got)
	}
}

// The span must reconstruct the declaration exactly; a past extractor shipped
// with no spans at all and the parity sweep could not see it.
func TestRuby_ByteSpanReconstructsDeclaration(t *testing.T) {
	nodes, _ := rbExtract(t)
	n := rbFind(t, nodes, topology.KindFunction, "trailing_helper")
	if !n.HasBytes {
		t.Fatal("HasBytes false; every emitted node must carry its span")
	}
	got := string(rbSrc[n.StartByte:n.EndByte])
	if !strings.HasPrefix(got, "def trailing_helper") || !strings.HasSuffix(strings.TrimSpace(got), "end") {
		t.Errorf("span does not reconstruct the declaration:\n%s", got)
	}
}

func TestRuby_DocSpanCoversPrecedingComment(t *testing.T) {
	nodes, _ := rbExtract(t)
	n := rbFind(t, nodes, topology.KindType, "Invoice")
	if n.DocStartByte == 0 && n.DocEndByte == 0 {
		t.Fatal("expected a doc span for a class with a comment directly above it")
	}
	if got := string(rbSrc[n.DocStartByte:n.DocEndByte]); !strings.Contains(got, "An invoice for a customer") {
		t.Errorf("doc span = %q, want the comment above the class", got)
	}
}

func TestRuby_EmptyAndCommentOnly(t *testing.T) {
	for _, src := range []string{"", "# just a comment\n", "\n\n"} {
		nodes, edges, err := NewRuby().Extract(context.Background(), "a.rb", []byte(src))
		if err != nil {
			t.Errorf("Extract(%q): %v", src, err)
		}
		if len(nodes) != 0 || len(edges) != 0 {
			t.Errorf("Extract(%q) = %d nodes, %d edges; want none", src, len(nodes), len(edges))
		}
	}
}

func TestRuby_LanguageAndPath(t *testing.T) {
	nodes, _ := rbExtract(t)
	if len(nodes) == 0 {
		t.Fatal("fixture produced no nodes; the loop below would be vacuous")
	}
	for _, n := range nodes {
		if n.Language != "ruby" {
			t.Errorf("node %q language = %q, want ruby", n.Name, n.Language)
		}
		if n.Path != "app/billing.rb" {
			t.Errorf("node %q path = %q, want the path passed to Extract", n.Name, n.Path)
		}
	}
}

// Gemfile and Rakefile carry no extension, and a Rails project's dependency and
// task definitions are among the first things an agent asks the Map for.
func TestRuby_Extensions(t *testing.T) {
	got := NewRuby().Extensions()
	for _, want := range []string{".rb", ".rake", ".gemspec", "gemfile", "rakefile"} {
		if !slicesContains(got, want) {
			t.Errorf("Extensions() = %v, missing %q", got, want)
		}
	}
}

func slicesContains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
