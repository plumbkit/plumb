package tools

import (
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/topology"
)

// A related declaration whose body follows does not print its signature twice: the
// line keeps the doc, the body opens with the signature, and a body cut for budget
// carries the signature on its omission line instead. Control: a declaration with
// no body keeps the signature on its line.
func TestRelatedText_TheSignatureGoesWithTheBody(t *testing.T) {
	const sig, doc = "func Apply(subtotal int, d Discount) (int)", "Apply takes d off subtotal."
	r := contextRelated{
		Node:     topology.Node{Kind: topology.KindFunction, Name: "Apply", Path: "pricing/discount.go", StartLine: 3, Signature: sig, Docstring: doc},
		Evidence: evidenceDerived, Source: sourceCallResolver, Conf: 0.85, Via: "callee of Total",
		Body: contextBody{File: 0, State: bodyReady, Start: 3, End: 5, Text: "func Apply(subtotal int, d Discount) int {\n\treturn subtotal\n}\n", SHA: strings.Repeat("a", 64)},
	}
	withBody := relatedText(r)
	if strings.Contains(withBody, sig) {
		t.Errorf("a related line with a body repeats the signature:\n%s", withBody)
	}
	if !strings.Contains(withBody, "// "+doc) {
		t.Errorf("a related line with a body lost its doc line:\n%s", withBody)
	}

	p := &contextPack{
		Root: "/ws", MaxBytes: 12000,
		Files:   []bodyFile{{Path: "pricing/discount.go", Abs: "/ws/pricing/discount.go", MTime: time.Unix(1, 0), SHA: strings.Repeat("b", 64)}},
		Related: []contextRelated{r},
	}
	line, ok := p.relatedBodyLine(0)
	if !ok || line.body == nil || len(line.body.lean) == 0 {
		t.Fatalf("no body unit for a ready body: %+v", line)
	}
	if !strings.Contains(line.body.lean[0], sig) {
		t.Errorf("a body cut for budget does not carry the signature on its omission line: %q", line.body.lean[0])
	}
	if !strings.HasPrefix(line.body.full[strings.Index(line.body.full, "\n3\t")+1:], "3\tfunc Apply(") {
		t.Errorf("the delivered body does not open with the signature it now carries alone:\n%s", line.body.full)
	}

	r.Body = contextBody{File: -1, State: bodyUnavailable, Why: "the file cannot be read"}
	if without := relatedText(r); !strings.Contains(without, sig) {
		t.Errorf("control: a related line with no body must keep its signature:\n%s", without)
	}
}

// Documents the source-read budget ran out before are one gap line per reason, not
// one per document. A single one keeps the familiar wording.
func TestUnreadDocs_OneLinePerReason(t *testing.T) {
	var u unreadDocs
	const spent = "the source-read budget was spent"
	u.add("docs/a.md", spent)
	u.add("docs/b.md", spent)
	u.add("docs/c.md", "the file cannot be read")
	u.add("docs/d.md", spent)
	got := u.lines()
	want := []string{
		"document sections: 3 were not read (" + spent + "): docs/a.md, docs/b.md, docs/d.md",
		"document sections: docs/c.md was not read: the file cannot be read",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("lines = %q, want %q", got, want)
	}
	var none unreadDocs
	if len(none.lines()) != 0 {
		t.Errorf("no unread document still produced a line: %q", none.lines())
	}
}
