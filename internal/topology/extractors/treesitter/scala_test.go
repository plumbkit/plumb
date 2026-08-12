package treesitter

import (
	"context"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

var scalaSrc = []byte(`package com.example.app

import scala.collection.mutable
import java.util.{List, Map}
import scala.util._

/** A greeter. */
class Greeter(name: String) extends Base with Mixin {
  val greeting: String = "hello"
  var count: Int = 0

  def greet(who: String): String = {
    val local = who
    def decorate(s: String): String = s
    decorate(local)
  }
}

case class Point(x: Int, y: Int)

abstract class Shape {
  def area: Double
}

object Greeter {
  def apply(n: String): Greeter = new Greeter(n)
}

case object Empty

trait Animal {
  def speak: String
  def legs: Int = 4
}

trait Contract {
  def must(): Unit
}

package object util {
  type StringMap = Map[String, String]
  val Version = "1.0"
}

package deeply.nested {
  class Inner
}

implicit class RichInt(val self: Int) extends AnyVal {
  def squared: Int = self * self
}

object Anon {
  val runner = new Runnable { def run(): Unit = () }
}
`)

var scala3Src = []byte(`package example

import cats.syntax.all.*

enum Colour:
  case Red, Green
  def isWarm: Boolean = this == Red

given intOrd: Ordering[Int] with
  def compare(x: Int, y: Int): Int = x - y

def maxOf[T](xs: List[T])(using ord: Ordering[T]): T = xs.max

extension (s: String)
  def shout: String = s.toUpperCase

class Indented(x: Int):
  val doubled = x * 2
  def triple: Int =
    val y = x
    y * 3

trait Pure:
  def must(): Unit

opaque type Money = BigDecimal
`)

var scalaSpecSrc = []byte(`import org.scalatest.flatspec.AnyFlatSpec

class StackSpec extends AnyFlatSpec {
  "A Stack" should "pop values" in {
    assert(true)
  }
}

class SuiteSpec extends munit.FunSuite {
  test("addition works") {
    assertEquals(1 + 1, 2)
  }
}

class NestedSpec extends AnyFunSpec {
  describe("A Set") {
    it("has size zero") {
      assert(true)
    }
  }
}
`)

func scalaExtract(t *testing.T, path string, src []byte) ([]topology.Node, []topology.Edge) {
	t.Helper()
	nodes, edges, err := NewScala().Extract(context.Background(), path, src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return nodes, edges
}

func scalaNamed(nodes []topology.Node, name string) (topology.Node, bool) {
	for _, n := range nodes {
		if n.Name == name {
			return n, true
		}
	}
	return topology.Node{}, false
}

func TestScala_LandmarksExtracted(t *testing.T) {
	nodes, _ := scalaExtract(t, "src/app.scala", scalaSrc)

	want := map[string]topology.NodeKind{
		"com.example.app":       topology.KindPackage,
		"deeply.nested":         topology.KindPackage,
		"java.util.{List, Map}": topology.KindImport,
		"scala.util._":          topology.KindImport,
		"Greeter":               topology.KindClass,
		"Point":                 topology.KindClass,
		"Shape":                 topology.KindClass,
		"Empty":                 topology.KindClass,
		"RichInt":               topology.KindClass,
		"Inner":                 topology.KindClass,
		"util":                  topology.KindClass,
		"greet":                 topology.KindMethod,
		"apply":                 topology.KindMethod,
		"area":                  topology.KindMethod,
		"squared":               topology.KindMethod,
		"StringMap":             topology.KindType,
	}
	for name, kind := range want {
		n, ok := scalaNamed(nodes, name)
		if !ok {
			t.Errorf("missing landmark %q", name)
			continue
		}
		if n.Kind != kind {
			t.Errorf("%q: kind = %q, want %q", name, n.Kind, kind)
		}
	}
}

// A `val` cannot be reassigned and a `var` can, which is exactly the split
// topology.KindField's doc comment asks a member of a CODE type to be sorted by.
// KindField is for the keys of a data-format file, so no Scala binding is ever
// one — at member scope or file scope.
func TestScala_ValIsConstantVarIsVariable(t *testing.T) {
	nodes, _ := scalaExtract(t, "src/app.scala", scalaSrc)

	for name, want := range map[string]topology.NodeKind{
		"greeting": topology.KindConstant,
		"count":    topology.KindVariable,
		"Version":  topology.KindConstant,
	} {
		n, ok := scalaNamed(nodes, name)
		if !ok {
			t.Fatalf("%q not extracted", name)
		}
		if n.Kind != want {
			t.Errorf("%q: kind = %q, want %q", name, n.Kind, want)
		}
	}
	for _, n := range nodes {
		if n.Kind == topology.KindField {
			t.Errorf("%q emitted as KindField; code members are constant/variable", n.Name)
		}
	}
}

// The concrete-vs-contract split the Rust, Kotlin and Swift extractors use: a
// trait carrying an implementation is a class, a pure contract is a type.
func TestScala_TraitKindDependsOnItsBody(t *testing.T) {
	nodes, _ := scalaExtract(t, "src/app.scala", scalaSrc)

	animal, ok := scalaNamed(nodes, "Animal")
	if !ok {
		t.Fatal("Animal not extracted")
	}
	if animal.Kind != topology.KindClass {
		t.Errorf("trait with a concrete member: kind = %q, want %q", animal.Kind, topology.KindClass)
	}
	contract, ok := scalaNamed(nodes, "Contract")
	if !ok {
		t.Fatal("Contract not extracted")
	}
	if contract.Kind != topology.KindType {
		t.Errorf("pure trait: kind = %q, want %q", contract.Kind, topology.KindType)
	}
}

// A flat `package a.b` header applies to the rest of the file, and a braced one
// nests, so the qualified name is the name a Scala programmer would write.
func TestScala_QualifiedNameCarriesPackageAndOwner(t *testing.T) {
	nodes, _ := scalaExtract(t, "src/app.scala", scalaSrc)

	for name, want := range map[string]string{
		"Greeter":   "com.example.app.Greeter",
		"greet":     "com.example.app.Greeter.greet",
		"Inner":     "com.example.app.deeply.nested.Inner",
		"StringMap": "com.example.app.util.StringMap",
	} {
		n, ok := scalaNamed(nodes, name)
		if !ok {
			t.Fatalf("%q not extracted", name)
		}
		if n.Qualified != want {
			t.Errorf("%q: qualified = %q, want %q", name, n.Qualified, want)
		}
	}
}

func TestScala_MembersAreContainedByTheirOwner(t *testing.T) {
	nodes, edges := scalaExtract(t, "src/app.scala", scalaSrc)

	idxOf := func(name string) int64 {
		t.Helper()
		for i, n := range nodes {
			if n.Name == name {
				return int64(i)
			}
		}
		t.Fatalf("node %q not found", name)
		return -1
	}
	contains := func(from, to int64) bool {
		for _, e := range edges {
			if e.FromID == from && e.ToID == to && e.Kind == topology.EdgeContains {
				if e.Confidence != 1.0 || e.Source != "extractor" {
					t.Errorf("containment edge: confidence=%v source=%q", e.Confidence, e.Source)
				}
				return true
			}
		}
		return false
	}
	for _, pair := range [][2]string{
		{"Greeter", "greeting"},
		{"Greeter", "count"},
		{"Shape", "area"},
		{"deeply.nested", "Inner"},
		{"util", "StringMap"},
	} {
		if !contains(idxOf(pair[0]), idxOf(pair[1])) {
			t.Errorf("no containment edge %s → %s", pair[0], pair[1])
		}
	}
}

// A def written inside a method body is a helper, not a member — and the
// members of an anonymous instance belong to a type with no name, so there is
// nothing to navigate to. Both land in a `block`, which the statement walk never
// enters.
func TestScala_LocalsAndAnonymousMembersAreSuppressed(t *testing.T) {
	nodes, _ := scalaExtract(t, "src/app.scala", scalaSrc)

	for _, name := range []string{"local", "decorate", "run"} {
		if _, ok := scalaNamed(nodes, name); ok {
			t.Errorf("%q should not be emitted", name)
		}
	}
}

func TestScala_Scala3IndentationSyntax(t *testing.T) {
	nodes, _ := scalaExtract(t, "src/three.scala", scala3Src)

	want := map[string]topology.NodeKind{
		"Colour":   topology.KindClass,
		"Red":      topology.KindConstant,
		"Green":    topology.KindConstant,
		"isWarm":   topology.KindMethod,
		"intOrd":   topology.KindConstant,
		"compare":  topology.KindMethod,
		"maxOf":    topology.KindFunction,
		"shout":    topology.KindMethod,
		"Indented": topology.KindClass,
		"doubled":  topology.KindConstant,
		"triple":   topology.KindMethod,
		"Pure":     topology.KindType,
		"Money":    topology.KindType,
	}
	for name, kind := range want {
		n, ok := scalaNamed(nodes, name)
		if !ok {
			t.Errorf("missing Scala 3 landmark %q", name)
			continue
		}
		if n.Kind != kind {
			t.Errorf("%q: kind = %q, want %q", name, n.Kind, kind)
		}
	}
	// `val y = x` inside an indented method body is a local, exactly as a braced
	// body's would be.
	if _, ok := scalaNamed(nodes, "y"); ok {
		t.Error("indented-block local should not be emitted")
	}
}

// An extension method is invoked on a receiver whatever scope it is written at,
// so it is a method, prefixed by the type it extends.
func TestScala_ExtensionMethodCarriesItsReceiver(t *testing.T) {
	nodes, _ := scalaExtract(t, "src/three.scala", scala3Src)

	n, ok := scalaNamed(nodes, "shout")
	if !ok {
		t.Fatal("shout not extracted")
	}
	if n.Qualified != "example.String.shout" {
		t.Errorf("qualified = %q, want %q", n.Qualified, "example.String.shout")
	}
}

// A self-type followed by a doc comment loses to Scala 3's fewer-braces rule and
// the grammar swallows the whole body into a colon_argument. Without the
// recovery the trait indexes with none of its members.
func TestScala_SelfTypeWithDocCommentStillYieldsMembers(t *testing.T) {
	src := []byte(`trait GivenWhenThen { this: Informing =>
  /** Forwards a message. */
  def Given(message: String): Unit = info(message)

  /** Forwards another. */
  def When(message: String): Unit = info(message)
}
`)
	nodes, _ := scalaExtract(t, "src/gwt.scala", src)

	for _, name := range []string{"Given", "When"} {
		n, ok := scalaNamed(nodes, name)
		if !ok {
			t.Fatalf("%q not extracted from a swallowed self-type body", name)
		}
		if n.Kind != topology.KindMethod {
			t.Errorf("%q: kind = %q, want %q", name, n.Kind, topology.KindMethod)
		}
	}
	trait, ok := scalaNamed(nodes, "GivenWhenThen")
	if !ok {
		t.Fatal("GivenWhenThen not extracted")
	}
	if trait.Kind != topology.KindClass {
		t.Errorf("trait carrying implementations: kind = %q, want %q", trait.Kind, topology.KindClass)
	}
}

// A recovery node still holds correctly parsed declarations, so descending into
// one recovers them without guessing. Nothing is read from ERROR text.
func TestScala_DeclarationsInsideErrorRecoveryAreFound(t *testing.T) {
	// An unclosed method body drags the whole file into recovery: the ROOT node
	// is an ERROR, and the declarations under it hang off the grammar's hidden
	// `_block` / `_indent` nodes rather than off the ERROR directly.
	src := []byte(`class A {
  def recovered: Int = {
}
class B { def g: Int = 2 }
object C { val v = 1 }
`)
	nodes, _ := scalaExtract(t, "src/broken.scala", src)

	for _, name := range []string{"recovered", "C", "v"} {
		if _, found := scalaNamed(nodes, name); !found {
			t.Errorf("%q lost to error recovery", name)
		}
	}
}

func TestScala_TestsAreDetected(t *testing.T) {
	nodes, _ := scalaExtract(t, "src/spec.scala", scalaSpecSrc)

	want := map[string]topology.NodeKind{
		"A Stack should pop values": topology.KindTest,
		"addition works":            topology.KindTest,
		"A Set":                     topology.KindSection,
		"has size zero":             topology.KindTest,
	}
	for name, kind := range want {
		n, ok := scalaNamed(nodes, name)
		if !ok {
			t.Errorf("missing test %q", name)
			continue
		}
		if n.Kind != kind {
			t.Errorf("%q: kind = %q, want %q", name, n.Kind, kind)
		}
	}
	// A case declared inside describe(…) belongs to it.
	var set, case0 int64 = -1, -1
	for i, n := range nodes {
		switch n.Name {
		case "A Set":
			set = int64(i)
		case "has size zero":
			case0 = int64(i)
		}
	}
	if set < 0 || case0 < 0 {
		t.Fatal("describe/it pair not extracted")
	}
	if nodes[case0].Qualified != nodes[set].Qualified+".has size zero" {
		t.Errorf("nested case qualified = %q, want it under %q", nodes[case0].Qualified, nodes[set].Qualified)
	}
}

// An assertion is the same infix operator as a spec clause; only the trailing
// block separates them.
func TestScala_AssertionsAreNotTests(t *testing.T) {
	src := []byte(`class S extends AnyFlatSpec {
  def check(): Unit = {
    result should be(3)
    behavior of "Thing"
  }
}
`)
	nodes, _ := scalaExtract(t, "src/s.scala", src)

	for _, n := range nodes {
		if n.Kind == topology.KindTest || n.Kind == topology.KindSection {
			t.Errorf("assertion mistaken for a test: %q (%s)", n.Name, n.Kind)
		}
	}
}

func TestScala_CallEdgesAreHeuristic(t *testing.T) {
	src := []byte(`object M {
  def helper(x: Int): Int = x + 1
  def caller(x: Int): Int = helper(x)
}
`)
	nodes, edges := scalaExtract(t, "src/m.scala", src)

	var from, to int64 = -1, -1
	for i, n := range nodes {
		switch n.Name {
		case "caller":
			from = int64(i)
		case "helper":
			to = int64(i)
		}
	}
	if from < 0 || to < 0 {
		t.Fatal("caller/helper not extracted")
	}
	found := false
	for _, e := range edges {
		if e.Kind != topology.EdgeCalls || e.FromID != from || e.ToID != to {
			continue
		}
		found = true
		if e.Confidence != 0.8 || e.Source != "heuristic" {
			t.Errorf("call edge: confidence=%v source=%q, want 0.8/heuristic", e.Confidence, e.Source)
		}
	}
	if !found {
		t.Error("no call edge caller → helper")
	}
}

func TestScala_DocSpanCoversScaladoc(t *testing.T) {
	nodes, _ := scalaExtract(t, "src/app.scala", scalaSrc)

	n, ok := scalaNamed(nodes, "Greeter")
	if !ok {
		t.Fatal("Greeter not extracted")
	}
	if n.DocEndByte <= n.DocStartByte {
		t.Fatal("no doc span on a Scaladoc'd class")
	}
	doc := string(scalaSrc[n.DocStartByte:n.DocEndByte])
	if !strings.Contains(doc, "A greeter.") {
		t.Errorf("doc span = %q, want the Scaladoc block", doc)
	}
}

func TestScala_ByteSpansAreValid(t *testing.T) {
	for _, src := range [][]byte{scalaSrc, scala3Src, scalaSpecSrc} {
		nodes, _ := scalaExtract(t, "src/x.scala", src)
		if len(nodes) == 0 {
			t.Fatal("no nodes")
		}
		for _, n := range nodes {
			if !n.HasBytes {
				t.Errorf("%q: HasBytes not set", n.Name)
				continue
			}
			if n.StartByte < 0 || n.EndByte > len(src) || n.StartByte > n.EndByte {
				t.Errorf("%q: span [%d,%d) out of range for %d bytes", n.Name, n.StartByte, n.EndByte, len(src))
			}
			if n.StartLine <= 0 || n.EndLine < n.StartLine {
				t.Errorf("%q: line range %d–%d", n.Name, n.StartLine, n.EndLine)
			}
			if n.Language != "scala" {
				t.Errorf("%q: language = %q", n.Name, n.Language)
			}
		}
	}
}

func TestScala_SpanCoversTheWholeDeclaration(t *testing.T) {
	nodes, _ := scalaExtract(t, "src/app.scala", scalaSrc)

	n, ok := scalaNamed(nodes, "greet")
	if !ok {
		t.Fatal("greet not extracted")
	}
	body := string(scalaSrc[n.StartByte:n.EndByte])
	if !strings.Contains(body, "decorate(local)") {
		t.Errorf("method span stops before the body: %q", body)
	}
	if !strings.Contains(n.Signature, "def greet(who: String): String") {
		t.Errorf("signature = %q", n.Signature)
	}
	if strings.Contains(n.Signature, "decorate") {
		t.Errorf("signature drags the body in: %q", n.Signature)
	}
}

func TestScala_MalformedInputDoesNotPanic(t *testing.T) {
	for _, src := range []string{
		"",
		"class",
		"package",
		"trait A { def",
		"object O {\n  val x = \"unterminated",
		"\x00\x01\x02",
		strings.Repeat("class A { ", 200),
	} {
		nodes, edges, err := NewScala().Extract(context.Background(), "src/bad.scala", []byte(src))
		if err != nil {
			continue // a bounded parse failure is a valid outcome
		}
		for _, n := range nodes {
			if n.HasBytes && (n.StartByte < 0 || n.EndByte > len(src) || n.StartByte > n.EndByte) {
				t.Errorf("%q: span out of range on malformed input", n.Name)
			}
		}
		for _, e := range edges {
			if e.FromID < 0 || int(e.FromID) >= len(nodes) || e.ToID < 0 || int(e.ToID) >= len(nodes) {
				t.Errorf("edge references a node outside the slice: %+v", e)
			}
		}
	}
}

func TestScala_LanguageAndExtensions(t *testing.T) {
	e := NewScala()
	if e.Language() != "scala" {
		t.Errorf("Language() = %q", e.Language())
	}
	got := e.Extensions()
	want := []string{".scala", ".sc"}
	if len(got) != len(want) {
		t.Fatalf("Extensions() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Extensions()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	nodes, _ := scalaExtract(t, "some/dir/app.scala", scalaSrc)
	for _, n := range nodes {
		if n.Path != "some/dir/app.scala" {
			t.Errorf("%q: path = %q", n.Name, n.Path)
		}
	}
}

func TestScala_ContextCancellationIsHonoured(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := NewScala().Extract(ctx, "src/app.scala", scalaSrc); err == nil {
		t.Error("cancelled context: want an error")
	}
}
