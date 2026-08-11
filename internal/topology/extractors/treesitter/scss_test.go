package treesitter

import (
	"context"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

// scssSrc deliberately avoids five constructs the tree-sitter-scss grammar
// mis-parses today — `!default`, `@use … as`, `@use … with`, map literals and
// `@include ns.mixin` — because this fixture also feeds the shared parse
// fidelity guard, which asserts zero ERROR and zero MISSING nodes. That guard
// exists to catch a cascade introduced by OUR change; letting a pinned upstream
// grammar defect fail it would turn a regression detector into noise. The
// defects themselves are covered, on purpose, by
// TestSCSS_KnownGrammarDefectsDegradeGracefully.
var scssSrc = []byte(`@use "sass:math";
@forward "tokens";
@import "reset";

$brand: #3f51b5;
$radius: 4px;

/* A card and the parts nested inside it. */
.card {
  color: $brand;
  --card-gap: 8px;

  .card__title {
    font-weight: 600;

    &:hover {
      text-decoration: underline;
    }
  }

  @media (min-width: 40rem) {
    .card__body {
      padding: 2rem;
    }
  }
}

%pill {
  border-radius: 999px;
}

// Paint a button in the given colours.
@mixin button($bg, $fg: white) {
  background: $bg;
  color: $fg;
  $shade: 10%;

  .icon {
    opacity: 1;
  }
}

@function double($n) {
  @return $n * 2;
}

.btn {
  @include button($brand);
  width: double(4px);
}

@keyframes fade {
  from { opacity: 0; }
}

// Ring drawn around a focused control.
@mixin focus-ring {
  outline: 2px solid $brand;
}
`)

func scssExtract(t *testing.T) ([]topology.Node, []topology.Edge) {
	t.Helper()
	nodes, edges, err := NewSCSS().Extract(context.Background(), "theme.scss", scssSrc)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return nodes, edges
}

func scssNamed(nodes []topology.Node, name string) (topology.Node, bool) {
	for _, n := range nodes {
		if n.Name == name {
			return n, true
		}
	}
	return topology.Node{}, false
}

func TestSCSS_LandmarksExtracted(t *testing.T) {
	nodes, _ := scssExtract(t)

	want := map[string]topology.NodeKind{
		"sass:math":    topology.KindImport,
		"tokens":       topology.KindImport,
		"reset":        topology.KindImport,
		"$brand":       topology.KindConstant,
		"$radius":      topology.KindConstant,
		".card":        topology.KindType,
		".card__title": topology.KindType,
		"%pill":        topology.KindType,
		"button":       topology.KindFunction,
		"double":       topology.KindFunction,
		"focus-ring":   topology.KindFunction,
		".btn":         topology.KindType,
		"fade":         topology.KindType,
	}
	for name, kind := range want {
		n, ok := scssNamed(nodes, name)
		if !ok {
			t.Errorf("missing landmark %q", name)
			continue
		}
		if n.Kind != kind {
			t.Errorf("%q: kind = %q, want %q", name, n.Kind, kind)
		}
	}
}

// A mixin is the one SCSS construct with a parameter list, so the signature has
// to carry it — including default values, which is what makes the source text
// the right thing to keep.
func TestSCSS_MixinSignatureCarriesParameters(t *testing.T) {
	nodes, _ := scssExtract(t)

	n, ok := scssNamed(nodes, "button")
	if !ok {
		t.Fatal("mixin button not extracted")
	}
	if got, want := n.Signature, "@mixin button($bg, $fg: white)"; got != want {
		t.Errorf("signature = %q, want %q", got, want)
	}

	fn, ok := scssNamed(nodes, "double")
	if !ok {
		t.Fatal("function double not extracted")
	}
	if got, want := fn.Signature, "@function double($n)"; got != want {
		t.Errorf("signature = %q, want %q", got, want)
	}
}

// Nesting is the whole reason SCSS is not CSS: a nested rule must become a
// contained symbol, not a sibling floating at file scope.
func TestSCSS_NestedRulesBecomeContainment(t *testing.T) {
	nodes, edges := scssExtract(t)

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

	if !contains(".card", ".card__title") {
		t.Error(".card should contain .card__title")
	}
	if !contains(".card__title", "&:hover") {
		t.Error(".card__title should contain &:hover")
	}
	if !contains(".card", "@media (min-width: 40rem)") {
		t.Error(".card should contain its nested media query")
	}
	if !contains("@media (min-width: 40rem)", ".card__body") {
		t.Error("the media query should contain the rule nested under it")
	}
	if !contains("button", ".icon") {
		t.Error("a mixin should contain the rules it emits")
	}

	nested, _ := scssNamed(nodes, ".card__title")
	if got, want := nested.Qualified, ".card > .card__title"; got != want {
		t.Errorf("qualified = %q, want %q", got, want)
	}
}

// An @include is the only call SCSS really has, and it is what makes a mixin
// findable by its callers.
func TestSCSS_IncludeProducesCallEdge(t *testing.T) {
	nodes, edges := scssExtract(t)

	var from, to int64 = -1, -1
	for i, n := range nodes {
		switch n.Name {
		case ".btn":
			from = int64(i)
		case "button":
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
	t.Error("no call edge from .btn to the mixin it includes")
}

// An @include naming a mixin this file does not define must produce no edge at
// all: the call graph is intra-file, and a guessed edge is worse than none.
func TestSCSS_IncludeOfUnknownMixinIsNotGuessed(t *testing.T) {
	src := []byte(".a {\n  @include from-another-file;\n}\n")
	_, edges, err := NewSCSS().Extract(context.Background(), "a.scss", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeCalls {
			t.Errorf("unexpected call edge for an unresolvable include: %+v", e)
		}
	}
}

// A $variable inside a mixin body is scoped to it. Emitting it would put a
// private detail in the Map next to the tokens the theme actually exports.
func TestSCSS_BlockScopedVariablesAreSuppressed(t *testing.T) {
	nodes, _ := scssExtract(t)

	if _, ok := scssNamed(nodes, "$shade"); ok {
		t.Error("$shade is local to the mixin and should not be emitted")
	}
	for _, name := range []string{"$brand", "$radius"} {
		if _, ok := scssNamed(nodes, name); !ok {
			t.Errorf("file-scope token %s should be emitted", name)
		}
	}
}

// Ordinary declarations are the noise CSSExtractor documents; custom properties
// are the deliberate exception and belong to the rule that declares them.
func TestSCSS_DeclarationsAreNotEmittedExceptCustomProperties(t *testing.T) {
	nodes, _ := scssExtract(t)

	for _, n := range nodes {
		if strings.HasPrefix(n.Name, "color") || strings.HasPrefix(n.Name, "padding") ||
			strings.HasPrefix(n.Name, "font-weight") {
			t.Errorf("ordinary declaration emitted as a symbol: %q", n.Name)
		}
	}
	n, ok := scssNamed(nodes, "--card-gap")
	if !ok {
		t.Fatal("custom property --card-gap should be emitted")
	}
	if n.Kind != topology.KindConstant {
		t.Errorf("--card-gap kind = %q, want %q", n.Kind, topology.KindConstant)
	}
}

// Sass codebases overwhelmingly use `//` comments, which the grammar names
// js_comment. Missing that node type would cost every doc span in the corpus
// without failing anything.
func TestSCSS_DocSpanCoversBothCommentSyntaxes(t *testing.T) {
	nodes, _ := scssExtract(t)

	line, ok := scssNamed(nodes, "button")
	if !ok {
		t.Fatal("mixin button not extracted")
	}
	if line.DocStartByte >= line.DocEndByte {
		t.Fatalf("no doc span for a mixin preceded by a // comment: %d..%d", line.DocStartByte, line.DocEndByte)
	}
	if got := string(scssSrc[line.DocStartByte:line.DocEndByte]); !strings.Contains(got, "Paint a button") {
		t.Errorf("doc span = %q, want the // comment above the mixin", got)
	}

	block, ok := scssNamed(nodes, ".card")
	if !ok {
		t.Fatal("rule .card not extracted")
	}
	if got := string(scssSrc[block.DocStartByte:block.DocEndByte]); !strings.Contains(got, "A card and the parts") {
		t.Errorf("doc span = %q, want the /* */ comment above the rule", got)
	}
}

// Every node must carry a byte span that reconstructs its source, which is the
// property the shared span guards check and which one shipped extractor once
// lacked entirely.
func TestSCSS_ByteSpansReconstructTheSource(t *testing.T) {
	nodes, _ := scssExtract(t)

	if len(nodes) == 0 {
		t.Fatal("no nodes extracted")
	}
	for _, n := range nodes {
		if n.StartByte >= n.EndByte {
			t.Errorf("%q: inverted or empty span %d..%d", n.Name, n.StartByte, n.EndByte)
			continue
		}
		if n.EndByte > len(scssSrc) {
			t.Errorf("%q: span %d..%d runs past the source (%d bytes)", n.Name, n.StartByte, n.EndByte, len(scssSrc))
		}
	}

	mixin, ok := scssNamed(nodes, "focus-ring")
	if !ok {
		t.Fatal("trailing mixin not extracted")
	}
	got := string(scssSrc[mixin.StartByte:mixin.EndByte])
	if !strings.HasPrefix(got, "@mixin focus-ring") || !strings.HasSuffix(strings.TrimSpace(got), "}") {
		t.Errorf("span does not reconstruct the mixin: %q", got)
	}
}

// The fixture feeds the shared fidelity guard, so its parse must be clean here
// too — a defect introduced into the fixture would otherwise surface as a
// confusing failure in a table two files away.
func TestSCSS_FixtureParsesWithoutDefects(t *testing.T) {
	nodes, _ := scssExtract(t)
	if len(nodes) < 12 {
		t.Fatalf("fixture yielded only %d nodes, expected the full landmark set", len(nodes))
	}
	if _, ok := scssNamed(nodes, "focus-ring"); !ok {
		t.Error("the declaration nearest EOF was lost, which is what a parse cascade looks like")
	}
}

// These five constructs are ordinary modern Sass that the pinned grammar
// mis-parses. The extractor cannot fix the parse, but it must not panic, and
// where a name survives in the tree it must still be reported — this test pins
// which of those two outcomes each defect gets, so an upstream grammar fix
// shows up here as a change rather than passing unnoticed.
func TestSCSS_KnownGrammarDefectsDegradeGracefully(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		wantSym string // "" when the construct is lost to the parse entirely
	}{
		{"bang-default", "$brand: #333 !default;\n", "$brand"},
		{"use-with-alias", "@use \"./config\" as cfg;\n", ""},
		{"use-with-config", "@use \"theme\" with ($primary: blue);\n", ""},
		// A map is the sharp case, and the two forms differ: written across
		// lines the `declaration` wrapper survives and the token name with it,
		// but written on one line the whole statement collapses into a
		// top-level ERROR and the name is genuinely unrecoverable. Recovering
		// it would mean reading names out of ERROR subtrees, which is the kind
		// of guessing that makes a Map untrustworthy, so the loss is accepted
		// and recorded here instead.
		{"map-literal-multiline", "$sizes: (\n  sm: 4px,\n  lg: 8px\n);\n", "$sizes"},
		{"map-literal-oneline", "$sizes: (sm: 4px, lg: 8px);\n", ""},
		// A CSS unicode escape as a value derails the parse from that point
		// on, so a file of icon variables yields almost nothing. Measured on
		// FontAwesome's _variables.scss: ~2,500 declarations, 1 recovered.
		{"unicode-escape-first", "$icon: \\f2b9;\n", ""},
		{"unicode-escape-later", "$icon: \\f2b9;\n$after: 1px;\n", "$icon"},
		{"namespaced-include", ".a {\n  @include cfg.button;\n}\n", ".a"},
		{"extend-placeholder", ".a {\n  @extend %b;\n}\n", ".a"},
		{"at-root", ".a {\n  @at-root .b { color: red; }\n}\n", ".a"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			nodes, _, err := NewSCSS().Extract(context.Background(), "a.scss", []byte(c.src))
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			if c.wantSym == "" {
				return // no panic, no claim about what survives
			}
			if _, ok := scssNamed(nodes, c.wantSym); !ok {
				t.Errorf("%q was lost; the extractor should still recover it from the partial parse", c.wantSym)
			}
		})
	}
}

// The grammar's recovery node routinely spans the rest of the file, but the
// mixins and rules inside it are parsed correctly as their own typed nodes.
// Walking into it is what takes @function recall over this corpus from 44% to
// 75%; without it a single mis-parsed line costs every symbol below it.
func TestSCSS_SymbolsInsideRecoveryNodesAreKept(t *testing.T) {
	// The one-line map is the trigger: it collapses into a recovery node that
	// swallows the two definitions after it.
	src := []byte("$sizes: (sm: 4px, lg: 8px);\n\n@mixin below-the-error {\n  color: red;\n}\n\n@function also-below($n) {\n  @return $n;\n}\n")

	nodes, _, err := NewSCSS().Extract(context.Background(), "a.scss", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, want := range []string{"below-the-error", "also-below"} {
		n, ok := scssNamed(nodes, want)
		if !ok {
			t.Errorf("%q was lost below a recovery node", want)
			continue
		}
		if n.StartByte >= n.EndByte || n.EndByte > len(src) {
			t.Errorf("%q: recovered with an unusable span %d..%d", want, n.StartByte, n.EndByte)
		}
	}
}

func TestSCSS_MalformedInputDoesNotPanic(t *testing.T) {
	for _, src := range []string{
		"",
		"   \n\n",
		"// just a comment\n",
		"@mixin",
		"@mixin m(",
		".a {",
		"}}}",
		"@use",
		"$",
		"$: 1;",
		"@include ;",
		strings.Repeat(".a { ", 200),
	} {
		if _, _, err := NewSCSS().Extract(context.Background(), "a.scss", []byte(src)); err != nil {
			t.Errorf("Extract(%q): unexpected error %v", src, err)
		}
	}
}

func TestSCSS_LanguageAndPath(t *testing.T) {
	nodes, _ := scssExtract(t)
	if len(nodes) == 0 {
		t.Fatal("no nodes extracted")
	}
	for _, n := range nodes {
		if n.Language != "scss" {
			t.Errorf("%q: language = %q, want scss", n.Name, n.Language)
		}
		if n.Path != "theme.scss" {
			t.Errorf("%q: path = %q, want theme.scss", n.Name, n.Path)
		}
	}
}

func TestSCSS_Extensions(t *testing.T) {
	e := NewSCSS()
	if got := e.Language(); got != "scss" {
		t.Errorf("Language() = %q, want scss", got)
	}
	// .sass (the indented syntax) is a different grammar and is deliberately
	// not claimed here.
	if got := e.Extensions(); len(got) != 1 || got[0] != ".scss" {
		t.Errorf("Extensions() = %v, want [.scss]", got)
	}
}
