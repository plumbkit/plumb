package treesitter

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/golimpio/plumb/internal/topology"
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
