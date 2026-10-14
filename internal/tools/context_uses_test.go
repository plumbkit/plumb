package tools

import (
	"os"
	"path/filepath"
	"regexp"
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
`,
	"store/other.go": "package store\n\nconst elsewhere = 5\n",
	"store/many.go": "package store\n\nvar _ = m1\n\nconst (\n\tm1 = 1\n\tm2 = 2\n\tm3 = 3\n\tm4 = 4\n\tm5 = 5\n\tm6 = 6\n\tm7 = 7\n\tm8 = 8\n\tm9 = 9\n\tm10 = 10\n)\n\n" +
		"func Many() int { _ = m1; return m1 + m2 + m3 + m4 + m5 + m6 + m7 + m8 + m9 + m10 }\n",
	"store/caller.go": "package store\n\nfunc Caller() string { return List(T{}, 1) }\n",
	"py/mod.py":       "LIMIT = 5\n\n\ndef cap(n):\n    return min(n, LIMIT)\n",
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
	return newShopTool(t, openContextStore(t, root, 5), root)
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
}

func TestContextForTask_UsesIsGoOnly(t *testing.T) {
	s := newUsesTool(t)
	pack := s.collect(t, map[string]any{"symbols": []string{"py/mod.py#cap", "store/store.go#List"}, "max_bytes": 12000})
	var py, gobody *contextBody
	for i, sd := range pack.Seeds {
		switch sd.Name {
		case "cap":
			py = &pack.Bodies[i]
		case "List":
			gobody = &pack.Bodies[i]
		}
	}
	if py == nil || gobody == nil || py.State != bodyReady || !strings.Contains(py.Text, "LIMIT") {
		t.Fatalf("want both bodies delivered, the Python one naming LIMIT: %+v / %+v", py, gobody)
	}
	if len(py.Uses) != 0 {
		t.Errorf("a Python body gets no uses, got %+v", py.Uses)
	}
	if len(gobody.Uses) == 0 {
		t.Fatal("positive control: the Go body in the same pack has uses")
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
}

// Under a tight budget the record degrades to names and lines, and when even that
// does not fit it is counted in the omission footer as "uses".
func TestContextForTask_UsesDegradesThenIsCounted(t *testing.T) {
	s := newUsesTool(t)
	var lean, counted bool
	for mb := contextMinMaxBytes; mb <= 6000 && (!lean || !counted); mb += 25 {
		out, err := s.run(t, map[string]any{"symbols": []string{"store/store.go#List"}, "max_bytes": mb})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "\n10\tWHERE n < ?`") && strings.Contains(out, "    uses, declared in this file: maxRows (constant, line 5)") {
			lean = true
		}
		if regexp.MustCompile(`omitted: \d+ item\(s\) \([^)]*\b1 uses\b`).MatchString(out) {
			counted = true
		}
	}
	if !lean || !counted {
		t.Fatalf("no budget showed the names-only record (%v) or counted a dropped one (%v)", lean, counted)
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
		d, ok := (&bodySource{lines: lines}).declLines(topology.Node{Name: "x", Kind: topology.KindVariable, StartLine: 1})
		if !ok || d.Cut != c.cut || (!c.cut && d.End != c.n) || (c.cut && d.Text != "var x = []int{\n") {
			t.Errorf("%d lines: got %+v, %v; want cut=%v", c.n, d, ok, c.cut)
		}
	}
}

func TestScanOpen(t *testing.T) {
	for _, c := range []struct {
		line      string
		depth     int
		raw       bool
		wantDepth int
		wantRaw   bool
	}{
		{"x = map[string]int{", 0, false, 1, false},
		{`x = "({["`, 0, false, 0, false},
		{`x = '('`, 0, false, 0, false},
		{`x = "a\"(" + f(`, 0, false, 1, false},
		{"x = 1 // (", 0, false, 0, false},
		{"x = `SELECT (", 0, false, 0, true},
		{"FROM t)`", 0, true, 0, false},
		{"FROM t`)", 1, true, 0, false},
		{"}", 1, false, 0, false},
	} {
		d, r := scanOpen(c.line, c.depth, c.raw)
		if d != c.wantDepth || r != c.wantRaw {
			t.Errorf("scanOpen(%q, %d, %v) = %d, %v; want %d, %v", c.line, c.depth, c.raw, d, r, c.wantDepth, c.wantRaw)
		}
	}
}
