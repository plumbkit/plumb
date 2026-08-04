package treesitter

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

var swiftSrc = []byte(`import Foundation
import UIKit

let MAX_RETRIES = 3
var counter: Int = 0

struct Point {
    let x: Double
    let y: Double

    func norm() -> Double {
        return scale(x)
    }

    func scale(_ v: Double) -> Double {
        return v * 2
    }
}

protocol Notifier {
    func notify(message: String) -> Bool
    var channel: String { get }
}

enum Role {
    case admin
    case user
    case guest
}

class UserService: Notifier {
    private var cache: [String: User] = [:]
    var channel: String = "default"

    func notify(message: String) -> Bool {
        return !message.isEmpty
    }
}

extension UserService {
    func count() -> Int {
        return cache.count
    }
}

func makeService() -> UserService {
    return UserService()
}

class CalcTests: XCTestCase {
    func testAddition() {
        XCTAssertEqual(2 + 2, 4)
    }

    func helper() -> Int {
        return 1
    }
}
`)

func TestSwift_KindsExtracted(t *testing.T) {
	nodes, _, err := NewSwift().Extract(context.Background(), "Sources/svc.swift", swiftSrc)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	cases := []struct {
		kind topology.NodeKind
		name string
	}{
		{topology.KindImport, "Foundation"},
		{topology.KindConstant, "MAX_RETRIES"},
		{topology.KindVariable, "counter"},
		{topology.KindClass, "Point"},    // struct → KindClass
		{topology.KindType, "Notifier"},  // protocol → KindType
		{topology.KindClass, "Role"},     // enum → KindClass
		{topology.KindConstant, "admin"}, // enum case
		{topology.KindClass, "UserService"},
		{topology.KindClass, "CalcTests"},
		{topology.KindMethod, "norm"},
		{topology.KindMethod, "scale"},
		{topology.KindMethod, "notify"},
		{topology.KindMethod, "count"},     // extension method
		{topology.KindConstant, "x"},       // struct let property
		{topology.KindVariable, "channel"}, // class var property
		{topology.KindFunction, "makeService"},
		{topology.KindTest, "testAddition"}, // XCTest method
		{topology.KindMethod, "helper"},     // non-test method in test class
	}
	for _, c := range cases {
		if !slices.Contains(names(nodes, c.kind), c.name) {
			t.Errorf("kind=%s name=%q not found; got %v", c.kind, c.name, names(nodes, c.kind))
		}
	}
}

// TestSwift_ImplicitlyUnwrappedOptionalProperty is the regression test for the
// AppKit/UIKit outline-collapse bug: the grammar cannot parse an implicitly-
// unwrapped optional type (`var x: T!`) and emits an ERROR that cascades and
// drops the entire enclosing class and its members. The extractor's reparse
// recovery must restore the class, the property, and the methods. Mirrors the
// reported NoCaps AppDelegate.
func TestSwift_ImplicitlyUnwrappedOptionalProperty(t *testing.T) {
	src := []byte(`import AppKit

class AppDelegate: NSObject, NSApplicationDelegate {
    private var menuBarManager: MenuBarManager!
    @IBOutlet var label: NSTextField!

    func applicationDidFinishLaunching(_ notification: Notification) {
        NSApp.setActivationPolicy(.accessory)
    }
}
`)
	nodes, _, err := NewSwift().Extract(context.Background(), "AppDelegate.swift", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !slices.Contains(names(nodes, topology.KindClass), "AppDelegate") {
		t.Errorf("class AppDelegate dropped — IUO `T!` property collapsed the type; classes=%v", names(nodes, topology.KindClass))
	}
	if !slices.Contains(names(nodes, topology.KindMethod), "applicationDidFinishLaunching") {
		t.Errorf("method applicationDidFinishLaunching dropped; methods=%v", names(nodes, topology.KindMethod))
	}
	if !slices.Contains(names(nodes, topology.KindVariable), "menuBarManager") {
		t.Errorf("property menuBarManager dropped; variables=%v", names(nodes, topology.KindVariable))
	}
}

func TestSwift_MethodContainmentCertain(t *testing.T) {
	nodes, edges, err := NewSwift().Extract(context.Background(), "svc.swift", swiftSrc)
	if err != nil {
		t.Fatal(err)
	}
	var pointIdx, normIdx int64 = -1, -1
	for i, n := range nodes {
		switch {
		case n.Kind == topology.KindClass && n.Name == "Point":
			pointIdx = int64(i)
		case n.Kind == topology.KindMethod && n.Name == "norm":
			normIdx = int64(i)
		}
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeContains && e.FromID == pointIdx && e.ToID == normIdx {
			if e.Confidence != 1.0 || e.Source != "extractor" {
				t.Errorf("contains edge conf=%v src=%q, want 1.0/extractor", e.Confidence, e.Source)
			}
			return
		}
	}
	t.Errorf("no contains edge Point→norm; edges=%v", edges)
}

func TestSwift_CallEdgeIntraFile(t *testing.T) {
	nodes, edges, err := NewSwift().Extract(context.Background(), "svc.swift", swiftSrc)
	if err != nil {
		t.Fatal(err)
	}
	var normIdx, scaleIdx int64 = -1, -1
	for i, n := range nodes {
		switch n.Name {
		case "norm":
			normIdx = int64(i)
		case "scale":
			scaleIdx = int64(i)
		}
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeCalls && e.FromID == normIdx && e.ToID == scaleIdx {
			if e.Confidence != 0.8 || e.Source != "heuristic" {
				t.Errorf("call edge conf=%v src=%q, want 0.8/heuristic", e.Confidence, e.Source)
			}
			return
		}
	}
	t.Errorf("no call edge norm→scale; edges=%v", edges)
}

func TestSwift_LetVsVar(t *testing.T) {
	nodes, _, err := NewSwift().Extract(context.Background(), "svc.swift", swiftSrc)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(names(nodes, topology.KindConstant), "MAX_RETRIES") {
		t.Error("let MAX_RETRIES should be a constant")
	}
	if !slices.Contains(names(nodes, topology.KindVariable), "counter") {
		t.Error("var counter should be a variable")
	}
	if slices.Contains(names(nodes, topology.KindConstant), "Point") {
		t.Error("Point is a struct/class, not a constant")
	}
}

func TestSwift_TestDetectionRequiresXCTestCase(t *testing.T) {
	nodes, _, err := NewSwift().Extract(context.Background(), "svc.swift", swiftSrc)
	if err != nil {
		t.Fatal(err)
	}
	tests := names(nodes, topology.KindTest)
	if !slices.Contains(tests, "testAddition") {
		t.Errorf("testAddition in an XCTestCase subclass should be a test; tests=%v", tests)
	}
	// A non-test-prefixed method in a test class must remain a method.
	if slices.Contains(tests, "helper") {
		t.Error("helper() is not test-prefixed and must not be a test")
	}
	// A test-prefixed-looking method NOT in an XCTestCase subclass must not be a test:
	// `norm`/`scale`/`notify`/`count` live in non-test types and must be methods.
	for _, n := range []string{"norm", "scale", "notify", "count"} {
		if slices.Contains(tests, n) {
			t.Errorf("%s is outside an XCTestCase subclass and must not be a test", n)
		}
	}
}

func TestSwift_LocalNotExtracted(t *testing.T) {
	src := []byte("func outer() {\n    let local = 5\n    var tmp = 1\n}\n")
	nodes, _, err := NewSwift().Extract(context.Background(), "x.swift", src)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []topology.NodeKind{topology.KindConstant, topology.KindVariable} {
		for _, n := range []string{"local", "tmp"} {
			if slices.Contains(names(nodes, kind), n) {
				t.Errorf("local %q inside a function body must not be extracted as %s", n, kind)
			}
		}
	}
}

func TestSwift_EndLineRecorded(t *testing.T) {
	nodes, _, err := NewSwift().Extract(context.Background(), "svc.swift", swiftSrc)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.Kind == topology.KindClass && n.Name == "UserService" {
			if n.EndLine <= n.StartLine {
				t.Errorf("UserService EndLine=%d should exceed StartLine=%d", n.EndLine, n.StartLine)
			}
			return
		}
	}
	t.Fatal("UserService node not found")
}

func TestSwift_EmptyAndCommentOnly(t *testing.T) {
	for _, src := range [][]byte{[]byte(""), []byte("// just a comment\n// more\n")} {
		nodes, edges, err := NewSwift().Extract(context.Background(), "e.swift", src)
		if err != nil {
			t.Fatalf("Extract: %v", err)
		}
		if len(nodes) != 0 || len(edges) != 0 {
			t.Errorf("src=%q: want 0 nodes/edges, got %d/%d", src, len(nodes), len(edges))
		}
	}
}

func TestSwift_LanguageAndPath(t *testing.T) {
	nodes, _, err := NewSwift().Extract(context.Background(), "Sources/svc.swift", swiftSrc)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.Language != "swift" {
			t.Errorf("node %q language=%q, want swift", n.Name, n.Language)
		}
		if n.Path != "Sources/svc.swift" {
			t.Errorf("node %q path=%q, want Sources/svc.swift", n.Name, n.Path)
		}
	}
}

func TestSwift_Extensions(t *testing.T) {
	if !slices.Contains(NewSwift().Extensions(), ".swift") {
		t.Error(".swift missing from Swift Extensions()")
	}
}

func TestSwift_Signature(t *testing.T) {
	// Vapor-style function — signature must contain the parameter type so that
	// topology_routes can pattern-match on "RoutesBuilder" or "Application".
	src := []byte(`
struct Routes: RouteCollection {
    func boot(routes: RoutesBuilder) throws {
        routes.get("ping") { _ in "pong" }
    }
}
`)
	nodes, _, err := NewSwift().Extract(context.Background(), "Sources/Routes.swift", src)
	if err != nil {
		t.Fatal(err)
	}
	var boot *topology.Node
	for i := range nodes {
		if nodes[i].Name == "boot" {
			boot = &nodes[i]
			break
		}
	}
	if boot == nil {
		t.Fatal("boot method not extracted")
	}
	if boot.Signature == "" {
		t.Error("Signature is empty — funcSignature not wired into addFunc")
	}
	if !strings.Contains(boot.Signature, "RoutesBuilder") {
		t.Errorf("Signature %q does not contain 'RoutesBuilder'", boot.Signature)
	}
}

func TestSwift_ParsableCommandConformanceInMethodSignature(t *testing.T) {
	// An ArgumentParser command conforms to ParsableCommand on the TYPE; its entry
	// point is run(). The extractor must surface that conformance on the method's
	// signature so topology_routes can detect the CLI entry point.
	src := []byte(`
struct Hello: ParsableCommand {
    func run() throws {
        print("hi")
    }

    func validate() throws {}
}
`)
	nodes, _, err := NewSwift().Extract(context.Background(), "Sources/Hello.swift", src)
	if err != nil {
		t.Fatal(err)
	}
	var run *topology.Node
	for i := range nodes {
		if nodes[i].Name == "run" {
			run = &nodes[i]
			break
		}
	}
	if run == nil {
		t.Fatal("run method not extracted")
	}
	if !strings.Contains(run.Signature, "ParsableCommand") {
		t.Errorf("run Signature %q does not carry the enclosing type's ParsableCommand conformance", run.Signature)
	}
}

// TestSwift_InitDeinitSubscript confirms non-identifier-named members are
// extracted under their fixed names (ported from the wasmts suite — the
// pure-Go extractor missed these entirely before the walk port).
func TestSwift_InitDeinitSubscript(t *testing.T) {
	src := []byte(`struct Matrix {
    let rows: Int
    init(rows: Int) { self.rows = rows }
    init?(text: String) { self.rows = 0 }
    subscript(i: Int) -> Int { rows }
    func makeIt() -> Matrix { Matrix.init(rows: rows) }
}

final class Handle {
    deinit { cleanup() }
    func cleanup() {}
}
`)
	nodes, edges, err := NewSwift().Extract(context.Background(), "m.swift", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	methods := names(nodes, topology.KindMethod)
	for _, want := range []string{"init", "deinit", "subscript", "cleanup"} {
		if !slices.Contains(methods, want) {
			t.Errorf("member %q not extracted; methods=%v", want, methods)
		}
	}
	// Two inits both surface (Matrix has a designated and a failable init).
	initCount := 0
	for _, m := range methods {
		if m == "init" {
			initCount++
		}
	}
	if initCount != 2 {
		t.Errorf("expected 2 init members, got %d (methods=%v)", initCount, methods)
	}
	// The new dispatch cases must not double-emit: a declaration handled by its
	// own case AND reached again via default-descent would appear twice at the
	// same position (the two inits legitimately share a name, so key by line).
	seen := map[string]int{}
	for _, n := range nodes {
		seen[string(n.Kind)+"|"+n.Name+"|"+strconv.Itoa(n.StartLine)]++
	}
	for key, count := range seen {
		if count > 1 {
			t.Errorf("%s emitted %d times, want once", key, count)
		}
	}
	// A named member is registered for call resolution like any other callable:
	// a sibling method calling `Matrix.init(…)` gets an EdgeCalls into an init
	// node. Nothing else in this suite observes that registration — dropping it
	// silently loses every init/deinit/subscript call edge the wasm walk emits.
	callToInit := false
	for _, e := range edges {
		if e.Kind == topology.EdgeCalls && nodes[e.FromID].Name == "makeIt" && nodes[e.ToID].Name == "init" {
			callToInit = true
		}
	}
	if !callToInit {
		t.Errorf("no EdgeCalls from makeIt to init — named members are not registered for call resolution; edges=%+v", edges)
	}
}

// TestSwift_OperatorsAndTypealias confirms operator functions (named by their
// operator token) and typealiases are extracted (ported from the wasmts suite).
func TestSwift_OperatorsAndTypealias(t *testing.T) {
	src := []byte(`typealias Handler = (Int) -> Void

infix operator <^>
func <^> (l: Int, r: Int) -> Int { l + r }

struct Vec: Equatable {
    let x: Double
    typealias Scalar = Double
    static func == (l: Vec, r: Vec) -> Bool { l.x == r.x }
    static func + (l: Vec, r: Vec) -> Vec { l }
}
`)
	nodes, edges, err := NewSwift().Extract(context.Background(), "v.swift", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	types := names(nodes, topology.KindType)
	for _, want := range []string{"Handler", "Scalar"} {
		if !slices.Contains(types, want) {
			t.Errorf("typealias %q not extracted; types=%v", want, types)
		}
	}
	methods := names(nodes, topology.KindMethod)
	for _, want := range []string{"==", "+"} {
		if !slices.Contains(methods, want) {
			t.Errorf("operator method %q not extracted; methods=%v", want, methods)
		}
	}
	// A file-scope custom operator function goes through the same operatorName
	// fallback but at enclosing == -1, so it must surface as a function.
	if !slices.Contains(names(nodes, topology.KindFunction), "<^>") {
		t.Errorf("file-scope operator function <^> not extracted; functions=%v", names(nodes, topology.KindFunction))
	}
	// The member typealias is contained by its type; the file-scope one is not.
	if !containedIn(t, nodes, edges, "Vec", "Scalar") {
		t.Error("member typealias Scalar not contained by Vec")
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeContains && nodes[e.ToID].Name == "Handler" {
			t.Errorf("file-scope typealias Handler must have no container; got edge from %q", nodes[e.FromID].Name)
		}
	}
}

// TestSwift_NamedMemberBodyLocalsSuppressed pins the locals-suppression
// invariant for the callable kinds that are NOT function_declaration: before
// the walk port, init/deinit/subscript bodies fell through the default case
// with the enclosing type still set, so `let x = …` inside an initialiser
// leaked into the index as a type member. TestSwift_LocalNotExtracted only
// covers func bodies and stayed green through that leak.
func TestSwift_NamedMemberBodyLocalsSuppressed(t *testing.T) {
	src := []byte(`class Grid {
    let rows: Int
    init(rows: Int) {
        let cached = rows
        var scratch = cached
        typealias LocalAlias = Int
        self.rows = scratch
    }
    deinit {
        let handle = 1
    }
    subscript(i: Int) -> Int {
        get {
            let offset = 1
            typealias Cell = Int
            return i + offset
        }
    }
}
`)
	nodes, _, err := NewSwift().Extract(context.Background(), "g.swift", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	// Anti-vacuity: the class, all three named members, and the real member
	// property must be present — otherwise the absence checks below pass on an
	// empty extraction.
	if !slices.Contains(names(nodes, topology.KindClass), "Grid") {
		t.Fatalf("class Grid missing; nodes=%+v", nodes)
	}
	methods := names(nodes, topology.KindMethod)
	for _, want := range []string{"init", "deinit", "subscript"} {
		if !slices.Contains(methods, want) {
			t.Fatalf("member %q missing; methods=%v", want, methods)
		}
	}
	if !slices.Contains(names(nodes, topology.KindConstant), "rows") {
		t.Errorf("member property rows should be extracted; constants=%v", names(nodes, topology.KindConstant))
	}
	for _, n := range nodes {
		switch n.Name {
		case "cached", "scratch", "handle", "offset", "LocalAlias", "Cell":
			t.Errorf("local %q inside a named-member body leaked into the index as %s", n.Name, n.Kind)
		}
	}
}

// TestSwift_MultiCaseEnumEntry: one enum_entry can bind several cases
// (`case a, b`); every bound identifier must surface as a constant contained
// by the enum, carrying the whole entry's span rather than its own
// identifier's — the entry is split across lines here so the two are
// distinguishable. Before the port only the first identifier was taken.
func TestSwift_MultiCaseEnumEntry(t *testing.T) {
	src := []byte("enum Compass {\n    case north,\n         south\n    case east\n}\n")
	nodes, edges, err := NewSwift().Extract(context.Background(), "c.swift", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	consts := names(nodes, topology.KindConstant)
	for _, want := range []string{"north", "south", "east"} {
		if !slices.Contains(consts, want) {
			t.Errorf("enum case %q not extracted; constants=%v", want, consts)
		}
		if !containedIn(t, nodes, edges, "Compass", want) {
			t.Errorf("enum case %q not contained by Compass", want)
		}
	}
	for _, n := range nodes {
		if n.Name != "north" && n.Name != "south" {
			continue
		}
		if n.StartLine != 2 || n.EndLine != 3 {
			t.Errorf("case %s span = %d-%d, want 2-3 (the enum_entry's span)", n.Name, n.StartLine, n.EndLine)
		}
	}
}

// containedIn reports whether a contains edge links the named container to the
// named member.
func containedIn(t *testing.T, nodes []topology.Node, edges []topology.Edge, container, member string) bool {
	t.Helper()
	for _, e := range edges {
		if e.Kind != topology.EdgeContains {
			continue
		}
		if nodes[e.FromID].Name == container && nodes[e.ToID].Name == member {
			return true
		}
	}
	return false
}

// TestSwift_SubscriptSignatureAndConformance: a subscript's body is a
// computed_property, not a function_body — funcSignature must stop there or
// the whole accessor block bleeds into the signature. The enclosing type's
// conformance suffix applies to named members exactly as it does to methods.
func TestSwift_SubscriptSignatureAndConformance(t *testing.T) {
	src := []byte(`struct Deck: Collection {
    subscript(i: Int) -> Int {
        get { return i }
    }
}
`)
	nodes, _, err := NewSwift().Extract(context.Background(), "d.swift", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	var sub *topology.Node
	for i := range nodes {
		if nodes[i].Name == "subscript" {
			sub = &nodes[i]
			break
		}
	}
	if sub == nil {
		t.Fatalf("subscript not extracted; nodes=%+v", nodes)
	}
	if !strings.Contains(sub.Signature, "Int") {
		t.Errorf("subscript Signature %q lost the parameter/return head", sub.Signature)
	}
	if strings.Contains(sub.Signature, "get") || strings.Contains(sub.Signature, "return") {
		t.Errorf("subscript Signature %q bleeds into the accessor body — computed_property break missing", sub.Signature)
	}
	if !strings.Contains(sub.Signature, "Collection") {
		t.Errorf("subscript Signature %q missing the enclosing type's Collection conformance suffix", sub.Signature)
	}
}

// TestSwift_ProtocolRequirementMembers: in the gotreesitter grammar a
// protocol's init/subscript requirements parse as plain init_declaration /
// subscript_declaration (no protocol_* variants exist), and a protocol-body
// typealias as typealias_declaration — all must surface as members of the
// protocol.
func TestSwift_ProtocolRequirementMembers(t *testing.T) {
	src := []byte(`protocol Storage {
    init(capacity: Int)
    subscript(key: String) -> Int { get }
    typealias Key = String
}
`)
	nodes, edges, err := NewSwift().Extract(context.Background(), "s.swift", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !slices.Contains(names(nodes, topology.KindType), "Storage") {
		t.Fatalf("protocol Storage missing; nodes=%+v", nodes)
	}
	for _, want := range []string{"init", "subscript"} {
		if !slices.Contains(names(nodes, topology.KindMethod), want) {
			t.Errorf("protocol requirement %q not extracted; methods=%v", want, names(nodes, topology.KindMethod))
		}
		if !containedIn(t, nodes, edges, "Storage", want) {
			t.Errorf("protocol requirement %q not contained by Storage", want)
		}
	}
	if !slices.Contains(names(nodes, topology.KindType), "Key") {
		t.Errorf("protocol typealias Key not extracted; types=%v", names(nodes, topology.KindType))
	}
}
