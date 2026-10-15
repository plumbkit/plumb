package tools

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

// context_uses_test.go — a delivered body is followed by the same-file
// package-level constants and variables it names (context_uses.go).

// usesFixture is a Go package whose bodies use constants and variables declared
// beside them, one declared in a sibling file, and a Python module of the same
// shape.
var usesFixture = map[string]string{
	"go.mod": "module example.com/u\n\ngo 1.22\n",
	"store/store.go": `package store

import "fmt"

const maxRows = 64

// listSQL is the query List runs.
const listSQL = ` + "`" + `SELECT id
FROM rows
WHERE n < ?` + "`" + `

var (
	defaultName = "x(" // a bracket in a string does not open anything
	table       = map[string]int{
		"a": 1,
	}
	unused = 3
)

const seq = 1

type T struct{ seq int }

func List(t T, n int) string {
	if n > maxRows {
		n = maxRows
	}
	return fmt.Sprint(listSQL, n, defaultName, table["a"], t.seq, elsewhere)
}

func Shadow() int {
	seq := 2
	return seq
}

var big = []string{
	"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12", "13",
	"14",
	"15",
	"16",
	"17",
	"18",
	"19",
	"20",
	"21",
	"22",
	"23",
}

func Big() int { return len(big) }

func ShadowVar() int {
	var seq = 3
	return seq
}

func Assign() (int, error) {
	var n int
	var err error
	defaultName, err = "y", nil
	n = seq
	return n, err
}

func Multi() int {
	a, seq := 1, 2
	return a + seq
}

func Multi2() int {
	seq, err := 2, error(nil)
	_ = err
	return seq
}
`,
	"store/other.go": "package store\n\nconst elsewhere = 5\n",
	"store/many.go": "package store\n\nvar _ = m1\n\nconst (\n\tm1 = 1\n\tm2 = 2\n\tm3 = 3\n\tm4 = 4\n\tm5 = 5\n\tm6 = 6\n\tm7 = 7\n\tm8 = 8\n\tm9 = 9\n\tm10 = 10\n)\n\n" +
		"func Many() int { _ = m1; return m1 + m2 + m3 + m4 + m5 + m6 + m7 + m8 + m9 + m10 }\n",
	"store/caller.go": "package store\n\nfunc Caller() string { return List(T{}, 1) }\n",
	// A declaration that is not valid UTF-8 is not offered, as a body is not.
	"store/bad.go": "package store\n\nconst bad = 1 // \xff\n\nconst good = 2\n\nfunc UsesBad() int { return bad + good }\n",
	"py/mod.py":    "LIMIT = 5\n\n\ndef cap(n):\n    return min(n, LIMIT)\n",
	// A class attribute is named through its class, not a file-level use; an
	// assignment in the body shadows the module's name.
	"py/cls.py": "RATE = 2\nSIZE = 7\nROUTES = [\n    \"a\",\n    \"b\",\n]\n\n\nclass Box:\n    SIZE = 3\n\n    def area(self):\n        return SIZE * RATE + len(ROUTES)\n\n\n" +
		"def shadowed(n):\n    RATE = 1\n    return RATE + n\n",
}

func newUsesTool(t *testing.T) shopTool {
	t.Helper()
	root := t.TempDir()
	for rel, body := range usesFixture {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return newShopTool(t, openContextStore(t, root, 7), root)
}

// usesBlock is the uses record that follows the body of the seed or related
// declaration whose line names name, or "" when it has none.
func usesBlock(t *testing.T, out, name string) string {
	t.Helper()
	at := strings.Index(out, name+" — ")
	if at < 0 {
		t.Fatalf("no line names %s:\n%s", name, out)
	}
	rest := out[at:]
	u := strings.Index(rest, "    uses, declared in this file: ")
	if u < 0 {
		return ""
	}
	// The record belongs to name only if no other declaration line comes between.
	if next := regexp.MustCompile(`\n  (symbol |\[e\d)`).FindStringIndex(rest[1:]); next != nil && next[0]+1 < u {
		return ""
	}
	// The record ends at the next line that is not one of its own: a section
	// heading (a letter in column 0) or a declaration line (two spaces, then text).
	// Its own lines are gutter lines (digits, possibly space-padded) and handoffs.
	end := regexp.MustCompile(`\n(?:[A-Za-z]|  [^\s\d])`).FindStringIndex(rest[u:])
	if end == nil {
		return rest[u:]
	}
	return rest[u : u+end[0]]
}

func TestContextForTask_UsesListsTheSameFileConstantsAndVariablesABodyNames(t *testing.T) {
	s := newUsesTool(t)
	out, err := s.run(t, map[string]any{"symbols": []string{"store/store.go#List"}, "max_bytes": 12000})
	if err != nil {
		t.Fatal(err)
	}
	got := usesBlock(t, out, "List")
	wantHead := "    uses, declared in this file: maxRows (constant, line 5), listSQL (constant, lines 8–10), " +
		"defaultName (variable, line 13), table (variable, lines 14–16)\n"
	if !strings.HasPrefix(got, wantHead) {
		t.Fatalf("uses record:\n%s\nwant it to start %q; whole pack:\n%s", got, wantHead, out)
	}
	for _, line := range []string{
		"5\tconst maxRows = 64", "8\tconst listSQL = `SELECT id", "9\tFROM rows", "10\tWHERE n < ?`",
		"13\t\tdefaultName = \"x(\" // a bracket", "14\t\ttable       = map[string]int{", "16\t\t}",
	} {
		if !strings.Contains(got, line) {
			t.Errorf("uses record lacks %q:\n%s", line, got)
		}
	}
	// Not listed: a name declared in a sibling file, one the body never names, one
	// it reaches only through a selector (t.seq), and the body's own declaration.
	for _, absent := range []string{"elsewhere", "unused", "seq (", "List ("} {
		if strings.Contains(got, absent) {
			t.Errorf("uses record lists %q:\n%s", absent, got)
		}
	}
}

func TestContextForTask_UsesSkipsANameTheBodyDeclaresItself(t *testing.T) {
	s := newUsesTool(t)
	out, err := s.run(t, map[string]any{"symbols": []string{"store/store.go#Shadow", "store/store.go#List"}, "max_bytes": 12000})
	if err != nil {
		t.Fatal(err)
	}
	if got := usesBlock(t, out, "Shadow"); got != "" {
		t.Errorf("Shadow declares its own seq, so it uses no package name, got:\n%s", got)
	}
	vout, err := s.run(t, map[string]any{"symbols": []string{"store/store.go#ShadowVar"}, "max_bytes": 12000})
	if err != nil {
		t.Fatal(err)
	}
	if got := usesBlock(t, vout, "ShadowVar"); got != "" {
		t.Errorf("ShadowVar declares var seq, so it uses no package name, got:\n%s", got)
	}
	// := with more names declares seq wherever it stands; a plain assignment
	// (defaultName, err = …) declares nothing, so Assign uses the package's
	// defaultName and seq.
	mout, err := s.run(t, map[string]any{"symbols": []string{"store/store.go#Multi", "store/store.go#Multi2", "store/store.go#Assign"}, "max_bytes": 12000})
	if err != nil {
		t.Fatal(err)
	}
	for _, fn := range []string{"Multi", "Multi2"} {
		if got := usesBlock(t, mout, fn); got != "" {
			t.Errorf("%s declares seq with :=, so it uses no package name, got:\n%s", fn, got)
		}
	}
	if got := usesBlock(t, mout, "Assign"); !strings.Contains(got, "defaultName (variable, line 13)") || !strings.Contains(got, "seq (constant, line 20)") {
		t.Errorf("Assign only assigns, so it uses the package's defaultName and seq:\n%s", mout)
	}
	// Positive control: in the same pack, List's record is there.
	if usesBlock(t, out, "List") == "" {
		t.Fatalf("List's uses record is missing, so the absence above proves nothing:\n%s", out)
	}
}

func TestContextForTask_UsesCutsALongDeclarationToItsFirstLine(t *testing.T) {
	s := newUsesTool(t)
	out, err := s.run(t, map[string]any{"symbols": []string{"store/store.go#Big"}, "max_bytes": 12000})
	if err != nil {
		t.Fatal(err)
	}
	got := usesBlock(t, out, "Big")
	if !strings.HasPrefix(got, "    uses, declared in this file: big (variable, line 36)\n36\tvar big = []string{\n") {
		t.Fatalf("want big's first line only, got:\n%s", got)
	}
	if !strings.Contains(got, "… longer than 12 lines: read_symbol {") || strings.Contains(got, `"14",`) {
		t.Errorf("a declaration over %d lines is a first line and a handoff, got:\n%s", contextUsesLines, got)
	}
}

func TestContextForTask_UsesFollowsARelatedBodyToo(t *testing.T) {
	s := newUsesTool(t)
	out, err := s.run(t, map[string]any{"symbols": []string{"store/caller.go#Caller"}, "max_bytes": 12000})
	if err != nil {
		t.Fatal(err)
	}
	related := strings.Index(out, "\nrelated (")
	got := usesBlock(t, out, "List")
	if related < 0 || got == "" || strings.Index(out, got) < related {
		t.Fatalf("want List's uses record under its related body:\n%s", out)
	}
	if !strings.Contains(got, "maxRows (constant, line 5)") {
		t.Errorf("related uses record: %s", got)
	}
}

// A constant seed's body is its own declaration: it is not listed as its own use.
func TestContextForTask_UsesNeverListsTheBodysOwnDeclaration(t *testing.T) {
	s := newUsesTool(t)
	out, err := s.run(t, map[string]any{"symbols": []string{"store/store.go#maxRows", "store/store.go#List"}, "max_bytes": 12000})
	if err != nil {
		t.Fatal(err)
	}
	if got := usesBlock(t, out, "maxRows"); got != "" {
		t.Errorf("a constant seed lists itself:\n%s", got)
	}
	if !strings.Contains(usesBlock(t, out, "List"), "maxRows (constant, line 5)") {
		t.Fatalf("positive control: List, in the same pack, lists maxRows:\n%s", out)
	}
}

func TestContextForTask_UsesListsAtMostEightPerBody(t *testing.T) {
	s := newUsesTool(t)
	out, err := s.run(t, map[string]any{"symbols": []string{"store/many.go#Many"}, "max_bytes": 12000})
	if err != nil {
		t.Fatal(err)
	}
	got := usesBlock(t, out, "Many")
	if n := strings.Count(strings.SplitN(got, "\n", 2)[0], "(constant, "); n != contextUsesMax {
		t.Errorf("listed %d of the 10 constants Many names, want the first %d:\n%s", n, contextUsesMax, got)
	}
	if !strings.Contains(got, "m8 (constant") || strings.Contains(got, "m9 (constant") {
		t.Errorf("want m1..m8 in file order, got:\n%s", got)
	}
	if !strings.Contains(strings.SplitN(got, "\n", 2)[0], ", and 2 more not listed") {
		t.Errorf("the two beyond the cap must be counted, got:\n%s", got)
	}
}

func TestContextForTask_UsesSkipsADeclarationThatIsNotUTF8(t *testing.T) {
	s := newUsesTool(t)
	out, err := s.run(t, map[string]any{"symbols": []string{"store/bad.go#UsesBad"}, "max_bytes": 12000})
	if err != nil {
		t.Fatal(err)
	}
	got := usesBlock(t, out, "UsesBad")
	if !strings.HasPrefix(got, "    uses, declared in this file: good (constant, line 5)") || strings.Contains(got, "bad (") {
		t.Errorf("want good listed and bad (invalid UTF-8) left out, got:\n%s", got)
	}
}

// A uses record needs its body delivered whole, and ranks below every seed body
// and best-ranked related declaration: at no budget does it show beside a body
// that is only a status line, or while another seed's body is cut.
func TestContextForTask_UsesNeverOutlivesItsBodyNorOutranksAnother(t *testing.T) {
	s := newUsesTool(t)
	var seen, cut bool
	for mb := contextMinMaxBytes; mb <= 6000; mb += 25 {
		out, err := s.run(t, map[string]any{"symbols": []string{"store/store.go#List", "store/store.go#Big"}, "max_bytes": mb})
		if err != nil {
			t.Fatal(err)
		}
		listBody := strings.Contains(out, "\n24\tfunc List(t T, n int) string {")
		bigBody := strings.Contains(out, "\n50\tfunc Big() int { return len(big) }")
		if strings.Contains(out, "uses, declared in this file: maxRows") {
			seen = true
			if !listBody || !bigBody {
				t.Errorf("max_bytes %d: List's uses record shown with List's body %v and Big's body %v:\n%s", mb, listBody, bigBody, out)
			}
		}
		if !listBody && strings.Contains(out, "body omitted for budget") {
			cut = true
		}
	}
	if !seen || !cut {
		t.Fatalf("the sweep must reach both a pack with List's record (%v) and one with a body cut (%v)", seen, cut)
	}
}

// Python bodies get the uses record too: a module-level binding the body names
// is listed; a class attribute is not (it is named through its class), and an
// assignment in the body shadows the module's name.
func TestContextForTask_UsesServesPython(t *testing.T) {
	s := newUsesTool(t)
	pack := s.collect(t, map[string]any{"symbols": []string{"py/mod.py#cap", "py/cls.py#area", "py/cls.py#shadowed"}, "max_bytes": 12000})
	uses := map[string][]string{}
	decls := map[string]usedDecl{}
	for i, sd := range pack.Seeds {
		b := pack.Bodies[i]
		if b.State != bodyReady {
			t.Fatalf("%s: body not delivered: %+v", sd.Name, b)
		}
		var names []string
		for _, u := range b.Uses {
			names = append(names, u.Name)
			decls[sd.Name+"/"+u.Name] = u
		}
		uses[sd.Name] = names
	}
	// The declaration is the whole statement the extractor recorded, the
	// multi-line list included; the module's SIZE (line 2), not Box's.
	if d := decls["area/ROUTES"]; d.Cut || d.Start != 3 || d.End != 6 || !strings.Contains(d.Text, "\"b\",") {
		t.Errorf("ROUTES declaration = %+v, want lines 3-6 whole", d)
	}
	if d := decls["area/SIZE"]; d.Start != 2 {
		t.Errorf("SIZE declaration = %+v, want the module's, line 2", d)
	}
	if !slices.Equal(uses["cap"], []string{"LIMIT"}) {
		t.Errorf("cap uses %v, want [LIMIT]", uses["cap"])
	}
	if !slices.Equal(uses["area"], []string{"RATE", "SIZE", "ROUTES"}) {
		t.Errorf("area uses %v, want [RATE SIZE ROUTES] (Box.SIZE, a class attribute, is not file-level)", uses["area"])
	}
	if len(uses["shadowed"]) != 0 {
		t.Errorf("shadowed uses %v, want none: its RATE is a local", uses["shadowed"])
	}
}

func TestUsesHelpers_PerLanguage(t *testing.T) {
	if got := bareNames("return $state.x + limit_1 + obj.skip", "typescript"); !got["$state"] || !got["limit_1"] || got["skip"] {
		t.Errorf("typescript bare names = %v", got)
	}
	cases := []struct {
		text, name, lang string
		want             bool
	}{
		{"x := LIMIT", "x", "go", true},
		{"var LIMIT = 1", "LIMIT", "go", true},
		{"y := LIMIT", "LIMIT", "go", false},
		{"LIMIT = 1", "LIMIT", "python", true},
		{"LIMIT: int = 1", "LIMIT", "python", true},
		{"for LIMIT in xs:", "LIMIT", "python", true},
		{"if LIMIT == 1:", "LIMIT", "python", false},
		{"self.LIMIT = 1", "LIMIT", "python", false},
		{"const LIMIT = 1", "LIMIT", "typescript", true},
		{"let LIMIT = 1", "LIMIT", "javascript", true},
		{"return LIMIT", "LIMIT", "javascript", false},
	}
	for _, c := range cases {
		if got := shadows(c.text, c.name, c.lang); got != c.want {
			t.Errorf("shadows(%q, %q, %s) = %v, want %v", c.text, c.name, c.lang, got, c.want)
		}
	}
	if !usesLanguage("javascript") || usesLanguage("rust") {
		t.Error("usesLanguage serves Go, Python, TypeScript and JavaScript only")
	}
}

// A body the caller already holds gets no uses record; the same body delivered
// does.
func TestContextForTask_UsesFollowsOnlyADeliveredBody(t *testing.T) {
	s := newUsesTool(t)
	args := map[string]any{"symbols": []string{"store/store.go#List"}, "max_bytes": 12000}
	out, err := s.run(t, args)
	if err != nil {
		t.Fatal(err)
	}
	if usesBlock(t, out, "List") == "" {
		t.Fatalf("positive control: a delivered body has its record:\n%s", out)
	}
	sha := regexp.MustCompile(`content_sha256=([0-9a-f]{64})`).FindStringSubmatch(out)
	if sha == nil {
		t.Fatalf("no content_sha256 in:\n%s", out)
	}
	args["have"] = []map[string]any{{"symbol": "store/store.go#List", "content_sha256": sha[1]}}
	if out, err = s.run(t, args); err != nil {
		t.Fatal(err)
	}
	if got := usesBlock(t, out, "List"); got != "" {
		t.Errorf("a held body gets no uses record, got:\n%s", got)
	}
	if strings.Contains(out, " uses)") || strings.Contains(out, ", 1 uses") || strings.Contains(out, "(1 uses") {
		t.Errorf("a held body's record is not made at all, so none is counted as omitted:\n%s", out)
	}
}

// A record that does not fit is counted in the omission footer as "uses".
func TestContextForTask_UsesIsCountedWhenDropped(t *testing.T) {
	s := newUsesTool(t)
	for mb := contextMinMaxBytes; mb <= 6000; mb += 25 {
		out, err := s.run(t, map[string]any{"symbols": []string{"store/store.go#List"}, "max_bytes": mb})
		if err != nil {
			t.Fatal(err)
		}
		if regexp.MustCompile(`omitted: \d+ item\(s\) \([^)]*\b1 uses\b`).MatchString(out) {
			return
		}
	}
	t.Fatal("no budget counted a dropped uses record")
}

// The packer's side, on hand-built records: a uses record shows whole when it
// fits, as its names-only line when only that fits, and not at all, counted,
// when its body was not delivered whole, even with room to spare for it.
func TestPackLines_UsesDegradesAndNeedsItsWholeBody(t *testing.T) {
	body := &bodyUnit{file: 0, full: strings.Repeat("B", 200), noGuard: strings.Repeat("B", 200), lean: []string{"body omitted"}}
	lines := []packLine{
		{text: "header", prio: prioHeader, section: secHeader},
		{prio: prioBody, section: secSeeds, class: classBody, body: body},
		{text: strings.Repeat("U", 300), alts: []string{"uses-lean"}, prio: prioUses, section: secSeeds, class: classUses, needs: body},
	}
	base := len("header") + 1 + 200
	for _, c := range []struct {
		budget                   int
		wantBody, wantUses, note string
	}{
		{base + 1 + 300, strings.Repeat("B", 200), strings.Repeat("U", 300), "everything fits"},
		{footerCeiling() + base + 1 + len("uses-lean") + 5, strings.Repeat("B", 200), "uses-lean", "only the names-only line fits"},
		{footerCeiling() + len("header") + 1 + 100, "body omitted", "", "the body is a status line: no record, though it would fit"},
		{100000, "too big", "", "a body with no complete form, with room for everything"},
	} {
		ls := lines
		if c.wantBody == "too big" {
			// A body larger than the budget has status lines only, even when the rest fits.
			big := &bodyUnit{file: 0, lean: []string{"too big"}}
			ls = []packLine{lines[0], {prio: prioBody, section: secSeeds, class: classBody, body: big}, lines[2]}
			ls[2].needs = big
		}
		out := packLines(ls, c.budget).Text
		if !strings.Contains(out, c.wantBody) {
			t.Errorf("%s: want the body as %.12q, got %q", c.note, c.wantBody, out)
		}
		gotUses := strings.Contains(out, "uses-lean") || strings.Contains(out, "UUU")
		if c.wantUses == "" {
			if gotUses || !strings.Contains(out, "1 uses") {
				t.Errorf("%s: want no record and a counted one, got %q", c.note, out)
			}
		} else if !strings.Contains(out, c.wantUses) {
			t.Errorf("%s: want the record as %.12q, got %q", c.note, c.wantUses, out)
		}
	}
}

// A uses record is an annotation: when it is dropped, whether it does not fit or
// its body was cut, what ranks below it (the affected tests) is still offered.
func TestPackLines_ADroppedUsesRecordDoesNotEndThePack(t *testing.T) {
	affected := packLine{text: "affected-line", prio: prioAffected, section: secAffected, class: classAffected}
	for _, c := range []struct {
		body *bodyUnit
		note string
	}{
		{&bodyUnit{full: strings.Repeat("B", 200), noGuard: strings.Repeat("B", 200), lean: []string{"body omitted"}}, "the record fits no tier"},
		{&bodyUnit{lean: []string{"too big"}}, "the record's body is a status line"},
	} {
		lines := []packLine{
			{text: "header", prio: prioHeader, section: secHeader},
			{prio: prioBody, section: secSeeds, class: classBody, body: c.body},
			{text: strings.Repeat("U", 300), alts: []string{strings.Repeat("u", 290)}, prio: prioUses, section: secSeeds, class: classUses, needs: c.body},
			affected,
		}
		// Room for the header, the body (whole when it has a whole form), the affected
		// line and the footer, but not for either form of the record.
		budget := footerCeiling() + len("header") + 1 + 200 + 1 + len("affected-line") + 1 + 50
		out := packLines(lines, budget).Text
		if !strings.Contains(out, "affected-line") || strings.Contains(out, "uuu") || !strings.Contains(out, "1 uses") {
			t.Errorf("%s: want the affected line kept and the record counted as omitted, got %q", c.note, out)
		}
	}
}

// The record's names-only form is exactly its first line: names, kinds and lines.
func TestUsesLine_TheNamesOnlyFormIsTheHead(t *testing.T) {
	p := &contextPack{Files: []bodyFile{{Abs: "/w/x.go"}}}
	b := contextBody{State: bodyReady, File: 0, Uses: []usedDecl{
		{Name: "a", Kind: topology.KindConstant, Start: 3, End: 3, Text: "const a = 1\n"},
		{Name: "q", Kind: topology.KindConstant, Start: 5, End: 6, Text: "const q = \"x\" +\n\t\"y\"\n"},
	}, UsesMore: 1}
	l, ok := p.usesLine(b, &bodyUnit{}, secSeeds)
	head := "    uses, declared in this file: a (constant, line 3), q (constant, lines 5–6), and 1 more not listed"
	if !ok || len(l.alts) != 1 || l.alts[0] != head {
		t.Fatalf("names-only form: %q, want %q", l.alts, head)
	}
	if l.text != head+"\n3\tconst a = 1\n5\tconst q = \"x\" +\n6\t\t\"y\"" {
		t.Errorf("full form:\n%s", l.text)
	}
	if _, ok := p.usesLine(b, nil, secSeeds); ok {
		t.Error("a record with no body to follow is not made")
	}
}

// A declaration of exactly contextUsesLines lines is shown whole; one line more
// and it is cut to its first line.
func TestDeclLines_TheLineLimitIsInclusive(t *testing.T) {
	for _, c := range []struct {
		n   int
		cut bool
	}{{contextUsesLines, false}, {contextUsesLines + 1, true}} {
		lines := []string{"var x = []int{"}
		for len(lines) < c.n-1 {
			lines = append(lines, "\t1,")
		}
		lines = append(lines, "}")
		d, ok := (&bodySource{lines: lines}).declLines(topology.Node{Name: "x", Kind: topology.KindVariable, StartLine: 1}, "go")
		if !ok || d.Cut != c.cut || (!c.cut && d.End != c.n) || (c.cut && d.Text != "var x = []int{\n") {
			t.Errorf("%d lines: got %+v, %v; want cut=%v", c.n, d, ok, c.cut)
		}
	}
}

func TestDeclEnd(t *testing.T) {
	for _, c := range []struct {
		src  string // the spec's lines, the first holding the name
		end  int    // the line it ends on, 0 when it does not end in the window
		note string
	}{
		{"const a = 1\nconst b = 2", 1, "one line"},
		{"\ttable = map[string]int{\n\t\t\"a\": 1,\n\t}\n\tnext = 2", 3, "a composite literal"},
		{"const q = \"a\" +\n\t\"b\" +\n\t\"c\"\nconst r = 1", 3, "a trailing operator continues the spec"},
		{"var ok = a ||\n\tb", 2, "so does a trailing ||"},
		{"const s = `SELECT (\nFROM t)`\nconst r = 1", 2, "a raw string, brackets inside it ignored"},
		{"var c = 1 /* ( */\nvar d = 2", 1, "a block comment's bracket is not one"},
		{"var r = '`'\nvar d = 2", 1, "a rune holding a backtick"},
		{"var x = \"({[\" // (\nvar d = 2", 1, "a string's and a comment's brackets"},
		{"var f = func() {\n\tx := 1\n\t_ = x\n}\nvar d = 2", 4, "statements inside a function literal do not end the spec"},
		{"\tm10 = 10)", 1, "the enclosing block closes on the spec's own line"},
		{"const s = `never\nclosed", 0, "a raw string that does not end in the window"},
	} {
		end, ok := declEnd(strings.Split(c.src, "\n"), 1)
		if (c.end == 0) == ok || (ok && end != c.end) {
			t.Errorf("%s: declEnd = %d, %v; want %d", c.note, end, ok, c.end)
		}
	}
}
