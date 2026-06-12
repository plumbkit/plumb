package wasmts

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

func swiftNames(nodes []topology.Node, kind topology.NodeKind) []string {
	var out []string
	for _, n := range nodes {
		if n.Kind == kind {
			out = append(out, n.Name)
		}
	}
	return out
}

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
		if !slices.Contains(swiftNames(nodes, c.kind), c.name) {
			t.Errorf("kind=%s name=%q not found; got %v", c.kind, c.name, swiftNames(nodes, c.kind))
		}
	}
}

// TestSwift_ImplicitlyUnwrappedOptional is the regression for the AppKit/UIKit
// outline-collapse bug. Through the canonical grammar (WASM) the class, its
// members AND the `T!` in a parameter signature all survive — the latter is the
// fidelity the gotreesitter byte-blanking workaround could not preserve.
func TestSwift_ImplicitlyUnwrappedOptional(t *testing.T) {
	src := []byte(`import AppKit

class AppDelegate: NSObject, NSApplicationDelegate {
    private var menuBarManager: MenuBarManager!
    @IBOutlet var label: NSTextField!

    func applicationDidFinishLaunching(_ notification: Notification) {
        NSApp.setActivationPolicy(.accessory)
    }

    func configure(_ widget: Widget!) {}
}
`)
	nodes, _, err := NewSwift().Extract(context.Background(), "AppDelegate.swift", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !slices.Contains(swiftNames(nodes, topology.KindClass), "AppDelegate") {
		t.Errorf("class AppDelegate dropped; classes=%v", swiftNames(nodes, topology.KindClass))
	}
	if !slices.Contains(swiftNames(nodes, topology.KindMethod), "applicationDidFinishLaunching") {
		t.Errorf("method applicationDidFinishLaunching dropped; methods=%v", swiftNames(nodes, topology.KindMethod))
	}
	if !slices.Contains(swiftNames(nodes, topology.KindVariable), "menuBarManager") {
		t.Errorf("property menuBarManager dropped; variables=%v", swiftNames(nodes, topology.KindVariable))
	}
	if !slices.Contains(swiftNames(nodes, topology.KindVariable), "label") {
		t.Errorf("property label (@IBOutlet) dropped; variables=%v", swiftNames(nodes, topology.KindVariable))
	}
	// Fidelity the workaround lost: the IUO `!` survives in the signature.
	var sig string
	for _, n := range nodes {
		if n.Kind == topology.KindMethod && n.Name == "configure" {
			sig = n.Signature
		}
	}
	if !strings.Contains(sig, "Widget!") {
		t.Errorf("IUO `!` not preserved in parameter signature; got %q", sig)
	}
}

// TestSwift_InitDeinitSubscript confirms non-identifier-named members are
// extracted (the gotreesitter extractor missed these entirely).
func TestSwift_InitDeinitSubscript(t *testing.T) {
	src := []byte(`struct Matrix {
    let rows: Int
    init(rows: Int) { self.rows = rows }
    init?(text: String) { self.rows = 0 }
    subscript(i: Int) -> Int { rows }
}

final class Handle {
    deinit { cleanup() }
    func cleanup() {}
}
`)
	nodes, _, err := NewSwift().Extract(context.Background(), "m.swift", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	methods := swiftNames(nodes, topology.KindMethod)
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
}

// TestSwift_OperatorsAndTypealias confirms operator functions (named by their
// operator token) and typealiases are extracted.
func TestSwift_OperatorsAndTypealias(t *testing.T) {
	src := []byte(`typealias Handler = (Int) -> Void

struct Vec: Equatable {
    let x: Double
    typealias Scalar = Double
    static func == (l: Vec, r: Vec) -> Bool { l.x == r.x }
    static func + (l: Vec, r: Vec) -> Vec { l }
}
`)
	nodes, _, err := NewSwift().Extract(context.Background(), "v.swift", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	types := swiftNames(nodes, topology.KindType)
	for _, want := range []string{"Handler", "Scalar"} {
		if !slices.Contains(types, want) {
			t.Errorf("typealias %q not extracted; types=%v", want, types)
		}
	}
	methods := swiftNames(nodes, topology.KindMethod)
	for _, want := range []string{"==", "+"} {
		if !slices.Contains(methods, want) {
			t.Errorf("operator method %q not extracted; methods=%v", want, methods)
		}
	}
}

// TestSwift_Containment confirms members are contained by their enclosing type.
func TestSwift_Containment(t *testing.T) {
	nodes, edges, err := NewSwift().Extract(context.Background(), "svc.swift", swiftSrc)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	idxOf := func(kind topology.NodeKind, name string) int64 {
		for i, n := range nodes {
			if n.Kind == kind && n.Name == name {
				return int64(i)
			}
		}
		return -1
	}
	point := idxOf(topology.KindClass, "Point")
	norm := idxOf(topology.KindMethod, "norm")
	if point < 0 || norm < 0 {
		t.Fatalf("Point=%d norm=%d", point, norm)
	}
	found := false
	for _, e := range edges {
		if e.Kind == topology.EdgeContains && e.FromID == point && e.ToID == norm {
			found = true
		}
	}
	if !found {
		t.Errorf("missing containment edge Point→norm")
	}
}

// TestSwift_CallEdge confirms intra-file call edges (norm → scale).
func TestSwift_CallEdge(t *testing.T) {
	nodes, edges, err := NewSwift().Extract(context.Background(), "svc.swift", swiftSrc)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	idxOf := func(name string) int64 {
		for i, n := range nodes {
			if n.Name == name && (n.Kind == topology.KindMethod || n.Kind == topology.KindFunction) {
				return int64(i)
			}
		}
		return -1
	}
	norm, scale := idxOf("norm"), idxOf("scale")
	found := false
	for _, e := range edges {
		if e.Kind == topology.EdgeCalls && e.FromID == norm && e.ToID == scale {
			found = true
		}
	}
	if !found {
		t.Errorf("missing call edge norm→scale; edges=%v", edges)
	}
}
