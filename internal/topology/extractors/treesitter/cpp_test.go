package treesitter

import (
	"context"
	"strings"
	"testing"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/plumbkit/plumb/internal/topology"
)

// cppSrc is the idiomatic sample: a namespace holding a class with declared
// members, a struct with an inline method, a scoped enum, a class template and a
// function template, followed by the out-of-line definitions a .cpp carries and
// a gtest body.
//
// It deliberately contains no Catch2 TEST_CASE. That macro does not parse (see
// TestCpp_Catch2DoesNotParse_ErrorRecoveryKeepsSymbols), and this fixture
// doubles as the cross-language parse-fidelity sample, which must isolate a
// grammar CASCADE rather than re-measure a defect already pinned by its own
// test.
var cppSrc = []byte(`#include <vector>
#include "widget.hpp"

#define MAX_ITEMS 10
#define SQUARE(x) ((x) * (x))

namespace geom {

constexpr double kPi = 3.14159;

using Scalar = double;
typedef unsigned int uint_t;

// A point in the plane.
class Point {
public:
  Point(double x, double y);
  ~Point();
  double norm() const;
  Point operator+(const Point &o) const;
  static Point origin();

private:
  double x_;
  double y_;
  const int kMaxPoints = 64;
};

struct Pair {
  int a;
  int b;
  int sum() const { return a + b; }
};

enum class Colour { Red, Green, Blue };

template <typename T>
class Box {
public:
  explicit Box(T v) : v_(v) {}
  T get() const { return v_; }

private:
  T v_;
};

template <typename T>
T identity(T v) {
  return v;
}

double Point::norm() const {
  double scale = 1.0;
  return scale * scaled_norm(x_, y_);
}

int &counter() {
  static int c = 0;
  return c;
}

int Point::instances_ = 0;

} // namespace geom

TEST(GeomSuite, NormIsPositive) {
  EXPECT_GT(1.0, 0.0);
}

double scaled_norm(double x, double y) {
  return x + y;
}
`)

func cppExtract(t *testing.T) ([]topology.Node, []topology.Edge) {
	t.Helper()
	nodes, edges, err := NewCpp().Extract(context.Background(), "src/geom.cpp", cppSrc)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return nodes, edges
}

func cppFind(t *testing.T, nodes []topology.Node, kind topology.NodeKind, name string) topology.Node {
	t.Helper()
	for _, n := range nodes {
		if n.Kind == kind && n.Name == name {
			return n
		}
	}
	t.Fatalf("no %s named %q among %d nodes", kind, name, len(nodes))
	return topology.Node{}
}

func cppIndexOf(nodes []topology.Node, kind topology.NodeKind, name string) int64 {
	for i, n := range nodes {
		if n.Kind == kind && n.Name == name {
			return int64(i)
		}
	}
	return -1
}

// cppIndexOfSig disambiguates the one shape that is deliberately two nodes: a
// method declared in its class and defined out of line in the same file.
func cppIndexOfSig(nodes []topology.Node, kind topology.NodeKind, name, sig string) int64 {
	for i, n := range nodes {
		if n.Kind == kind && n.Name == name && strings.Contains(n.Signature, sig) {
			return int64(i)
		}
	}
	return -1
}

func cppHasEdge(edges []topology.Edge, kind topology.EdgeKind, from, to int64) bool {
	for _, e := range edges {
		if e.Kind == kind && e.FromID == from && e.ToID == to {
			return true
		}
	}
	return false
}

func TestCpp_KindsExtracted(t *testing.T) {
	nodes, _ := cppExtract(t)
	for _, w := range []struct {
		kind topology.NodeKind
		name string
	}{
		{topology.KindImport, "vector"},
		{topology.KindImport, "widget.hpp"},
		{topology.KindConstant, "MAX_ITEMS"},
		{topology.KindFunction, "SQUARE"}, // a function-like macro is called like one
		{topology.KindConstant, "kPi"},
		{topology.KindType, "Scalar"}, // using-alias
		{topology.KindType, "uint_t"}, // typedef
		{topology.KindClass, "Point"},
		{topology.KindClass, "Pair"},
		{topology.KindClass, "Box"},    // class template
		{topology.KindType, "Colour"},  // scoped enum
		{topology.KindConstant, "Red"}, // enumerator
		{topology.KindMethod, "Point"}, // constructor declaration
		{topology.KindMethod, "~Point"},
		{topology.KindMethod, "norm"},
		{topology.KindMethod, "operator+"},
		{topology.KindMethod, "origin"},
		{topology.KindMethod, "sum"}, // inline member definition
		{topology.KindVariable, "x_"},
		{topology.KindVariable, "a"},
		{topology.KindFunction, "identity"}, // function template
		{topology.KindFunction, "counter"},  // reference return
		{topology.KindVariable, "instances_"},
		{topology.KindTest, "GeomSuite.NormIsPositive"},
		{topology.KindFunction, "scaled_norm"},
	} {
		cppFind(t, nodes, w.kind, w.name)
	}
}

// A namespace is re-opened in every file that contributes to it, so indexing it
// would scatter a node named `detail` across a codebase. Its name survives where
// it is useful — in the qualified name of everything inside it.
func TestCpp_NamespaceQualifiesButEmitsNoSymbol(t *testing.T) {
	nodes, _ := cppExtract(t)
	for _, n := range nodes {
		if n.Name == "geom" {
			t.Errorf("namespace emitted as %s %q; it should only qualify", n.Kind, n.Qualified)
		}
	}
	for _, w := range []struct {
		kind      topology.NodeKind
		name      string
		qualified string
	}{
		{topology.KindClass, "Point", "geom::Point"},
		{topology.KindMethod, "sum", "geom::Pair::sum"},
		{topology.KindVariable, "x_", "geom::Point::x_"},
		{topology.KindConstant, "Red", "geom::Colour::Red"},
		{topology.KindConstant, "kPi", "geom::kPi"},
	} {
		if got := cppFind(t, nodes, w.kind, w.name).Qualified; got != w.qualified {
			t.Errorf("%s %q qualified = %q, want %q", w.kind, w.name, got, w.qualified)
		}
	}
}

// A class body holds methods, fields and nested types where C has only fields.
// Treating it as C would emit every method as a field and drop the rest.
func TestCpp_ClassMembersAreOwnedByTheirClass(t *testing.T) {
	nodes, edges := cppExtract(t)
	point := cppIndexOf(nodes, topology.KindClass, "Point")
	for _, w := range []struct {
		kind topology.NodeKind
		name string
	}{
		{topology.KindMethod, "norm"},
		{topology.KindMethod, "origin"},
		{topology.KindVariable, "x_"},
		{topology.KindVariable, "y_"},
	} {
		child := cppIndexOf(nodes, w.kind, w.name)
		if !cppHasEdge(edges, topology.EdgeContains, point, child) {
			t.Errorf("class Point does not contain %s %q", w.kind, w.name)
		}
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeContains && e.FromID == point {
			if e.Confidence != 1.0 || e.Source != "extractor" {
				t.Errorf("containment edge = %v/%q, want 1.0/extractor", e.Confidence, e.Source)
			}
		}
	}
}

func TestCpp_EnumeratorsBelongToTheirEnum(t *testing.T) {
	nodes, edges := cppExtract(t)
	colour := cppIndexOf(nodes, topology.KindType, "Colour")
	for _, name := range []string{"Red", "Green", "Blue"} {
		if !cppHasEdge(edges, topology.EdgeContains, colour, cppIndexOf(nodes, topology.KindConstant, name)) {
			t.Errorf("enum Colour must contain enumerator %q", name)
		}
	}
}

// Most of a .cpp file is out-of-line member definitions. C's declarator walk
// stops at a plain identifier, so every one of them would resolve to no name at
// all and vanish.
func TestCpp_OutOfLineDefinitionIsAQualifiedMethod(t *testing.T) {
	src := []byte("void Foo::bar(int x) { }\nFoo::Foo() { }\nFoo::~Foo() { }\nFoo &Foo::operator=(const Foo &o) { return *this; }\n")
	nodes, _, err := NewCpp().Extract(context.Background(), "a.cpp", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, w := range []struct{ name, qualified string }{
		{"bar", "Foo::bar"},
		{"Foo", "Foo::Foo"},
		{"~Foo", "Foo::~Foo"},
		{"operator=", "Foo::operator="},
	} {
		if got := cppFind(t, nodes, topology.KindMethod, w.name).Qualified; got != w.qualified {
			t.Errorf("%q qualified = %q, want %q", w.name, got, w.qualified)
		}
	}
}

// A reference-returning function wraps its declarator in a node C never
// produces; without looking through it the function is dropped before it is
// named.
func TestCpp_ReferenceReturnIsResolved(t *testing.T) {
	nodes, _ := cppExtract(t)
	cppFind(t, nodes, topology.KindFunction, "counter")

	hdr := []byte("const Foo &cref();\nint &mref();\n")
	nodes, _, err := NewCpp().Extract(context.Background(), "a.hpp", hdr)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, name := range []string{"cref", "mref"} {
		cppFind(t, nodes, topology.KindFunction, name)
	}
}

// replace_symbol_body and move_symbol slice source with these spans, so a
// template's span must cover its `template <…>` header — a span starting at the
// class would leave the header stranded on the next edit.
func TestCpp_TemplateSpanCoversItsHeader(t *testing.T) {
	nodes, _ := cppExtract(t)
	for _, w := range []struct {
		kind topology.NodeKind
		name string
	}{
		{topology.KindClass, "Box"},
		{topology.KindFunction, "identity"},
	} {
		n := cppFind(t, nodes, w.kind, w.name)
		if got := string(cppSrc[n.StartByte:n.EndByte]); !strings.HasPrefix(got, "template <typename T>") {
			t.Errorf("%s %q span starts %q, want the template header", w.kind, w.name, firstLine(got))
		}
		if !strings.HasPrefix(n.Signature, "template <typename T>") {
			t.Errorf("%s %q signature = %q, want the template header", w.kind, w.name, n.Signature)
		}
	}
}

func TestCpp_LocalsInsideFunctionsAreSuppressed(t *testing.T) {
	nodes, _ := cppExtract(t)
	for _, n := range nodes {
		switch n.Name {
		case "scale", "c", "v":
			t.Errorf("a local declaration leaked into the index as %s %q", n.Kind, n.Qualified)
		}
	}
}

func TestCpp_CallEdgeIntraFile(t *testing.T) {
	nodes, edges := cppExtract(t)
	norm := cppIndexOfSig(nodes, topology.KindMethod, "norm", "double Point::norm")
	helper := cppIndexOf(nodes, topology.KindFunction, "scaled_norm")
	if !cppHasEdge(edges, topology.EdgeCalls, norm, helper) {
		t.Error("no call edge from the out-of-line Point::norm to scaled_norm")
	}
}

// A class member declared in one place and defined in another is two nodes on
// purpose: the declaration carries the class containment and the doc comment,
// the definition carries the body, and a reader navigates to both.
func TestCpp_DeclarationAndOutOfLineDefinitionAreBothIndexed(t *testing.T) {
	nodes, _ := cppExtract(t)
	if decl := cppIndexOfSig(nodes, topology.KindMethod, "norm", "double norm() const;"); decl < 0 {
		t.Error("the in-class declaration of norm is missing")
	}
	if def := cppIndexOfSig(nodes, topology.KindMethod, "norm", "double Point::norm"); def < 0 {
		t.Error("the out-of-line definition of norm is missing")
	}
}

// A test body is a function whose enclosing name is a macro, so the call pass
// has to resolve it the same way the emitting pass named it or every call a test
// makes goes unattributed.
func TestCpp_CallEdgeFromTestBody(t *testing.T) {
	src := []byte("int helper() { return 1; }\nTEST(S, N) { helper(); }\n")
	nodes, edges, err := NewCpp().Extract(context.Background(), "a_test.cpp", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	from := cppIndexOf(nodes, topology.KindTest, "S.N")
	to := cppIndexOf(nodes, topology.KindFunction, "helper")
	if !cppHasEdge(edges, topology.EdgeCalls, from, to) {
		t.Errorf("no call edge from the test body to helper (test=%d helper=%d)", from, to)
	}
}

// gtest and Boost name a test by its macro arguments, and so does the test
// runner: `--gtest_filter=GeomSuite.NormIsPositive` is what someone chasing a
// failure types, so it is what the index must hold.
func TestCpp_TestMacroBodiesAreTests(t *testing.T) {
	src := []byte(`TEST(SuiteA, One) { }
TEST_F(FixtureB, Two) { }
TEST_P(SuiteC, Three) { }
BOOST_AUTO_TEST_CASE(four) { }
`)
	nodes, _, err := NewCpp().Extract(context.Background(), "a_test.cpp", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, want := range []string{"SuiteA.One", "FixtureB.Two", "SuiteC.Three", "four"} {
		cppFind(t, nodes, topology.KindTest, want)
	}
}

// TestCpp_Catch2DoesNotParse_ErrorRecoveryKeepsSymbols pins a measured
// gotreesitter v0.48.1 defect and the mitigation for it.
//
// `TEST_CASE("name", "[tag]") { … }` takes string arguments, which the grammar
// cannot reconcile with a function declarator. On its own at the top of a file
// it flattens into loose expression nodes; after any other declaration it
// becomes an ERROR node — and that node routinely swallows the rest of the
// translation unit, including a whole `#ifndef` guard. Descending into the ERROR
// is what keeps the file's real declarations: they are still parsed correctly as
// their own typed nodes inside it.
func TestCpp_Catch2DoesNotParse_ErrorRecoveryKeepsSymbols(t *testing.T) {
	src := []byte(`#ifndef WIDGET_HPP
#define WIDGET_HPP

namespace w {
class Widget {
public:
  void draw();
};
}

TEST_CASE("widget draws", "[widget]") { }

int after_the_error(int v) { return v; }
#endif
`)
	if !cppParsesWithDefect(t, src) {
		t.Error("Catch2 TEST_CASE now parses cleanly — re-check whether the ERROR descent in cppWalk.top is still needed, and consider emitting TEST_CASE as a test")
	}
	nodes, _, err := NewCpp().Extract(context.Background(), "widget.hpp", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	// The class sits INSIDE the ERROR node; the trailing function sits after it.
	cppFind(t, nodes, topology.KindClass, "Widget")
	cppFind(t, nodes, topology.KindMethod, "draw")
	cppFind(t, nodes, topology.KindConstant, "WIDGET_HPP")
	cppFind(t, nodes, topology.KindFunction, "after_the_error")
	for _, n := range nodes {
		if strings.Contains(n.Name, "TEST_CASE") {
			t.Errorf("a symbol was invented from ERROR text: %s %q", n.Kind, n.Name)
		}
	}
}

// A header is mostly class declarations, and a guarded one must not index as
// empty — the preprocessor branch has to be descended into.
func TestCpp_GuardedHeaderIsIndexed(t *testing.T) {
	hdr := []byte(`#pragma once
#ifndef GEOM_HPP
#define GEOM_HPP
namespace geom {
class Shape {
public:
  virtual double area() const = 0;
  virtual ~Shape() = default;
};
double area_of(const Shape &s);
}
#endif
`)
	nodes, _, err := NewCpp().Extract(context.Background(), "geom.hpp", hdr)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	cppFind(t, nodes, topology.KindClass, "Shape")
	cppFind(t, nodes, topology.KindMethod, "area")
	cppFind(t, nodes, topology.KindMethod, "~Shape")
	cppFind(t, nodes, topology.KindFunction, "area_of")
}

// `extern "C" { … }` wraps ordinary declarations, and the C walk would dispatch
// anything inside it through C's switch.
func TestCpp_ExternCAndNestedNamespaceAreDescended(t *testing.T) {
	src := []byte("extern \"C\" {\nint c_api(int x);\n}\nnamespace a::b {\nclass Deep { public: void go(); };\n}\n")
	nodes, _, err := NewCpp().Extract(context.Background(), "a.hpp", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	cppFind(t, nodes, topology.KindFunction, "c_api")
	if got := cppFind(t, nodes, topology.KindClass, "Deep").Qualified; got != "a::b::Deep" {
		t.Errorf("nested-namespace qualified name = %q, want a::b::Deep", got)
	}
}

func TestCpp_SignatureOmitsTheBody(t *testing.T) {
	nodes, _ := cppExtract(t)
	def := cppIndexOfSig(nodes, topology.KindMethod, "norm", "double Point::norm() const")
	if def < 0 {
		t.Fatalf("no out-of-line definition of norm among %d nodes", len(nodes))
	}
	if sig := nodes[def].Signature; strings.Contains(sig, "return") {
		t.Errorf("signature = %q, must not include the body", sig)
	}
	if got := cppFind(t, nodes, topology.KindClass, "Point").Signature; !strings.HasPrefix(got, "class Point") {
		t.Errorf("class signature = %q, want the declaration head", got)
	}
	// A class declaration's head is where `final` and the base list live.
	src := []byte("class D final : public B, private H { public: int r() override; };\n")
	nodes, _, err := NewCpp().Extract(context.Background(), "a.hpp", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got := cppFind(t, nodes, topology.KindClass, "D").Signature; !strings.Contains(got, ": public B, private H") {
		t.Errorf("class signature = %q, want the base list", got)
	}
	if got := cppFind(t, nodes, topology.KindMethod, "r").Signature; !strings.Contains(got, "override") {
		t.Errorf("method declaration signature = %q, want the trailing specifiers", got)
	}
}

func TestCpp_ByteSpanReconstructsDeclaration(t *testing.T) {
	nodes, _ := cppExtract(t)
	for _, n := range nodes {
		if !n.HasBytes {
			t.Fatalf("%s %q has no span; every emitted node must carry one", n.Kind, n.Name)
		}
		if n.StartByte < 0 || n.EndByte > len(cppSrc) || n.EndByte <= n.StartByte {
			t.Fatalf("%s %q span [%d,%d) is not inside the source", n.Kind, n.Name, n.StartByte, n.EndByte)
		}
	}
	p := cppFind(t, nodes, topology.KindClass, "Point")
	if got := string(cppSrc[p.StartByte:p.EndByte]); !strings.HasPrefix(got, "class Point") {
		t.Errorf("span does not reconstruct the declaration:\n%s", got)
	}
}

func TestCpp_DocSpanCoversPrecedingComment(t *testing.T) {
	nodes, _ := cppExtract(t)
	n := cppFind(t, nodes, topology.KindClass, "Point")
	if !n.HasDocSpan() {
		t.Fatal("expected a doc span for a class with a comment directly above it")
	}
	if got := string(cppSrc[n.DocStartByte:n.DocEndByte]); !strings.Contains(got, "A point in the plane") {
		t.Errorf("doc span = %q, want the comment above the class", got)
	}
}

func TestCpp_EmptyAndCommentOnly(t *testing.T) {
	for _, src := range []string{"", "// just a comment\n", "\n\n", "/* block */\n"} {
		nodes, edges, err := NewCpp().Extract(context.Background(), "a.cpp", []byte(src))
		if err != nil {
			t.Errorf("Extract(%q): %v", src, err)
		}
		if len(nodes) != 0 || len(edges) != 0 {
			t.Errorf("Extract(%q) = %d nodes, %d edges; want none", src, len(nodes), len(edges))
		}
	}
}

func TestCpp_LanguageAndPath(t *testing.T) {
	nodes, _ := cppExtract(t)
	if len(nodes) == 0 {
		t.Fatal("fixture produced no nodes; the loop below would be vacuous")
	}
	for _, n := range nodes {
		if n.Language != "cpp" {
			t.Errorf("node %q language = %q, want cpp", n.Name, n.Language)
		}
		if n.Path != "src/geom.cpp" {
			t.Errorf("node %q path = %q, want the path passed to Extract", n.Name, n.Path)
		}
	}
}

func TestCpp_Extensions(t *testing.T) {
	got := NewCpp().Extensions()
	for _, want := range []string{".cc", ".cpp", ".cxx", ".hh", ".hpp", ".hxx"} {
		if !slicesContains(got, want) {
			t.Errorf("Extensions() = %v, missing %q", got, want)
		}
	}
}

// The fixture doubles as the cross-language parse-fidelity sample, which is only
// meaningful if the grammar parses it cleanly to begin with.
func TestCpp_FixtureParsesWithoutDefect(t *testing.T) {
	if cppParsesWithDefect(t, cppSrc) {
		t.Error("the fixture no longer parses cleanly; a fidelity sample must isolate a cascade, not carry a known defect")
	}
}

// cppParsesWithDefect reports whether the raw grammar leaves an ERROR or a
// zero-width MISSING node in src. A MISSING node is not an ERROR, and checking
// only for errors misses that entire class of silent loss.
func cppParsesWithDefect(t *testing.T, src []byte) bool {
	t.Helper()
	tree, err := tsg.NewParser(grammars.CppLanguage()).Parse(src)
	if err != nil || tree == nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()
	return hasMissingOrError(tree.RootNode())
}

func firstLine(s string) string {
	if before, _, ok := strings.Cut(s, "\n"); ok {
		return before
	}
	return s
}

// A member's kind follows its mutability, not where it sits. KindField is for
// keys of a data-format file; topology.KindField's own doc comment says a member
// of a code type is KindConstant when immutable and KindVariable otherwise, and
// TestExtractors_MemberConventions enforces that across the package — C++ is now
// in that table, which is what stops this drifting back.
func TestCpp_MemberKindFollowsMutability(t *testing.T) {
	nodes, _ := cppExtract(t)

	var immutable, mutable bool
	for _, n := range nodes {
		switch n.Name {
		case "kMaxPoints":
			immutable = n.Kind == topology.KindConstant
			if n.Kind != topology.KindConstant {
				t.Errorf("const member kind = %q, want %q", n.Kind, topology.KindConstant)
			}
		case "x_":
			mutable = n.Kind == topology.KindVariable
			if n.Kind != topology.KindVariable {
				t.Errorf("mutable member kind = %q, want %q", n.Kind, topology.KindVariable)
			}
		}
	}
	if !immutable || !mutable {
		t.Fatalf("fixture must exercise both cases: immutable=%v mutable=%v", immutable, mutable)
	}
	for _, n := range nodes {
		if n.Kind == topology.KindField {
			t.Errorf("%q emitted as KindField; a code-type member is never KindField", n.Name)
		}
	}
}
