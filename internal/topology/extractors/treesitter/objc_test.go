package treesitter

import (
	"context"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

// objcSrc is the fidelity fixture. It deliberately avoids the two shapes the
// grammar is known to mis-parse — a bare function-like macro with no semicolon
// ahead of a class (NS_ASSUME_NONNULL_BEGIN), and an `NS_ENUM` body — because
// those have tests of their own below and a fidelity sample must isolate a
// grammar cascade rather than re-measure a defect already pinned.
var objcSrc = []byte(`#import <Foundation/Foundation.h>
#import "MTLModel.h"
@import CoreData;
@class MTLReachability, MTLQueue;

static NSString *const kDefaultName = @"anon";

typedef void (^MTLCompletion)(NSError *error);

@protocol MTLSerialising <NSObject>
@required
- (NSString *)serialise;
@optional
- (void)reset;
@end

@interface MTLWidget () <MTLSerialising>
@property (nonatomic, copy) NSString *secretName;
@end

@interface MTLWidget (Debugging)
- (NSString *)debugSummary;
@end

@implementation MTLWidget {
    NSInteger _tickCount;
}

@synthesize secretName = _secretName;
@dynamic tally;

// Builds the widget's display name.
- (NSString *)displayNameForLocale:(NSLocale *)locale fallback:(NSString *)fallback {
    NSString *local = [self serialise];
    return local ?: fallback;
}

- (NSString *)serialise {
    return kDefaultName;
}

+ (instancetype)sharedWidget {
    return nil;
}

- (void)runBlock {
    MTLCompletion done = ^(NSError *error) { NSLog(@"%@", error); };
    done(nil);
}
@end

@implementation MTLWidget (Debugging)
- (NSString *)debugSummary {
    return [self serialise];
}
@end
`)

func objcExtract(t *testing.T, src []byte) ([]topology.Node, []topology.Edge) {
	t.Helper()
	nodes, edges, err := NewObjC().Extract(context.Background(), "src/MTLWidget.m", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return nodes, edges
}

func objcFind(t *testing.T, nodes []topology.Node, kind topology.NodeKind, name string) topology.Node {
	t.Helper()
	for _, n := range nodes {
		if n.Kind == kind && n.Name == name {
			return n
		}
	}
	t.Fatalf("no %s named %q; got %s", kind, name, objcSummary(nodes))
	return topology.Node{}
}

func objcHas(nodes []topology.Node, kind topology.NodeKind, name string) bool {
	for _, n := range nodes {
		if n.Kind == kind && n.Name == name {
			return true
		}
	}
	return false
}

func objcSummary(nodes []topology.Node) string {
	parts := make([]string, 0, len(nodes))
	for _, n := range nodes {
		parts = append(parts, string(n.Kind)+":"+n.Name)
	}
	return strings.Join(parts, " ")
}

func TestObjc_LanguageAndExtensions(t *testing.T) {
	e := NewObjC()
	if e.Language() != "objc" {
		t.Fatalf("Language() = %q", e.Language())
	}
	// A `.h` is C's, deliberately: nothing in the file says which language it is.
	if got := strings.Join(e.Extensions(), ","); got != ".m,.mm" {
		t.Fatalf("Extensions() = %q", got)
	}
}

func TestObjc_Containers(t *testing.T) {
	nodes, _ := objcExtract(t, objcSrc)

	// A category keeps its canonical `Class(Category)` spelling so it stays a
	// symbol distinct from the class it extends.
	for _, name := range []string{"MTLWidget", "MTLWidget(Debugging)"} {
		if !objcHas(nodes, topology.KindClass, name) {
			t.Fatalf("missing class %q; got %s", name, objcSummary(nodes))
		}
	}
	// A protocol is a contract, not a class — the Java/Kotlin interface precedent.
	proto := objcFind(t, nodes, topology.KindType, "MTLSerialising")
	if !strings.Contains(proto.Signature, "<NSObject>") {
		t.Fatalf("protocol signature lost its conformance: %q", proto.Signature)
	}
	// The class extension and the implementation both declare MTLWidget, so both
	// are emitted; their signatures are what tell them apart.
	var sigs []string
	for _, n := range nodes {
		if n.Kind == topology.KindClass && n.Name == "MTLWidget" {
			sigs = append(sigs, n.Signature)
		}
	}
	if len(sigs) != 2 {
		t.Fatalf("want the extension and the implementation, got %v", sigs)
	}
	if !strings.Contains(strings.Join(sigs, "|"), "@interface MTLWidget ()") ||
		!strings.Contains(strings.Join(sigs, "|"), "@implementation MTLWidget") {
		t.Fatalf("signatures = %v", sigs)
	}
}

func TestObjc_MethodsUseFullSelectors(t *testing.T) {
	nodes, _ := objcExtract(t, objcSrc)

	m := objcFind(t, nodes, topology.KindMethod, "displayNameForLocale:fallback:")
	if m.Qualified != "-[MTLWidget displayNameForLocale:fallback:]" {
		t.Fatalf("Qualified = %q", m.Qualified)
	}
	if !strings.HasPrefix(m.Signature, "- (NSString *)displayNameForLocale:") {
		t.Fatalf("Signature = %q", m.Signature)
	}
	if strings.Contains(m.Signature, "{") {
		t.Fatalf("signature must stop before the body: %q", m.Signature)
	}
	// A class method is spelled with `+`, which is the whole reason the bracket
	// form is used instead of a dot.
	shared := objcFind(t, nodes, topology.KindMethod, "sharedWidget")
	if shared.Qualified != "+[MTLWidget sharedWidget]" {
		t.Fatalf("class-method Qualified = %q", shared.Qualified)
	}
	// A category's methods qualify under the category, not the bare class.
	summary := objcFind(t, nodes, topology.KindMethod, "debugSummary")
	if !strings.Contains(summary.Qualified, "MTLWidget(Debugging)") {
		t.Fatalf("category method Qualified = %q", summary.Qualified)
	}
	// A protocol's `@required` / `@optional` members are grouped under a node of
	// their own; both must survive that extra level.
	for _, sel := range []string{"serialise", "reset"} {
		if !objcHas(nodes, topology.KindMethod, sel) {
			t.Fatalf("missing protocol method %q; got %s", sel, objcSummary(nodes))
		}
	}
}

func TestObjc_Fields(t *testing.T) {
	nodes, _ := objcExtract(t, objcSrc)

	// A mutable property is KindVariable, not KindField: that kind is reserved
	// for keys of data-format files, and topology.KindField's doc comment says a
	// member of a code type is KindConstant when immutable and KindVariable
	// otherwise. TestExtractors_MemberConventions enforces this package-wide.
	prop := objcFind(t, nodes, topology.KindVariable, "secretName")
	if prop.Qualified != "MTLWidget.secretName" {
		t.Fatalf("property Qualified = %q", prop.Qualified)
	}
	ivar := objcFind(t, nodes, topology.KindVariable, "_tickCount")
	if ivar.Qualified != "MTLWidget._tickCount" {
		t.Fatalf("ivar Qualified = %q", ivar.Qualified)
	}
	// @dynamic is the only mention of `tally` in the file, so dropping it would
	// leave the property with no symbol at all.
	if !objcHas(nodes, topology.KindVariable, "tally") {
		t.Fatalf("@dynamic dropped; got %s", objcSummary(nodes))
	}
}

func TestObjc_Imports(t *testing.T) {
	nodes, _ := objcExtract(t, objcSrc)

	for _, name := range []string{
		"Foundation/Foundation.h", // #import <…>
		"MTLModel.h",              // #import "…"
		"CoreData",                // @import
		"MTLReachability",         // @class — a dependency, not a definition
		"MTLQueue",
	} {
		if !objcHas(nodes, topology.KindImport, name) {
			t.Fatalf("missing import %q; got %s", name, objcSummary(nodes))
		}
	}
	// A forward declaration must NOT masquerade as a definition.
	if objcHas(nodes, topology.KindClass, "MTLReachability") ||
		objcHas(nodes, topology.KindType, "MTLReachability") {
		t.Fatalf("@class emitted a definition-shaped node: %s", objcSummary(nodes))
	}
}

func TestObjc_InheritsCConstructs(t *testing.T) {
	nodes, _ := objcExtract(t, objcSrc)

	// KindVariable, not KindConstant: cWalk decides constness from a DIRECT
	// type_qualifier child, and in `static NSString *const k` the `const` nests
	// inside the pointer declarator. That is C's inherited behaviour, recorded
	// here rather than papered over in the Objective-C walk.
	if !objcHas(nodes, topology.KindVariable, "kDefaultName") {
		t.Fatalf("file-scope const dropped; got %s", objcSummary(nodes))
	}
	// A block typedef buries its name two declarators deep, out of reach of C's
	// scan — the shape every asynchronous Objective-C API uses for its callback.
	if !objcHas(nodes, topology.KindType, "MTLCompletion") {
		t.Fatalf("block typedef dropped; got %s", objcSummary(nodes))
	}
}

func TestObjc_LocalsAreSuppressed(t *testing.T) {
	nodes, _ := objcExtract(t, objcSrc)

	for _, n := range nodes {
		switch n.Name {
		case "local", "done", "fallback", "locale", "error":
			t.Fatalf("local %q leaked as %s", n.Name, n.Kind)
		}
	}
}

func TestObjc_ContainmentEdges(t *testing.T) {
	nodes, edges := objcExtract(t, objcSrc)

	idxOf := func(kind topology.NodeKind, name string) int64 {
		for i, n := range nodes {
			if n.Kind == kind && n.Name == name {
				return int64(i)
			}
		}
		t.Fatalf("no %s %q", kind, name)
		return -1
	}
	want := [][2]int64{
		{idxOf(topology.KindClass, "MTLWidget(Debugging)"), idxOf(topology.KindMethod, "debugSummary")},
		{idxOf(topology.KindType, "MTLSerialising"), idxOf(topology.KindMethod, "reset")},
	}
	for _, w := range want {
		found := false
		for _, e := range edges {
			if e.Kind == topology.EdgeContains && e.FromID == w[0] && e.ToID == w[1] {
				found = true
				if e.Confidence != 1.0 || e.Source != "extractor" {
					t.Fatalf("containment edge %v: confidence=%v source=%q", w, e.Confidence, e.Source)
				}
			}
		}
		if !found {
			t.Fatalf("missing containment edge %v", w)
		}
	}
}

func TestObjc_MessageSendCallEdges(t *testing.T) {
	src := []byte(`@implementation Foo
- (void)outer {
    [self innerWithValue:1 flag:NO];
    helperFunction(3);
}
- (void)innerWithValue:(NSInteger)v flag:(BOOL)f {}
@end

void helperFunction(int v) {}
`)
	nodes, edges := objcExtract(t, src)

	idxOf := func(name string) int64 {
		for i, n := range nodes {
			if n.Name == name {
				return int64(i)
			}
		}
		t.Fatalf("no node %q in %s", name, objcSummary(nodes))
		return -1
	}
	outer := idxOf("outer")
	// A message send resolves through the FULL selector, which is the only way
	// `innerWithValue:flag:` is told from any other `innerWithValue…` method.
	for _, target := range []string{"innerWithValue:flag:", "helperFunction"} {
		to := idxOf(target)
		found := false
		for _, e := range edges {
			if e.Kind == topology.EdgeCalls && e.FromID == outer && e.ToID == to {
				found = true
				if e.Confidence != 0.8 || e.Source != "heuristic" {
					t.Fatalf("call edge to %s: confidence=%v source=%q", target, e.Confidence, e.Source)
				}
			}
		}
		if !found {
			t.Fatalf("missing call edge outer → %s", target)
		}
	}
}

func TestObjc_UnaryMessageCallEdge(t *testing.T) {
	src := []byte(`@implementation Foo
- (void)outer { [self reload]; }
- (void)reload {}
@end
`)
	nodes, edges := objcExtract(t, src)
	if len(edges) == 0 {
		t.Fatalf("no edges from a unary message; nodes = %s", objcSummary(nodes))
	}
	var calls int
	for _, e := range edges {
		if e.Kind == topology.EdgeCalls {
			calls++
		}
	}
	if calls != 1 {
		t.Fatalf("want exactly one call edge, got %d", calls)
	}
}

func TestObjc_TestDetection(t *testing.T) {
	src := []byte(`@interface WidgetTests : XCTestCase
@end

@implementation WidgetTests
- (void)setUp {}
- (void)testRoundTrip { }
- (void)testEncodesNil:(id)x { }
@end
`)
	nodes, _ := objcExtract(t, src)

	for _, sel := range []string{"testRoundTrip", "testEncodesNil:"} {
		if !objcHas(nodes, topology.KindTest, sel) {
			t.Fatalf("%q not a test; got %s", sel, objcSummary(nodes))
		}
	}
	// setUp is a fixture hook, not a test — only a `test…` selector qualifies.
	if !objcHas(nodes, topology.KindMethod, "setUp") {
		t.Fatalf("setUp misclassified; got %s", objcSummary(nodes))
	}
}

func TestObjc_TestDetectionViaCustomBaseClass(t *testing.T) {
	// A suite that subclasses a project base class still reads as tests, which
	// is how most real Objective-C suites are written.
	src := []byte(`@interface SDImageTests : SDTestCase
@end
@implementation SDImageTests
- (void)testDecode {}
@end
`)
	nodes, _ := objcExtract(t, src)
	if !objcHas(nodes, topology.KindTest, "testDecode") {
		t.Fatalf("custom base class not recognised; got %s", objcSummary(nodes))
	}
}

func TestObjc_NonTestClassKeepsMethods(t *testing.T) {
	src := []byte(`@implementation Parser
- (void)testConnection {}
@end
`)
	nodes, _ := objcExtract(t, src)
	// `testConnection` on an ordinary class is a method, not a test: without the
	// XCTestCase evidence there is nothing to promote it.
	if !objcHas(nodes, topology.KindMethod, "testConnection") {
		t.Fatalf("promoted a non-test; got %s", objcSummary(nodes))
	}
}

func TestObjc_ClassInsideConditionalCompilation(t *testing.T) {
	// The regression the embedded-cWalk design exists to prevent: cWalk.top
	// recurses into ITSELF, so delegating a #if block downward would dispatch the
	// class through C's switch, which has no case for it, and lose it silently.
	src := []byte(`#if TARGET_OS_IPHONE
@implementation PlatformView
- (void)layout {}
@end
#endif
`)
	nodes, _ := objcExtract(t, src)
	if !objcHas(nodes, topology.KindClass, "PlatformView") || !objcHas(nodes, topology.KindMethod, "layout") {
		t.Fatalf("class inside #if lost; got %s", objcSummary(nodes))
	}
}

func TestObjc_RecoversSymbolsInsideErrorNode(t *testing.T) {
	// An NS_ENUM body makes the grammar produce an ERROR that swallows the rest
	// of the file. Walking INTO it recovers the class, because the class is still
	// parsed as its own typed node — nothing is read out of the ERROR's text.
	src := []byte(`typedef NS_ENUM(NSInteger, MTLState) {
    MTLStateIdle = 0,
    MTLStateBusy = 1,
};

@implementation MTLRunner
- (void)start {}
@end
`)
	nodes, _ := objcExtract(t, src)
	if !objcHas(nodes, topology.KindClass, "MTLRunner") || !objcHas(nodes, topology.KindMethod, "start") {
		t.Fatalf("ERROR node swallowed real symbols; got %s", objcSummary(nodes))
	}
	// And the enum keeps its own name rather than the macro's.
	if !objcHas(nodes, topology.KindType, "MTLState") {
		t.Fatalf("NS_ENUM name lost; got %s", objcSummary(nodes))
	}
	if objcHas(nodes, topology.KindType, "NS_ENUM") {
		t.Fatalf("emitted the macro as a type: %s", objcSummary(nodes))
	}
}

func TestObjc_RecoversMembersOfAnUnreadableHeader(t *testing.T) {
	// The single largest recall win measured on the real corpus. When the class
	// header itself defeats the grammar the members below it are still parsed as
	// typed nodes, either under the recovery node or under a class_implementation
	// whose name is a zero-width MISSING token (the shape reproduced here). Losing
	// a whole file's API to a header the grammar could not read is the wrong trade:
	// AFNetworking's AFURLSessionManager.m yields no class_implementation at all and
	// 82 sound method_definitions, all of them dropped before this path existed.
	src := []byte("@implementation\n- (void)handleResponse:(NSURLResponse *)r {}\n- (void)cancel {}\n@end\n")
	nodes, _ := objcExtract(t, src)
	for _, sel := range []string{"handleResponse:", "cancel"} {
		m := objcFind(t, nodes, topology.KindMethod, sel)
		// The owner really is unknown, so the qualified name says only the
		// selector rather than inventing a class for it.
		if m.Qualified != sel {
			t.Fatalf("unowned %q qualified as %q; want the bare selector", sel, m.Qualified)
		}
		if !m.HasBytes || m.EndByte > len(src) || m.EndByte <= m.StartByte {
			t.Fatalf("unowned %q span %d..%d", sel, m.StartByte, m.EndByte)
		}
	}
}

func TestObjc_MembersSurviveADefectInsideTheBody(t *testing.T) {
	// A `#if` splitting an expression wrecks the parse mid-body, but the methods
	// after it are still typed nodes and still belong to THIS class — so members()
	// walks through the recovery node rather than stopping at it.
	src := []byte(`@implementation MASMaker
- (void)build {
    NSInteger mask = (1
#if TARGET_OS_IPHONE
        | 2
#endif
    );
}
- (void)afterTheDefect {}
@end
`)
	nodes, _ := objcExtract(t, src)
	if !objcHas(nodes, topology.KindMethod, "afterTheDefect") {
		t.Fatalf("a defect mid-body cost the methods after it; got %s", objcSummary(nodes))
	}
}

func TestObjc_DocSpanCoversMethodComment(t *testing.T) {
	nodes, _ := objcExtract(t, objcSrc)

	// The comment is a sibling of the implementation_definition WRAPPER, not of
	// the method node, so anchoring on the method alone would find nothing.
	m := objcFind(t, nodes, topology.KindMethod, "displayNameForLocale:fallback:")
	if m.DocStartByte == 0 || m.DocEndByte <= m.DocStartByte {
		t.Fatalf("no doc span: %d..%d", m.DocStartByte, m.DocEndByte)
	}
	doc := string(objcSrc[m.DocStartByte:m.DocEndByte])
	if !strings.Contains(doc, "Builds the widget's display name") {
		t.Fatalf("doc span = %q", doc)
	}
}

func TestObjc_SpansAreValid(t *testing.T) {
	nodes, _ := objcExtract(t, objcSrc)

	if len(nodes) == 0 {
		t.Fatal("no nodes")
	}
	for _, n := range nodes {
		if !n.HasBytes {
			t.Fatalf("%s %q has no byte span", n.Kind, n.Name)
		}
		if n.StartByte < 0 || n.EndByte > len(objcSrc) || n.EndByte <= n.StartByte {
			t.Fatalf("%s %q span %d..%d out of range (len %d)", n.Kind, n.Name, n.StartByte, n.EndByte, len(objcSrc))
		}
		if n.StartLine < 1 || n.EndLine < n.StartLine {
			t.Fatalf("%s %q lines %d..%d", n.Kind, n.Name, n.StartLine, n.EndLine)
		}
		if n.Language != "objc" || n.Path != "src/MTLWidget.m" {
			t.Fatalf("%s %q stamped language=%q path=%q", n.Kind, n.Name, n.Language, n.Path)
		}
	}
}

func TestObjc_ParseFidelity(t *testing.T) {
	nodes, _, err := NewObjC().Extract(context.Background(), "src/MTLWidget.m", objcSrc)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	// The fixture is chosen to parse cleanly, so every declaration in it must
	// surface; a regression in the grammar shows up here as a missing symbol
	// rather than as a silent shortfall in the corpus.
	want := []struct {
		kind topology.NodeKind
		name string
	}{
		{topology.KindClass, "MTLWidget"},
		{topology.KindClass, "MTLWidget(Debugging)"},
		{topology.KindType, "MTLSerialising"},
		{topology.KindMethod, "displayNameForLocale:fallback:"},
		{topology.KindMethod, "serialise"},
		{topology.KindMethod, "sharedWidget"},
		{topology.KindMethod, "runBlock"},
		{topology.KindMethod, "debugSummary"},
		{topology.KindVariable, "secretName"},
		{topology.KindVariable, "_tickCount"},
		{topology.KindVariable, "kDefaultName"},
		{topology.KindType, "MTLCompletion"},
	}
	for _, w := range want {
		if !objcHas(nodes, w.kind, w.name) {
			t.Fatalf("fidelity: missing %s %q; got %s", w.kind, w.name, objcSummary(nodes))
		}
	}
}

func TestObjc_EmptyAndTrivialInput(t *testing.T) {
	for _, src := range []string{"", "\n\n", "// just a comment\n"} {
		nodes, edges, err := NewObjC().Extract(context.Background(), "a.m", []byte(src))
		if err != nil {
			t.Fatalf("Extract(%q): %v", src, err)
		}
		if len(nodes) != 0 || len(edges) != 0 {
			t.Fatalf("Extract(%q) = %d nodes, %d edges", src, len(nodes), len(edges))
		}
	}
}

func TestObjc_CancelledContextDoesNotParse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := NewObjC().Extract(ctx, "a.m", objcSrc); err == nil {
		t.Fatal("want an error for a dead context")
	}
}

// TestObjc_AssumeNonnullTripwire pins a live upstream defect so that fixing it
// upstream is noticed here rather than silently improving the corpus. A
// function-like macro with NO trailing semicolon (the NS_ASSUME_NONNULL_BEGIN
// idiom) merges with the `@implementation` that follows and destroys it: the
// class is re-parsed as a binary expression, so there is no typed node left to
// recover and walking into the ERROR cannot help. Only one file in a 541-file
// real corpus is affected, because the idiom belongs to headers.
func TestObjc_AssumeNonnullTripwire(t *testing.T) {
	src := []byte(`NS_ASSUME_NONNULL_BEGIN

@implementation Guarded
- (void)work {}
@end

NS_ASSUME_NONNULL_END
`)
	nodes, _ := objcExtract(t, src)
	if objcHas(nodes, topology.KindClass, "Guarded") {
		t.Fatal("upstream now parses a macro-guarded @implementation — delete this " +
			"tripwire and the caveat on ObjCExtractor.Extract")
	}
	// The same source with the macro terminated parses cleanly, which is the
	// evidence that the semicolon — not the class — is what breaks it.
	ok, _ := objcExtract(t, []byte("NS_ASSUME_NONNULL_BEGIN;\n@implementation Guarded\n- (void)work {}\n@end\n"))
	if !objcHas(ok, topology.KindClass, "Guarded") {
		t.Fatalf("terminated form also failed; got %s", objcSummary(ok))
	}
}

// A `readonly` property publishes no setter, which is Objective-C's nearest
// equivalent to a final field, so it is the immutable case and takes
// KindConstant while every other property takes KindVariable.
func TestObjc_ReadonlyPropertyIsAConstant(t *testing.T) {
	src := []byte(`#import <Foundation/Foundation.h>

@interface Widget : NSObject
@property (nonatomic, readonly) NSString *identifier;
@property (nonatomic, copy) NSString *title;
@end
`)
	nodes, _ := objcExtract(t, src)

	ro := objcFind(t, nodes, topology.KindConstant, "identifier")
	if ro.Qualified != "Widget.identifier" {
		t.Errorf("readonly property Qualified = %q, want Widget.identifier", ro.Qualified)
	}
	if !objcHas(nodes, topology.KindVariable, "title") {
		t.Errorf("a mutable property should be KindVariable; got %s", objcSummary(nodes))
	}
}
