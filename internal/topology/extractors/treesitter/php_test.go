package treesitter

import (
	"context"
	"slices"
	"strings"
	"testing"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/plumbkit/plumb/internal/topology"
)

// phpSrc is the idiomatic sample: one modern PHP file exercising every
// construct the extractor claims — a namespaced, attributed abstract class with
// promoted constructor properties, an interface, a trait, a backed enum, all
// four `use` spellings, file-scope constants and closures — ending in
// trailing_helper so a grammar cascade that swallowed the tail would be caught.
var phpSrc = []byte(`<?php

declare(strict_types=1);

namespace App\Billing;

use App\Model\User;
use App\Model\{Order, Item};
use App\Contracts\Repository as Repo;
use function App\Support\slugify;
use const App\Support\VERSION;

const TAX_RATE = 0.15;

/**
 * A billable document.
 */
#[Entity]
abstract class Invoice extends Document implements Payable, Countable
{
    use Loggable;

    public const STATUS_OPEN = 'open';

    protected static int $issued = 0;
    private ?string $reference = null;
    public readonly int $id;

    public function __construct(
        private readonly string $currency,
        protected int $retries = 3,
    ) {
        $this->id = 1;
    }

    abstract protected function render(): string;

    final public static function make(string $currency): static
    {
        return new static($currency);
    }

    // Sums the line items.
    public function total(): int|float
    {
        return $this->subtotal() + 1;
    }

    private function subtotal(): int
    {
        $rows = array_map(fn ($r) => $r * 2, []);
        return count($rows);
    }
}

interface Payable
{
    const MAX_ATTEMPTS = 3;

    public function pay(int $amount): bool;
}

trait Loggable
{
    public function log(string $message): void {}
}

enum Currency: string
{
    case Aud = 'AUD';
    case Usd = 'USD';

    public function symbol(): string
    {
        return match ($this) {
            Currency::Aud, Currency::Usd => '$',
        };
    }
}

$formatter = function (int $cents): string {
    return (string) $cents;
};

$halve = fn (int $cents): int => intdiv($cents, 2);

function trailing_helper(Invoice ...$invoices): string
{
    return slugify('done');
}
`)

func phpExtract(t *testing.T) ([]topology.Node, []topology.Edge) {
	t.Helper()
	nodes, edges, err := NewPHP().Extract(context.Background(), "src/Billing/Invoice.php", phpSrc)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return nodes, edges
}

// phpWalkSrc runs the walk over arbitrary source with the ERROR-descent knob
// under test control, which is what makes the A/B in
// TestPHP_ErrorDescentRecovery a measurement rather than an assertion.
func phpWalkSrc(t *testing.T, src string, errorDescent bool) ([]topology.Node, []topology.Edge) {
	t.Helper()
	lang := grammars.PhpLanguage()
	nodes, edges, err := extractWith(context.Background(), lang, []byte(src),
		func(root *tsg.Node) ([]topology.Node, []topology.Edge) {
			return phpWalkTree(root, lang, []byte(src), "a.php", errorDescent)
		})
	if err != nil {
		t.Fatalf("extractWith: %v", err)
	}
	return nodes, edges
}

func phpNodes(t *testing.T, src string) []topology.Node {
	t.Helper()
	nodes, _, err := NewPHP().Extract(context.Background(), "a.php", []byte(src))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return nodes
}

func phpFind(t *testing.T, nodes []topology.Node, kind topology.NodeKind, name string) topology.Node {
	t.Helper()
	for _, n := range nodes {
		if n.Kind == kind && n.Name == name {
			return n
		}
	}
	t.Fatalf("no %s named %q; nodes=%v", kind, name, nodeNames(nodes))
	return topology.Node{}
}

// TestPHP_SampleParsesClean is the fidelity precondition: the idiomatic sample
// must parse with no ERROR and no MISSING node, or the shared
// TestExtractors_ParseFidelity guard would be measuring a broken sample rather
// than the extractor. A MISSING node is not an ERROR node — the PHP grammar
// recovers several malformed declarations by inserting a MISSING token and
// producing a tree that looks entirely clean in its S-expression — so both are
// checked, which is exactly what HasError covers.
func TestPHP_SampleParsesClean(t *testing.T) {
	tree, err := tsg.NewParser(grammars.PhpLanguage()).Parse(phpSrc)
	if err != nil || tree == nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()
	if tree.RootNode().HasError() {
		t.Errorf("idiomatic sample parses with ERROR/MISSING nodes:\n%s",
			tree.RootNode().SExpr(grammars.PhpLanguage()))
	}
}

func TestPHP_KindsExtracted(t *testing.T) {
	nodes, _ := phpExtract(t)
	for _, tc := range []struct {
		kind topology.NodeKind
		want []string
	}{
		{topology.KindPackage, []string{"App\\Billing"}},
		{topology.KindClass, []string{"Invoice", "Loggable", "Currency"}},
		{topology.KindType, []string{"Payable"}},
		{topology.KindMethod, []string{"__construct", "render", "make", "total", "subtotal", "pay", "log", "symbol"}},
		{topology.KindFunction, []string{"formatter", "halve", "trailing_helper"}},
		{topology.KindConstant, []string{"TAX_RATE", "STATUS_OPEN", "id", "currency", "MAX_ATTEMPTS", "Aud", "Usd"}},
		{topology.KindVariable, []string{"issued", "reference", "retries"}},
		{topology.KindImport, []string{
			"App\\Model\\User", "App\\Model\\Order", "App\\Model\\Item",
			"App\\Contracts\\Repository", "App\\Support\\slugify", "App\\Support\\VERSION",
		}},
	} {
		got := names(nodes, tc.kind)
		for _, w := range tc.want {
			if !slices.Contains(got, w) {
				t.Errorf("%s %q not extracted; got=%v", tc.kind, w, got)
			}
		}
	}
}

// TestPHP_QualifiedNames pins the notation: a class is namespace-qualified with
// PHP's own backslash, and a member is `Class::name` — what a stack trace, a
// docblock and a static analyser all print — with the `$` kept only for a
// property, which is how PHP itself distinguishes `C::CONST` from `C::$prop`.
func TestPHP_QualifiedNames(t *testing.T) {
	nodes, _ := phpExtract(t)
	for _, tc := range []struct{ kind, name, want string }{
		{string(topology.KindClass), "Invoice", "App\\Billing\\Invoice"},
		{string(topology.KindType), "Payable", "App\\Billing\\Payable"},
		{string(topology.KindMethod), "total", "App\\Billing\\Invoice::total"},
		{string(topology.KindConstant), "STATUS_OPEN", "App\\Billing\\Invoice::STATUS_OPEN"},
		{string(topology.KindConstant), "Aud", "App\\Billing\\Currency::Aud"},
		{string(topology.KindVariable), "issued", "App\\Billing\\Invoice::$issued"},
		{string(topology.KindConstant), "currency", "App\\Billing\\Invoice::$currency"},
		{string(topology.KindFunction), "trailing_helper", "App\\Billing\\trailing_helper"},
		{string(topology.KindConstant), "TAX_RATE", "App\\Billing\\TAX_RATE"},
	} {
		n := phpFind(t, nodes, topology.NodeKind(tc.kind), tc.name)
		if n.Qualified != tc.want {
			t.Errorf("%s %q qualified = %q, want %q", tc.kind, tc.name, n.Qualified, tc.want)
		}
	}
}

// TestPHP_PromotedProperties covers the PHP 8 shape an extractor that only
// looks at `property_declaration` misses entirely: a promoted constructor
// parameter is a real property of the CLASS, and `readonly` still decides
// constant versus variable.
func TestPHP_PromotedProperties(t *testing.T) {
	nodes, edges := phpExtract(t)
	if !slices.Contains(names(nodes, topology.KindConstant), "currency") {
		t.Errorf("readonly promoted property should be KindConstant; consts=%v", names(nodes, topology.KindConstant))
	}
	if !slices.Contains(names(nodes, topology.KindVariable), "retries") {
		t.Errorf("mutable promoted property should be KindVariable; vars=%v", names(nodes, topology.KindVariable))
	}
	for _, member := range []string{"currency", "retries"} {
		if !containedIn(t, nodes, edges, "Invoice", member) {
			t.Errorf("promoted property %q should be contained by the class, not the constructor", member)
		}
	}
}

// TestPHP_MemberConventions is the local half of the cross-language member
// contract: `readonly` is immutable so a constant, anything else a variable,
// and a function-local is never surfaced at all.
func TestPHP_MemberConventions(t *testing.T) {
	nodes := phpNodes(t, `<?php
class C {
    public readonly int $IMMUT;
    public int $mut = 2;
    public function m(): void {
        $localv = 3;
        $localfn = fn () => 1;
    }
}
`)
	if !slices.Contains(names(nodes, topology.KindConstant), "IMMUT") {
		t.Errorf("readonly property should be KindConstant; consts=%v", names(nodes, topology.KindConstant))
	}
	if !slices.Contains(names(nodes, topology.KindVariable), "mut") {
		t.Errorf("mutable property should be KindVariable; vars=%v", names(nodes, topology.KindVariable))
	}
	for _, local := range []string{"localv", "localfn"} {
		if hasNodeNamed(nodes, local) {
			t.Errorf("function-local %q must not be surfaced; nodes=%v", local, nodeNames(nodes))
		}
	}
}

// TestPHP_Imports covers all four `use` spellings. A group is several
// dependencies, not one, and an alias must not displace the real path — a node
// named "Repo" would be invisible to anyone searching for the class it imports.
func TestPHP_Imports(t *testing.T) {
	nodes, _ := phpExtract(t)
	got := importNames(nodes)
	for _, want := range []string{
		"App\\Model\\User", "App\\Model\\Order", "App\\Model\\Item",
		"App\\Contracts\\Repository", "App\\Support\\slugify", "App\\Support\\VERSION",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("import %q missing; imports=%v", want, got)
		}
	}
	if slices.Contains(got, "Repo") {
		t.Errorf("alias must not replace the imported path; imports=%v", got)
	}
	// A group's members must not share one span, or an edit range resolved
	// against either becomes ambiguous.
	order := phpFind(t, nodes, topology.KindImport, "App\\Model\\Order")
	item := phpFind(t, nodes, topology.KindImport, "App\\Model\\Item")
	if order.StartByte == item.StartByte {
		t.Errorf("grouped imports share a span: Order=%d-%d Item=%d-%d",
			order.StartByte, order.EndByte, item.StartByte, item.EndByte)
	}
	// The `function` / `const` keyword is the only thing distinguishing a
	// symbol import from a class one, so it has to survive somewhere.
	fn := phpFind(t, nodes, topology.KindImport, "App\\Support\\slugify")
	if !strings.HasPrefix(fn.Signature, "function ") {
		t.Errorf("`use function` signature = %q, want the keyword preserved", fn.Signature)
	}
}

// TestPHP_NamespaceScoping pins the difference between PHP's two namespace
// spellings: braces scope to the block, a semicolon governs the rest of the
// file. Getting this wrong silently mis-qualifies every symbol after the first
// namespace in a multi-namespace file.
func TestPHP_NamespaceScoping(t *testing.T) {
	braced := phpNodes(t, `<?php
namespace App\First {
    class Alpha {}
}
namespace App\Second {
    class Beta {}
}
`)
	for _, tc := range []struct{ name, want string }{
		{"Alpha", "App\\First\\Alpha"},
		{"Beta", "App\\Second\\Beta"},
	} {
		if got := phpFind(t, braced, topology.KindClass, tc.name).Qualified; got != tc.want {
			t.Errorf("braced namespace: %q qualified = %q, want %q", tc.name, got, tc.want)
		}
	}

	semi := phpNodes(t, `<?php
namespace App\Only;

class Gamma {}

function later(): void {}
`)
	if got := phpFind(t, semi, topology.KindFunction, "later").Qualified; got != "App\\Only\\later" {
		t.Errorf("semicolon namespace must govern later siblings: later qualified = %q", got)
	}
}

// TestPHP_NamespaceContainment checks the namespace node actually owns what
// follows it, which is what makes KindPackage useful rather than decorative.
func TestPHP_NamespaceContainment(t *testing.T) {
	nodes, edges := phpExtract(t)
	for _, member := range []string{"Invoice", "Payable", "Currency", "trailing_helper", "TAX_RATE"} {
		if !containedIn(t, nodes, edges, "App\\Billing", member) {
			t.Errorf("namespace should contain %q; nodes=%v", member, nodeNames(nodes))
		}
	}
}

// TestPHP_Containment checks the certain (1.0/extractor) containment edges from
// every container kind to its members.
func TestPHP_Containment(t *testing.T) {
	nodes, edges := phpExtract(t)
	for _, tc := range [][2]string{
		{"Invoice", "total"},
		{"Invoice", "STATUS_OPEN"},
		{"Invoice", "issued"},
		{"Payable", "pay"},
		{"Payable", "MAX_ATTEMPTS"},
		{"Loggable", "log"},
		{"Currency", "Aud"},
		{"Currency", "symbol"},
	} {
		if !containedIn(t, nodes, edges, tc[0], tc[1]) {
			t.Errorf("%s should contain %s", tc[0], tc[1])
		}
	}
	for _, e := range edges {
		if e.Kind != topology.EdgeContains {
			continue
		}
		if e.Confidence != 1.0 || e.Source != "extractor" {
			t.Errorf("containment edge %s→%s has confidence %v source %q, want 1.0/extractor",
				nodes[e.FromID].Name, nodes[e.ToID].Name, e.Confidence, e.Source)
		}
	}
}

func TestPHP_CallEdges(t *testing.T) {
	nodes, edges := phpExtract(t)
	want := [2]string{"total", "subtotal"}
	found := false
	for _, e := range edges {
		if e.Kind != topology.EdgeCalls {
			continue
		}
		if nodes[e.FromID].Name == want[0] && nodes[e.ToID].Name == want[1] {
			found = true
			if e.Confidence != 0.8 || e.Source != "heuristic" {
				t.Errorf("call edge confidence = %v source %q, want 0.8/heuristic", e.Confidence, e.Source)
			}
		}
	}
	if !found {
		t.Errorf("no call edge %s→%s (a `$this->m()` call site)", want[0], want[1])
	}
}

// TestPHP_CallForms covers the four spellings a PHP call takes; the receiver is
// discarded, so all four resolve by simple name.
func TestPHP_CallForms(t *testing.T) {
	nodes, edges := phpWalkSrc(t, `<?php
class C {
    public function driver(): void {
        plainCall();
        $this->viaObject();
        self::viaScope();
        $x?->viaNullsafe();
    }
    public function viaObject(): void {}
    public static function viaScope(): void {}
    public function viaNullsafe(): void {}
}
function plainCall(): void {}
`, true)
	for _, target := range []string{"plainCall", "viaObject", "viaScope", "viaNullsafe"} {
		found := false
		for _, e := range edges {
			if e.Kind == topology.EdgeCalls && nodes[e.FromID].Name == "driver" && nodes[e.ToID].Name == target {
				found = true
			}
		}
		if !found {
			t.Errorf("no call edge driver→%s", target)
		}
	}
}

// TestPHP_TestDetection covers PHPUnit's three marks and the one case the
// narrower out-of-class name rule exists for: `testable()` in ordinary code is
// not a test.
func TestPHP_TestDetection(t *testing.T) {
	nodes := phpNodes(t, `<?php
namespace Tests;

use PHPUnit\Framework\TestCase;

class InvoiceTest extends TestCase
{
    public function testItTotals(): void {}

    #[Test]
    public function itAlsoTotals(): void {}

    /** @test */
    public function annotatedCase(): void {}

    public function setUp(): void {}
}

class Helper
{
    public function testable(): bool { return true; }
    public function test_thing(): void {}
}
`)
	tests := names(nodes, topology.KindTest)
	for _, want := range []string{"testItTotals", "itAlsoTotals", "annotatedCase", "test_thing"} {
		if !slices.Contains(tests, want) {
			t.Errorf("%q should be KindTest; tests=%v", want, tests)
		}
	}
	for _, notTest := range []string{"setUp", "testable"} {
		if slices.Contains(tests, notTest) {
			t.Errorf("%q must not be KindTest; tests=%v", notTest, tests)
		}
	}
}

// TestPHP_InterleavedHTML is the template case. A .php file may open in HTML
// and switch in and out of PHP any number of times; the grammar keeps the
// declarations as siblings of the interleaved text, so every block must be
// indexed — not just the first.
func TestPHP_InterleavedHTML(t *testing.T) {
	nodes := phpNodes(t, `<h1>Header</h1>
<?php
namespace App\View;

class First {}
?>
<div>middle</div>
<?php
function second(): void {}
?>
<p>tail</p>
<?php
class Third
{
    public function inside(): void { ?>
        <span>markup in a method body</span>
    <?php }
}
?>
<footer>bye</footer>
`)
	for _, want := range []string{"First", "second", "Third", "inside"} {
		if !hasNodeNamed(nodes, want) {
			t.Errorf("%q lost across an HTML block; nodes=%v", want, nodeNames(nodes))
		}
	}
	if got := phpFind(t, nodes, topology.KindClass, "Third").Qualified; got != "App\\View\\Third" {
		t.Errorf("namespace must survive `?> … <?php`: Third qualified = %q", got)
	}
}

// TestPHP_ErrorDescentRecovery is the A/B: the same broken file walked with and
// without descent into ERROR recovery nodes. One unbalanced brace makes the
// grammar wrap the file's tail in a single ERROR, and everything inside it is
// invisible unless the walk descends — while still coming from real typed
// nodes, never from ERROR text.
func TestPHP_ErrorDescentRecovery(t *testing.T) {
	src := `<?php
function one(): void {}

class Half {
    public function two(): void {

function three(): void {}

class Deep
{
    public function four(): void {}
}

function five(): void {}
`
	with, _ := phpWalkSrc(t, src, true)
	without, _ := phpWalkSrc(t, src, false)

	for _, want := range []string{"three", "Deep", "four", "five"} {
		if !hasNodeNamed(with, want) {
			t.Errorf("ERROR descent should recover %q; nodes=%v", want, nodeNames(with))
		}
	}
	if len(with) <= len(without) {
		t.Errorf("ERROR descent recovered nothing: %d nodes with, %d without", len(with), len(without))
	}
	t.Logf("A/B on a defective file: %d nodes with ERROR descent, %d without", len(with), len(without))

	// Recovery must not invent symbols: every node still has to name something
	// that appears in the source.
	for _, n := range with {
		if n.Name != "" && !strings.Contains(src, n.Name) {
			t.Errorf("recovered node %q is not in the source — a symbol was synthesised from ERROR text", n.Name)
		}
	}
}

// TestPHP_ByteSpans guards the setSpan discipline at every emission site: a
// node without a byte span degrades the symbol-edit fallback to line
// granularity, which on a one-line property rewrites its siblings.
func TestPHP_ByteSpans(t *testing.T) {
	nodes, _ := phpExtract(t)
	for _, n := range nodes {
		if !n.HasBytes {
			t.Errorf("%s %q has no byte span", n.Kind, n.Name)
			continue
		}
		if n.EndByte <= n.StartByte || n.EndByte > len(phpSrc) {
			t.Errorf("%s %q has span %d-%d, out of range for %d bytes", n.Kind, n.Name, n.StartByte, n.EndByte, len(phpSrc))
		}
		if n.EndLine < n.StartLine {
			t.Errorf("%s %q has inverted line range %d-%d", n.Kind, n.Name, n.StartLine, n.EndLine)
		}
	}
}

// TestPHP_Signatures checks the declaration head survives with the parts worth
// searching for: modifiers, `extends`/`implements`, promoted parameters and
// union or nullable return types — none of which this extractor rebuilds, so a
// regression here means signatureHead stopped at the wrong node.
func TestPHP_Signatures(t *testing.T) {
	nodes, _ := phpExtract(t)
	for _, tc := range []struct {
		kind topology.NodeKind
		name string
		want string
	}{
		{topology.KindClass, "Invoice", "abstract class Invoice extends Document implements Payable, Countable"},
		{topology.KindMethod, "make", "final public static function make(string $currency): static"},
		{topology.KindMethod, "total", "public function total(): int|float"},
		{topology.KindMethod, "render", "abstract protected function render(): string"},
		{topology.KindType, "Payable", "interface Payable"},
		{topology.KindClass, "Currency", "enum Currency: string"},
	} {
		if got := phpFind(t, nodes, tc.kind, tc.name).Signature; got != tc.want {
			t.Errorf("%s %q signature = %q, want %q", tc.kind, tc.name, got, tc.want)
		}
	}
}

// TestPHP_DocSpans checks the docblock lands on the declaration it precedes and
// nowhere else. PHP spells `/** */`, `//` and `#` all as `comment`, so a rule
// keyed on one syntax would silently lose the others.
func TestPHP_DocSpans(t *testing.T) {
	nodes, _ := phpExtract(t)
	assertDocSpans(t, phpSrc, nodes, map[string]string{
		"Invoice": "/**\n * A billable document.\n */",
		"total":   "// Sums the line items.",
	})
	// A constant is not a declaration a doc span is stamped for, and a method
	// with nothing above it must claim none rather than reach back across the
	// blank line to the previous member's closing brace.
	assertNoDocSpan(t, phpSrc, nodes, "subtotal")

	hashed := phpNodes(t, `<?php
# A hash comment.
function hashed(): void {}
`)
	n := phpFind(t, hashed, topology.KindFunction, "hashed")
	if !n.HasDocSpan() {
		t.Error("a `#` comment is still a comment and should produce a doc span")
	}
}

// TestPHP_ClosuresAndArrows: a closure bound to a name at file scope is a
// declaration; one bound inside a function body is a local.
func TestPHP_ClosuresAndArrows(t *testing.T) {
	nodes, _ := phpExtract(t)
	for _, want := range []string{"formatter", "halve"} {
		n := phpFind(t, nodes, topology.KindFunction, want)
		if n.Signature == "" {
			t.Errorf("closure %q has no signature", want)
		}
	}
	scoped := phpNodes(t, `<?php
$outer = fn () => 1;
function host(): void {
    $inner = fn () => 2;
}
`)
	if !hasNodeNamed(scoped, "outer") {
		t.Errorf("file-scope closure should be emitted; nodes=%v", nodeNames(scoped))
	}
	if hasNodeNamed(scoped, "inner") {
		t.Errorf("closure inside a function body is a local; nodes=%v", nodeNames(scoped))
	}
}

// TestPHP_AttributesDoNotBecomeSymbols: `#[Entity]` is metadata on the class,
// not a declaration of its own, and it must not leak into the signature either.
func TestPHP_AttributesDoNotBecomeSymbols(t *testing.T) {
	nodes, _ := phpExtract(t)
	if hasNodeNamed(nodes, "Entity") {
		t.Errorf("an attribute must not be emitted as a symbol; nodes=%v", nodeNames(nodes))
	}
	if sig := phpFind(t, nodes, topology.KindClass, "Invoice").Signature; strings.Contains(sig, "#[") {
		t.Errorf("attribute leaked into the signature: %q", sig)
	}
}

// TestPHP_TraitUseIsNotAnImport: `use Loggable;` inside a class body is trait
// composition, and the grammar spells it `use_declaration` — a walk that keyed
// imports on the keyword rather than the node type would emit a bogus one.
func TestPHP_TraitUseIsNotAnImport(t *testing.T) {
	nodes, _ := phpExtract(t)
	if slices.Contains(importNames(nodes), "Loggable") {
		t.Errorf("a trait `use` is not an import; imports=%v", importNames(nodes))
	}
}

// TestPHP_MultiElementDeclarations: PHP lets one statement declare several
// members, and collapsing them into one node named after the first would drop
// the rest.
func TestPHP_MultiElementDeclarations(t *testing.T) {
	nodes := phpNodes(t, `<?php
const A_CONST = 1, B_CONST = 2;
class K {
    const D = 1, E = 2;
    public int $x = 1, $y = 2;
}
`)
	for _, want := range []string{"A_CONST", "B_CONST", "D", "E"} {
		if !slices.Contains(names(nodes, topology.KindConstant), want) {
			t.Errorf("constant %q missing; consts=%v", want, names(nodes, topology.KindConstant))
		}
	}
	for _, want := range []string{"x", "y"} {
		if !slices.Contains(names(nodes, topology.KindVariable), want) {
			t.Errorf("property %q missing; vars=%v", want, names(nodes, topology.KindVariable))
		}
	}
}

// TestPHP_EmptyAndMalformed asserts the extractor degrades quietly rather than
// erroring on the inputs a real tree is full of.
func TestPHP_EmptyAndMalformed(t *testing.T) {
	for _, src := range []string{
		"",
		"<?php",
		"<?php\nclass",
		"plain html with no php at all\n",
		"<?php\nfunction (: {{{ }\n",
	} {
		if _, _, err := NewPHP().Extract(context.Background(), "a.php", []byte(src)); err != nil {
			t.Errorf("Extract(%q) returned error (want graceful nil): %v", src, err)
		}
	}
}

// TestPHP_ExtractorIdentity pins the wiring contract the registry depends on.
func TestPHP_ExtractorIdentity(t *testing.T) {
	e := NewPHP()
	if e.Language() != "php" {
		t.Errorf("Language() = %q, want php", e.Language())
	}
	if got := e.Extensions(); !slices.Equal(got, []string{".php"}) {
		t.Errorf("Extensions() = %v, want [.php]", got)
	}
	nodes, _ := phpExtract(t)
	for _, n := range nodes {
		if n.Language != "php" || n.Path != "src/Billing/Invoice.php" {
			t.Fatalf("%s %q has language %q path %q", n.Kind, n.Name, n.Language, n.Path)
		}
	}
}
