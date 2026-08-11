package treesitter

import (
	"context"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

var xmlSrc = []byte(`<?xml version="1.0" encoding="UTF-8"?>
<?xml-stylesheet href="render.xsl"?>
<!-- The order schema. -->
<xs:schema xmlns:xs="http://www.w3.org/2001/XMLSchema">
  <xs:import namespace="urn:common" schemaLocation="common.xsd"/>
  <xs:include schemaLocation="types.xsd"/>

  <xs:element name="Order">
    <xs:complexType name="OrderType">
      <xs:sequence>
        <xs:element name="LineItem" ref="Item"/>
      </xs:sequence>
    </xs:complexType>
  </xs:element>

  <plain>
    <wrapper>
      <deep id="buried"/>
    </wrapper>
  </plain>

  <xs:element name="Invoice"/>
</xs:schema>
`)

func xmlExtract(t *testing.T) ([]topology.Node, []topology.Edge) {
	t.Helper()
	nodes, edges, err := NewXML().Extract(context.Background(), "schema.xsd", xmlSrc)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return nodes, edges
}

func xmlNamed(nodes []topology.Node, name string) (topology.Node, bool) {
	for _, n := range nodes {
		if n.Name == name {
			return n, true
		}
	}
	return topology.Node{}, false
}

func TestXml_LandmarksExtracted(t *testing.T) {
	nodes, _ := xmlExtract(t)

	want := map[string]topology.NodeKind{
		"render.xsl": topology.KindImport,
		"common.xsd": topology.KindImport,
		"types.xsd":  topology.KindImport,
		"xs:schema":  topology.KindSection,
		"Order":      topology.KindConstant,
		"OrderType":  topology.KindConstant,
		"LineItem":   topology.KindConstant,
		"buried":     topology.KindConstant,
		"Invoice":    topology.KindConstant,
	}
	for name, kind := range want {
		n, ok := xmlNamed(nodes, name)
		if !ok {
			t.Errorf("missing landmark %q", name)
			continue
		}
		if n.Kind != kind {
			t.Errorf("%q: kind = %q, want %q", name, n.Kind, kind)
		}
	}
}

// The whole design rests on most elements being transparent. If plain wrapper
// tags started emitting nodes, a Maven pom would flood the Map — the explosion
// HTMLExtractor documents.
func TestXml_UninterestingElementsAreTransparent(t *testing.T) {
	nodes, _ := xmlExtract(t)

	for _, tag := range []string{"xs:sequence", "wrapper"} {
		if _, ok := xmlNamed(nodes, tag); ok {
			t.Errorf("%q has no identity and is not near the root; it should be transparent", tag)
		}
	}
	// `plain` is a direct child of the root, so it IS structural.
	if _, ok := xmlNamed(nodes, "plain"); !ok {
		t.Error("a direct child of the root should be emitted as structure")
	}
}

// A transparent element must still pass its parent through, so an identified
// element buried under wrappers lands under the nearest real landmark rather
// than being lost or inventing intermediate nodes.
func TestXml_BuriedIdentityIsKeptUnderNearestLandmark(t *testing.T) {
	nodes, edges := xmlExtract(t)

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

	if !contains("plain", "buried") {
		t.Error("an identified element under a transparent wrapper should hang off the nearest landmark")
	}
	if !contains("Order", "OrderType") {
		t.Error("element nesting should become containment")
	}

	buried, _ := xmlNamed(nodes, "buried")
	if got, want := buried.Qualified, "xs:schema/plain/wrapper/buried"; got != want {
		t.Errorf("qualified = %q, want %q — a transparent tag should still contribute to the path", got, want)
	}
}

// `name` outranks `ref`: an element that both names itself and points at
// something else is identified by its own name.
func TestXml_IdentifierPrecedence(t *testing.T) {
	nodes, _ := xmlExtract(t)

	n, ok := xmlNamed(nodes, "LineItem")
	if !ok {
		t.Fatal("LineItem not extracted")
	}
	if got, want := n.Signature, "xs:element name"; got != want {
		t.Errorf("signature = %q, want %q", got, want)
	}
	if _, ok := xmlNamed(nodes, "Item"); ok {
		t.Error("the weaker `ref` attribute should not also produce a node")
	}
}

// An import is an import even though `xs:import` also carries a `namespace`
// attribute that the identifier rule would otherwise match.
func TestXml_ImportsOutrankIdentity(t *testing.T) {
	nodes, _ := xmlExtract(t)

	n, ok := xmlNamed(nodes, "common.xsd")
	if !ok {
		t.Fatal("xs:import was not recorded as an import")
	}
	if n.Kind != topology.KindImport {
		t.Errorf("kind = %q, want %q", n.Kind, topology.KindImport)
	}
	if _, ok := xmlNamed(nodes, "urn:common"); ok {
		t.Error("the namespace attribute should not produce a second node")
	}
}

func TestXml_ByteSpansReconstructTheSource(t *testing.T) {
	nodes, _ := xmlExtract(t)

	if len(nodes) == 0 {
		t.Fatal("no nodes extracted")
	}
	for _, n := range nodes {
		if n.StartByte >= n.EndByte {
			t.Errorf("%q: inverted or empty span %d..%d", n.Name, n.StartByte, n.EndByte)
			continue
		}
		if n.EndByte > len(xmlSrc) {
			t.Errorf("%q: span %d..%d runs past the source (%d bytes)", n.Name, n.StartByte, n.EndByte, len(xmlSrc))
		}
	}

	last, ok := xmlNamed(nodes, "Invoice")
	if !ok {
		t.Fatal("the declaration nearest EOF was lost, which is what a parse cascade looks like")
	}
	if got := string(xmlSrc[last.StartByte:last.EndByte]); !strings.Contains(got, `name="Invoice"`) {
		t.Errorf("span does not reconstruct the element: %q", got)
	}
}

func TestXml_DocSpanCoversPrecedingComment(t *testing.T) {
	nodes, _ := xmlExtract(t)

	root, ok := xmlNamed(nodes, "xs:schema")
	if !ok {
		t.Fatal("root element not extracted")
	}
	if root.DocStartByte >= root.DocEndByte {
		t.Fatalf("no doc span for an element preceded by a comment: %d..%d", root.DocStartByte, root.DocEndByte)
	}
	if got := string(xmlSrc[root.DocStartByte:root.DocEndByte]); !strings.Contains(got, "The order schema") {
		t.Errorf("doc span = %q, want the XML comment above the root", got)
	}
}

// Empty input is the one place the XML grammar differs from every other grammar
// in this package: it returns a nil root where JSON, HTML and TOML return a
// real one. extractWith has no reason to guard that, so xml.go does — and this
// pins it, because the failure mode is a panic in the indexer rather than a bad
// result.
func TestXml_EmptyInputDoesNotPanic(t *testing.T) {
	for _, src := range []string{"", "   ", "\n\n\t"} {
		nodes, edges, err := NewXML().Extract(context.Background(), "a.xml", []byte(src))
		if err != nil {
			t.Errorf("Extract(%q): unexpected error %v", src, err)
		}
		if len(nodes) != 0 || len(edges) != 0 {
			t.Errorf("Extract(%q) = %d nodes, %d edges; want nothing", src, len(nodes), len(edges))
		}
	}
}

func TestXml_MalformedInputDoesNotPanic(t *testing.T) {
	for _, src := range []string{
		"<a><b></a>",
		"<a>>>></a>",
		"<",
		"<<<<",
		"<a",
		"</a>",
		`<a id="unclosed>`,
		"<!DOCTYPE",
		"<![CDATA[",
		"<?xml",
		strings.Repeat("<a>", 300),
		"\x00\x01\x02",
	} {
		if _, _, err := NewXML().Extract(context.Background(), "a.xml", []byte(src)); err != nil {
			t.Errorf("Extract(%q): unexpected error %v", src, err)
		}
	}
}

// A document with no attributes anywhere — the common Maven/Ant shape — must
// still yield a usable outline, which is the entire reason structural elements
// are emitted alongside identified ones.
func TestXml_AttributelessDocumentStillHasAnOutline(t *testing.T) {
	src := []byte("<project>\n  <artifactId>demo</artifactId>\n  <dependencies>\n    <dependency>\n      <groupId>g</groupId>\n    </dependency>\n  </dependencies>\n</project>\n")

	nodes, _, err := NewXML().Extract(context.Background(), "pom.xml", src)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, want := range []string{"project", "artifactId", "dependencies"} {
		if _, ok := xmlNamed(nodes, want); !ok {
			t.Errorf("%q missing from the outline", want)
		}
	}
	// Bounded: the grandchildren stay transparent.
	if _, ok := xmlNamed(nodes, "groupId"); ok {
		t.Error("a grandchild with no identity should not be emitted; that is the flood this design avoids")
	}
}

// A long run of identical tags is a data array, not structure. Without this
// bound a single CPU-intrinsics file in a real sweep emitted 14,188 nodes and
// buried every document around it in ranked search.
func TestXml_RepeatedSiblingsAreBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString("<intrinsics>\n")
	for i := range xmlMaxRepeatedSiblings + 50 {
		b.WriteString(`  <intrinsic name="i`)
		b.WriteString(strings.Repeat("x", 1))
		b.WriteString(string(rune('0' + i%10)))
		b.WriteString("\"/>\n")
	}
	b.WriteString("  <tail id=\"kept\"/>\n</intrinsics>\n")

	nodes, _, err := NewXML().Extract(context.Background(), "a.xml", []byte(b.String()))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	var intrinsics int
	for _, n := range nodes {
		if n.Signature == "intrinsic name" {
			intrinsics++
		}
	}
	if intrinsics > xmlMaxRepeatedSiblings {
		t.Errorf("walked %d repeated siblings, cap is %d", intrinsics, xmlMaxRepeatedSiblings)
	}
	if intrinsics == 0 {
		t.Error("the cap should bound the run, not suppress it")
	}
	// A differently-named sibling after the run is unaffected: the count is per
	// tag, not per container.
	if _, ok := xmlNamed(nodes, "kept"); !ok {
		t.Error("a different tag after a capped run should still be emitted")
	}
}

func TestXml_LanguageAndPath(t *testing.T) {
	nodes, _ := xmlExtract(t)
	if len(nodes) == 0 {
		t.Fatal("no nodes extracted")
	}
	for _, n := range nodes {
		if n.Language != "xml" {
			t.Errorf("%q: language = %q, want xml", n.Name, n.Language)
		}
		if n.Path != "schema.xsd" {
			t.Errorf("%q: path = %q, want schema.xsd", n.Name, n.Path)
		}
	}
}

func TestXml_Extensions(t *testing.T) {
	e := NewXML()
	if got := e.Language(); got != "xml" {
		t.Errorf("Language() = %q, want xml", got)
	}
	got := e.Extensions()
	if len(got) != 3 || got[0] != ".xml" || got[1] != ".xsd" || got[2] != ".xsl" {
		t.Errorf("Extensions() = %v, want [.xml .xsd .xsl]", got)
	}
}
