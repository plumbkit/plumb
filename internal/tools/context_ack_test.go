package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// context_ack_test.go — have-acknowledgements (PLAN-462 A4, C15): a body the
// caller still holds is acknowledged in place of the body, keyed by the exact hash
// of the body the pack would deliver, and an acknowledgement is never a read.

const heldMarker = "    unchanged, still held:"

func heldLines(out string) []string {
	var held []string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, heldMarker) {
			held = append(held, l)
		}
	}
	return held
}

func haveArg(symbol, sha string) map[string]string {
	return map[string]string{"symbol": symbol, "content_sha256": sha}
}

// totalBody delivers the (*Cart).Total body once and returns what was delivered.
func totalBody(t *testing.T, s shopTool) deliveredBody {
	t.Helper()
	out, err := s.run(t, oracle(t, "C15").args())
	if err != nil {
		t.Fatal(err)
	}
	bodies := parseBodies(t, out)
	if len(bodies) != 1 {
		t.Fatalf("want the one seed body, got %d:\n%s", len(bodies), out)
	}
	return bodies[0]
}

// C15, both phases. Unchanged: the body is acknowledged with its current location,
// no edit guard is printed, no read is recorded, and a strict edit is still
// refused. After Total is edited on disk the same acknowledgement no longer
// matches and the body returns with a new content_sha256.
func TestContextForTask_Have_C15_AnUnchangedBodyIsAcknowledgedNotResentAndNotARead(t *testing.T) {
	s := newShop(t)
	c := oracle(t, "C15")
	file := filepath.Join(s.root, "cart", "cart.go")
	held := totalBody(t, s)

	trackers := newAgentTrackers("a")
	s.tool.WithReadsFor(trackers.resolve)
	args := c.args()
	args["have"] = []map[string]string{haveArg("cart/cart.go::(*Cart).Total", held.contentSHA)}
	out, err := s.runAs(t, asAgent("a"), args)
	if err != nil {
		t.Fatal(err)
	}
	if got := parseBodies(t, out); len(got) != 0 {
		t.Fatalf("an acknowledged body was sent again: %+v", got)
	}
	lines := heldLines(out)
	wantPrefix := fmt.Sprintf("%s lines %d–%d (%d B) content_sha256=%s; body not re-sent. An acknowledgement is not a read and carries no edit guard: read_symbol ",
		heldMarker, held.start, held.end, held.size, held.contentSHA)
	if len(lines) != 1 || !strings.HasPrefix(lines[0], wantPrefix) {
		t.Fatalf("want one acknowledgement line starting %q, got %q", wantPrefix, lines)
	}
	if strings.Contains(out, "guard for edit_file on cart/cart.go") {
		t.Errorf("an acknowledgement printed an edit guard:\n%s", out)
	}
	if !trackers["a"].Mtime(file).IsZero() {
		t.Error("forbidden: an acknowledgement created a read record")
	}
	_, err = strictEdit(t, asAgent("a"), trackers, file, nil)
	requireErr(t, err, "has not been read")
	if strings.Contains(out, "have:") {
		t.Errorf("a matched acknowledgement was reported as unmatched:\n%s", out)
	}

	// Control: the same call without have delivers the body and records the read, so
	// the refusal above was the acknowledgement's, not a broken edit.
	plain, err := s.runAs(t, asAgent("a"), c.args())
	if err != nil {
		t.Fatal(err)
	}
	if got := parseBodies(t, plain); len(got) != 1 || got[0].contentSHA != held.contentSHA {
		t.Fatalf("control: the plain call did not deliver the body:\n%s", plain)
	}
	if _, err := strictEdit(t, asAgent("a"), trackers, file, nil); err != nil {
		t.Fatalf("control: the delivered body did not satisfy the strict edit: %v", err)
	}

	// Phase 2: Total changes on disk; the old hash no longer describes it.
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(data), "\tsub := 0\n", "\tsub := 0 // starts at zero\n", 1)
	if edited == string(data) {
		t.Fatal("control: the edit changed nothing")
	}
	if err := os.WriteFile(file, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := s.run(t, args)
	if err != nil {
		t.Fatal(err)
	}
	bodies := parseBodies(t, after)
	if len(bodies) != 1 || bodies[0].contentSHA == held.contentSHA || len(heldLines(after)) != 0 {
		t.Fatalf("after the edit want the body re-sent under a new content_sha256, got %+v and held %q:\n%s", bodies, heldLines(after), after)
	}
	_, _, wantText := declLines(t, file, "func (c *Cart) Total()")
	if bodies[0].contentSHA != hashText(wantText) {
		t.Errorf("the re-sent body's hash is %s, want the hash of the edited declaration %s", bodies[0].contentSHA, hashText(wantText))
	}
	if !strings.Contains(after, "have: 1 entry matched no body in this pack") {
		t.Errorf("the stale acknowledgement is not reported:\n%s", after)
	}
}

// The key is the exact body hash and the declaration it names, and nothing else. A
// file's hash, the hash of a line range of the body, a hash borrowed for another
// declaration, and a path alone each leave the body to be delivered; the spellings
// of the right pair all acknowledge it.
func TestContextForTask_Have_IsKeyedByTheExactBodyHashAndTheDeclaration(t *testing.T) {
	s := newShop(t)
	file := filepath.Join(s.root, "cart", "cart.go")
	body := totalBody(t, s)
	lines := strings.SplitAfter(body.text, "\n")
	rangeHash := hashText(strings.Join(lines[:3], ""))
	args := func(h map[string]string) map[string]any {
		a := oracle(t, "C15").args()
		a["have"] = []map[string]string{h}
		return a
	}
	deliveredFor := func(t *testing.T, h map[string]string) {
		t.Helper()
		out, err := s.run(t, args(h))
		if err != nil {
			t.Fatal(err)
		}
		if got := parseBodies(t, out); len(got) != 1 || got[0].contentSHA != body.contentSHA || len(heldLines(out)) != 0 {
			t.Errorf("%v: want the body delivered and no acknowledgement, got bodies %+v held %q", h, got, heldLines(out))
		}
		if !strings.Contains(out, "have: 1 entry matched no body in this pack") {
			t.Errorf("%v: the unmatched entry is not reported:\n%s", h, out)
		}
	}
	for name, h := range map[string]map[string]string{
		"the file's hash":                          haveArg("cart/cart.go#Cart.Total", fileSHA(t, file)),
		"the hash of three lines of the body":      haveArg("cart/cart.go#Cart.Total", rangeHash),
		"the right hash under another symbol":      haveArg("cart/cart.go#Cart.Add", body.contentSHA),
		"the right hash under another path":        haveArg("store/ledger.go#Ledger.Total", body.contentSHA),
		"the right hash and selector, other path":  haveArg("store/ledger.go#Cart.Total", body.contentSHA),
		"the right hash and bare name, other path": haveArg("store/ledger.go#Total", body.contentSHA),
		"the right symbol under a wrong hash":      haveArg("cart/cart.go#Cart.Total", strings.Repeat("0", 64)),
		"the hash of other bytes":                  haveArg("cart/cart.go#Cart.Total", hashText("\x00")),
	} {
		t.Run("not "+name, func(t *testing.T) { deliveredFor(t, h) })
	}
	for _, symbol := range []string{
		"cart/cart.go#Cart.Total", "cart/cart.go#(*Cart).Total", "cart/cart.go#*Cart.Total",
		"cart/cart.go::(*Cart).Total", "Total", "Cart.Total", "./cart/cart.go#Cart.Total", file + "#(*Cart).Total",
	} {
		t.Run("acknowledged as "+symbol, func(t *testing.T) {
			out, err := s.run(t, args(haveArg(symbol, strings.ToUpper(body.contentSHA)))) // the hash is case-insensitive
			if err != nil {
				t.Fatal(err)
			}
			if len(parseBodies(t, out)) != 0 || len(heldLines(out)) != 1 {
				t.Errorf("want the body acknowledged, got held %q:\n%s", heldLines(out), out)
			}
		})
	}
}

var headerSHA = regexp.MustCompile(`sha256=([0-9a-f]{64})`)

// The ranged-read counterexample. A caller that read only part of the file with
// read_file (whose header carries the WHOLE file's hash) is recorded as having read
// the file, yet holds neither the file nor the whole body: offering that hash, or
// the hash of the lines it did read, does not suppress the body.
func TestContextForTask_Have_ARangedReadDoesNotMakeACallerTheHolderOfTheWholeBody(t *testing.T) {
	s := newShop(t)
	file := filepath.Join(canonicalRoot(s.root), "cart", "cart.go")
	body := totalBody(t, s)
	tracker := NewReadTracker()
	s.tool.WithReads(tracker)

	rf, err := NewReadFile(tracker).Execute(t.Context(), mustJSON(map[string]any{"file_path": file, "start_line": body.start, "end_line": body.start + 2}))
	if err != nil {
		t.Fatal(err)
	}
	m := headerSHA.FindStringSubmatch(rf)
	if m == nil || m[1] != fileSHA(t, file) {
		t.Fatalf("control: the ranged read's header does not carry the whole file's hash:\n%.200s", rf)
	}
	if tracker.Mtime(file).IsZero() {
		t.Fatal("control: the ranged read left no record, so the test would not show the tracker being ignored")
	}
	partial := strings.Join(strings.SplitAfter(body.text, "\n")[:3], "")
	for name, sha := range map[string]string{"the file hash its header reports": m[1], "the hash of the lines it read": hashText(partial)} {
		a := oracle(t, "C15").args()
		a["have"] = []map[string]string{haveArg("cart/cart.go#Cart.Total", sha)}
		out, err := s.run(t, a)
		if err != nil {
			t.Fatal(err)
		}
		if got := parseBodies(t, out); len(got) != 1 || got[0].contentSHA != body.contentSHA || len(heldLines(out)) != 0 {
			t.Errorf("%s: a ranged read made the caller the holder of the whole body (bodies %+v, held %q)", name, got, heldLines(out))
		}
	}
	// Without any have, the record the ranged read left changes nothing either: the
	// pack does not consult the tracker to decide what a caller holds.
	if out, err := s.run(t, oracle(t, "C15").args()); err != nil || len(parseBodies(t, out)) != 1 {
		t.Errorf("a ranged read suppressed a body with no acknowledgement (err %v):\n%s", err, out)
	}
}

// At most sixteen acknowledgements are honoured; the rest are ignored, said so, and
// their bodies are sent. Positive control: the sixteenth is honoured.
func TestContextForTask_Have_HonoursSixteenAndSaysSoForTheRest(t *testing.T) {
	if contextMaxHave != 16 { // gate v1 work_caps.have_acks
		t.Fatalf("contextMaxHave = %d, but the frozen gate caps acknowledgements at 16", contextMaxHave)
	}
	s := newShop(t)
	body := totalBody(t, s)
	with := func(position int) string {
		have := make([]map[string]string, 0, position+1)
		for i := range position {
			have = append(have, haveArg(fmt.Sprintf("cart/cart.go#Cart.Add%d", i), strings.Repeat("a", 64)))
		}
		have = append(have, haveArg("cart/cart.go#Cart.Total", body.contentSHA))
		a := oracle(t, "C15").args()
		a["have"] = have
		out, err := s.run(t, a)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if out := with(contextMaxHave - 1); len(parseBodies(t, out)) != 0 || len(heldLines(out)) != 1 {
		t.Errorf("control: the sixteenth acknowledgement was not honoured:\n%s", out)
	}
	out := with(contextMaxHave)
	if len(parseBodies(t, out)) != 1 || len(heldLines(out)) != 0 {
		t.Errorf("the seventeenth acknowledgement was honoured:\n%s", out)
	}
	if !strings.Contains(out, "have: 1 entry beyond the 16-entry cap was ignored") {
		t.Errorf("the ignored entry is not disclosed:\n%s", out)
	}
}

// A related declaration's body is held the same way as a seed's.
func TestContextForTask_Have_AcknowledgesARelatedBodyToo(t *testing.T) {
	s := newShop(t)
	c := oracle(t, "C01")
	out, err := s.run(t, c.args())
	if err != nil {
		t.Fatal(err)
	}
	var apply deliveredBody
	for _, b := range parseRelatedBodies(t, out) {
		if strings.HasPrefix(b.text, "func Apply(") {
			apply = b
		}
	}
	if apply.contentSHA == "" {
		t.Fatalf("control: Apply's body was not delivered as a related body:\n%s", out)
	}
	a := c.args()
	a["have"] = []map[string]string{haveArg("pricing/discount.go#Apply", apply.contentSHA)}
	held, err := s.run(t, a)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range parseRelatedBodies(t, held) {
		if b.contentSHA == apply.contentSHA {
			t.Errorf("Apply's body was sent again:\n%s", held)
		}
	}
	if lines := heldLines(held); len(lines) != 1 || !strings.Contains(lines[0], apply.contentSHA) {
		t.Errorf("want one acknowledgement for Apply, got %q", lines)
	}
}

// When an acknowledged body is the only one of its file, nothing is recorded; when
// another body of the same file IS delivered, the file is recorded once for that
// body, as always, and the acknowledged one does not change it.
func TestContextForTask_Have_RecordsTheFileOnlyForADeliveredBody(t *testing.T) {
	s := newShop(t)
	file := filepath.Join(s.root, "cart", "cart.go")
	total := totalBody(t, s)
	trackers := newAgentTrackers("a")
	s.tool.WithReadsFor(trackers.resolve)
	a := map[string]any{
		"symbols": []string{"cart/cart.go#Cart.Total", "cart/cart.go#Cart.Add"},
		"have":    []map[string]string{haveArg("cart/cart.go#Cart.Total", total.contentSHA)},
	}
	out, err := s.runAs(t, asAgent("a"), a)
	if err != nil {
		t.Fatal(err)
	}
	if got := parseBodies(t, out); len(got) != 1 || !strings.HasPrefix(got[0].text, "func (c *Cart) Add(") || got[0].guard == nil {
		t.Fatalf("want only Add delivered, with the file's guard:\n%s", out)
	}
	if trackers["a"].Mtime(file).IsZero() {
		t.Error("the delivered body of the file was not recorded")
	}
	if len(heldLines(out)) != 1 {
		t.Errorf("Total was not acknowledged:\n%s", out)
	}
}
