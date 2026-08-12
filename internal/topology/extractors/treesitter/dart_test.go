package treesitter

import (
	"context"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

var dartSrc = []byte(`import 'dart:async';
import 'package:http/http.dart' as http;
export 'src/models.dart';

const defaultLabel = 'counter';

/// A counter that can be ticked.
class Counter<T> extends Base implements Tickable {
  int count = 0;
  final String label;

  Counter(this.count, this.label);
  Counter.zero() : this(0, defaultLabel);

  /// Increment the counter.
  void bump() {
    var scratch = 1;
    reset();
  }

  int get value => count;
  set value(int v) {}

  void reset() {}
}

mixin Loggable {
  void log(String message) {}
}

enum Colour { red, green }

extension StringX on String {
  String shout() => toUpperCase();
}

typedef Handler = void Function(int);

int add(int a, int b) => a + b;

Future<void> trailingHelper() async {
  add(1, 2);
}
`)

func dartExtract(t *testing.T) ([]topology.Node, []topology.Edge) {
	t.Helper()
	nodes, edges, err := NewDart().Extract(context.Background(), "counter.dart", dartSrc)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return nodes, edges
}

func dartNamed(nodes []topology.Node, name string) (topology.Node, bool) {
	for _, n := range nodes {
		if n.Name == name {
			return n, true
		}
	}
	return topology.Node{}, false
}

func TestDart_LandmarksExtracted(t *testing.T) {
	nodes, _ := dartExtract(t)

	want := map[string]topology.NodeKind{
		"dart:async":             topology.KindImport,
		"package:http/http.dart": topology.KindImport,
		"src/models.dart":        topology.KindImport,
		"defaultLabel":           topology.KindConstant,
		"Counter":                topology.KindClass,
		"count":                  topology.KindVariable,
		"label":                  topology.KindConstant,
		"bump":                   topology.KindMethod,
		"reset":                  topology.KindMethod,
		"Loggable":               topology.KindClass,
		"log":                    topology.KindMethod,
		"Colour":                 topology.KindClass,
		"red":                    topology.KindConstant,
		"StringX":                topology.KindClass,
		"shout":                  topology.KindMethod,
		"Handler":                topology.KindType,
		"add":                    topology.KindFunction,
		"trailingHelper":         topology.KindFunction,
	}
	for name, kind := range want {
		n, ok := dartNamed(nodes, name)
		if !ok {
			t.Errorf("missing landmark %q", name)
			continue
		}
		if n.Kind != kind {
			t.Errorf("%q: kind = %q, want %q", name, n.Kind, kind)
		}
	}
}

// A function's signature and its body are siblings in this grammar, so an
// extractor that takes the signature's own span truncates every function in the
// file at its opening brace. This is the defect the shared span guards exist to
// catch, and it is worth pinning locally too.
func TestDart_SpanCoversSignatureAndBody(t *testing.T) {
	nodes, _ := dartExtract(t)

	fn, ok := dartNamed(nodes, "add")
	if !ok {
		t.Fatal("add not extracted")
	}
	got := string(dartSrc[fn.StartByte:fn.EndByte])
	if !strings.Contains(got, "=>") {
		t.Errorf("span stops before the body: %q", got)
	}

	m, ok := dartNamed(nodes, "bump")
	if !ok {
		t.Fatal("bump not extracted")
	}
	body := string(dartSrc[m.StartByte:m.EndByte])
	if !strings.Contains(body, "reset()") {
		t.Errorf("method span does not reach into the body: %q", body)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "}") {
		t.Errorf("method span does not close its body: %q", body)
	}
}

// The same sibling layout defeats the shared walkCallSites/scopeByType helper,
// because a body sits outside its own signature's subtree. Without the local
// pairing there would be no call edges at all.
func TestDart_CallEdgesSurviveTheSiblingBodyLayout(t *testing.T) {
	nodes, edges := dartExtract(t)

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
	has := func(from, to string) bool {
		f, tt := idxOf(from), idxOf(to)
		for _, e := range edges {
			if e.Kind == topology.EdgeCalls && e.FromID == f && e.ToID == tt {
				return true
			}
		}
		return false
	}

	if !has("bump", "reset") {
		t.Error("no call edge from bump to reset")
	}
	if !has("trailingHelper", "add") {
		t.Error("no call edge from trailingHelper to add")
	}
}

// A bare identifier is not a call. Without requiring an argument list, every
// mention of a name would invent an edge.
func TestDart_BareIdentifierIsNotACall(t *testing.T) {
	src := []byte("int add(int a) => a;\n\nvoid main() {\n  var f = add;\n}\n")

	nodes, edges, err := NewDart().Extract(context.Background(), "a.dart", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	var addIdx int64 = -1
	for i, n := range nodes {
		if n.Name == "add" {
			addIdx = int64(i)
		}
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeCalls && e.ToID == addIdx {
			t.Error("a bare reference to a function should not produce a call edge")
		}
	}
}

// Dart writes a named constructor `Counter.zero`, so the name already carries
// its class and the qualified name must not repeat it.
func TestDart_ConstructorQualifiedNameDoesNotRepeatTheClass(t *testing.T) {
	nodes, _ := dartExtract(t)

	n, ok := dartNamed(nodes, "Counter.zero")
	if !ok {
		t.Fatal("named constructor not extracted")
	}
	if got, want := n.Qualified, "Counter.zero"; got != want {
		t.Errorf("qualified = %q, want %q", got, want)
	}
	if n.Kind != topology.KindMethod {
		t.Errorf("kind = %q, want %q", n.Kind, topology.KindMethod)
	}
}

func TestDart_MembersAreContainedByTheirType(t *testing.T) {
	nodes, edges := dartExtract(t)

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
	contains := func(parent, child string) bool {
		p, c := idxOf(parent), idxOf(child)
		for _, e := range edges {
			if e.Kind == topology.EdgeContains && e.FromID == p && e.ToID == c {
				if e.Confidence != 1.0 || e.Source != "extractor" {
					t.Errorf("%s contains %s: confidence=%v source=%q", parent, child, e.Confidence, e.Source)
				}
				return true
			}
		}
		return false
	}

	for _, pair := range [][2]string{
		{"Counter", "bump"},
		{"Counter", "count"},
		{"Loggable", "log"},
		{"Colour", "red"},
		{"StringX", "shout"},
	} {
		if !contains(pair[0], pair[1]) {
			t.Errorf("%s should contain %s", pair[0], pair[1])
		}
	}

	m, _ := dartNamed(nodes, "bump")
	if got, want := m.Qualified, "Counter.bump"; got != want {
		t.Errorf("qualified = %q, want %q", got, want)
	}
}

// Locals are marked unambiguously by the grammar, and a scratch variable inside
// a build method is an implementation detail rather than a landmark.
func TestDart_LocalsAreSuppressed(t *testing.T) {
	nodes, _ := dartExtract(t)

	if _, ok := dartNamed(nodes, "scratch"); ok {
		t.Error("a local variable should not be emitted")
	}
}

// Dart spells its doc comments `///`, which the grammar reports as
// documentation_comment rather than comment. Missing that node type would cost
// every doc span in an idiomatic package.
func TestDart_DocSpanCoversTripleSlashComment(t *testing.T) {
	nodes, _ := dartExtract(t)

	c, ok := dartNamed(nodes, "Counter")
	if !ok {
		t.Fatal("Counter not extracted")
	}
	if got := string(dartSrc[c.DocStartByte:c.DocEndByte]); !strings.Contains(got, "A counter that can be ticked") {
		t.Errorf("doc span = %q, want the /// comment above the class", got)
	}

	m, ok := dartNamed(nodes, "bump")
	if !ok {
		t.Fatal("bump not extracted")
	}
	if got := string(dartSrc[m.DocStartByte:m.DocEndByte]); !strings.Contains(got, "Increment the counter") {
		t.Errorf("method doc span = %q, want the /// comment above it", got)
	}
}

func TestDart_ByteSpansAreValid(t *testing.T) {
	nodes, _ := dartExtract(t)

	if len(nodes) == 0 {
		t.Fatal("no nodes extracted")
	}
	for _, n := range nodes {
		if n.StartByte >= n.EndByte {
			t.Errorf("%q: inverted or empty span %d..%d", n.Name, n.StartByte, n.EndByte)
			continue
		}
		if n.EndByte > len(dartSrc) {
			t.Errorf("%q: span %d..%d runs past the source (%d bytes)", n.Name, n.StartByte, n.EndByte, len(dartSrc))
		}
		if n.EndLine < n.StartLine {
			t.Errorf("%q: inverted line range %d..%d", n.Name, n.StartLine, n.EndLine)
		}
	}

	if _, ok := dartNamed(nodes, "trailingHelper"); !ok {
		t.Error("the declaration nearest EOF was lost, which is what a parse cascade looks like")
	}
}

func TestDart_MalformedInputDoesNotPanic(t *testing.T) {
	for _, src := range []string{
		"",
		"   \n\n",
		"// just a comment\n",
		"class",
		"class A {",
		"void f(",
		"import",
		"import '",
		"}}}",
		"enum {}",
		"extension on {}",
		strings.Repeat("class A {} ", 200),
	} {
		if _, _, err := NewDart().Extract(context.Background(), "a.dart", []byte(src)); err != nil {
			t.Errorf("Extract(%q): unexpected error %v", src, err)
		}
	}
}

func TestDart_LanguageAndPath(t *testing.T) {
	nodes, _ := dartExtract(t)
	if len(nodes) == 0 {
		t.Fatal("no nodes extracted")
	}
	for _, n := range nodes {
		if n.Language != "dart" {
			t.Errorf("%q: language = %q, want dart", n.Name, n.Language)
		}
		if n.Path != "counter.dart" {
			t.Errorf("%q: path = %q, want counter.dart", n.Name, n.Path)
		}
	}
}

func TestDart_Extensions(t *testing.T) {
	e := NewDart()
	if got := e.Language(); got != "dart" {
		t.Errorf("Language() = %q, want dart", got)
	}
	if got := e.Extensions(); len(got) != 1 || got[0] != ".dart" {
		t.Errorf("Extensions() = %v, want [.dart]", got)
	}
}

// A binding's kind follows its MUTABILITY, not where it sits. KindField is for
// keys of a data-format file; a member of a code type is KindConstant when
// immutable and KindVariable otherwise, which is what
// TestExtractors_MemberConventions enforces package-wide. Dart marks the
// distinction plainly with final/const, and at file scope it puts that keyword
// in a SIBLING node — reading the declaration's own text would call every
// top-level binding mutable.
func TestDart_BindingKindFollowsMutability(t *testing.T) {
	src := []byte("const topConst = 1;\nfinal topFinal = 2;\nvar topVar = 3;\n\nclass C {\n  int mut = 0;\n  final String immut = 'x';\n  static const int SC = 4;\n  void m() { var localv = 5; }\n}\n")

	nodes, _, err := NewDart().Extract(context.Background(), "a.dart", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	want := map[string]topology.NodeKind{
		"topConst": topology.KindConstant,
		"topFinal": topology.KindConstant,
		"topVar":   topology.KindVariable,
		"mut":      topology.KindVariable,
		"immut":    topology.KindConstant,
		"SC":       topology.KindConstant,
	}
	for name, kind := range want {
		n, ok := dartNamed(nodes, name)
		if !ok {
			t.Errorf("%q not extracted", name)
			continue
		}
		if n.Kind != kind {
			t.Errorf("%q: kind = %q, want %q", name, n.Kind, kind)
		}
	}
	for _, n := range nodes {
		if n.Kind == topology.KindField {
			t.Errorf("%q emitted as KindField; a code-type member is never KindField", n.Name)
		}
	}
	if _, ok := dartNamed(nodes, "localv"); ok {
		t.Error("a function-local binding should not be surfaced")
	}
}
