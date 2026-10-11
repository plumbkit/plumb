package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/topology"
)

// context_review_test.go — the findings of the independent review of A2–A4: a span
// is trusted by the hash read WITH it (S1), a body is never outside the window of its
// line (S2), the documents and memories share the one source-read budget (S4), a
// sensitive caller is not searched on from (S5), and a cut walk says so (NIT 1).

// waitIndexed blocks until the index has parsed exactly content as rel, which is
// what the watcher achieves a moment after an edit.
func waitIndexed(t *testing.T, store *topology.Store, rel, content string) {
	t.Helper()
	want := hashText(content)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if h, ok, err := store.IndexedContentHash(t.Context(), rel); err == nil && ok && h == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s was not reindexed to the new content in time", rel)
}

// Inserted above Total, this makes the OLD span (45–51) land on lines that still
// mention Total, so a span trusted by mistake passes the name check and delivers the
// wrong lines while looking right.
const cartInsertion = "// Count returns how many lines the cart holds.\nfunc (c *Cart) Count() int { return len(c.items) }\n\n// Total returns"

// S1: the index catches up with an edit between the moment a seed's span is read and
// the moment the file is snapshotted. The index's hash now equals the snapshot's, but
// the span is the old one; trusting it would slice the wrong lines and record the
// read. The span is judged by the hash read with it, so it is re-extracted from the
// snapshot instead.
func TestContextForTask_AReindexBetweenResolveAndSnapshotNeverSlicesTheOldSpan(t *testing.T) {
	file := func(s shopTool) string { return filepath.Join(s.root, "cart", "cart.go") }
	args := map[string]any{"symbols": []string{"cart/cart.go#Cart.Total"}}

	t.Run("a reindex after the span was read", func(t *testing.T) {
		s := newShop(t)
		original, err := os.ReadFile(file(s))
		if err != nil {
			t.Fatal(err)
		}
		indexed := s.collect(t, args)
		staleStart, staleEnd := indexed.Seeds[0].Line, indexed.Seeds[0].EndLine
		mutated := strings.Replace(string(original), "// Total returns", cartInsertion, 1)
		if mutated == string(original) {
			t.Fatal("the fixture no longer has the anchor the mutation inserts above")
		}
		s.collector.beforeBodies = func() {
			if err := os.WriteFile(file(s), []byte(mutated), 0o600); err != nil {
				t.Error(err)
				return
			}
			s.store.Enqueue(file(s))
			waitIndexed(t, s.store, "cart/cart.go", mutated)
		}

		out, err := s.run(t, args)
		if err != nil {
			t.Fatal(err)
		}
		wantStart, wantEnd, wantText := declLines(t, file(s), "func (c *Cart) Total()")
		// Control: the old span, applied to the new file, slices lines that mention
		// Total and are not Total. Without this the assertions below could pass for a
		// mutation that moved nothing, or one the name check alone would have caught.
		lines := strings.Split(mutated, "\n")
		stale := strings.Join(lines[staleStart-1:staleEnd], "\n") + "\n"
		if !strings.Contains(stale, "Total") || stale == wantText {
			t.Fatalf("control: the stale span [%d-%d] slices %q, which the name check cannot tell from Total", staleStart, staleEnd, stale)
		}
		// Control: the index really did catch up, so the old comparison would have
		// matched the snapshot.
		if h, ok, err := s.store.IndexedContentHash(t.Context(), "cart/cart.go"); err != nil || !ok || h != fileSHA(t, file(s)) {
			t.Fatalf("control: the index did not reach the new bytes (%q, %v, %v)", h, ok, err)
		}
		bodies := parseBodies(t, out)
		if len(bodies) != 1 {
			t.Fatalf("want one delivered body, got %d:\n%s", len(bodies), out)
		}
		if b := bodies[0]; b.text != wantText || b.start != wantStart || b.end != wantEnd {
			t.Errorf("body = lines %d-%d, want the current Total at lines %d-%d:\n%s", b.start, b.end, wantStart, wantEnd, b.text)
		}
		if !strings.Contains(out, "cart/cart.go changed since it was indexed") {
			t.Errorf("the pack does not say the span it was given is out of date:\n%s", out)
		}
	})

	t.Run("no edit: the span is trusted", func(t *testing.T) {
		s := newShop(t)
		ran := false
		s.collector.beforeBodies = func() { // the hook runs, and the file is untouched
			ran = true
			s.store.Enqueue(file(s))
		}
		out, err := s.run(t, args)
		if err != nil {
			t.Fatal(err)
		}
		_, _, wantText := declLines(t, file(s), "func (c *Cart) Total()")
		if bodies := parseBodies(t, out); !ran || len(bodies) != 1 || bodies[0].text != wantText {
			t.Fatalf("hook ran %v; want the one correct body:\n%s", ran, out)
		}
		if strings.Contains(out, "changed since it was indexed") {
			t.Errorf("an unchanged file was labelled out of date:\n%s", out)
		}
	})
}

// S2: a body belongs to a line in the top window, and a withheld node holds its place
// in that window. Under a tight budget no body is delivered without the line that
// names it.
func TestContextForTask_NoBodyOutlivesTheLineThatNamesIt(t *testing.T) {
	s := newModuleWorkspace(t, hubFiles(20))
	s.collector.WithSensitive(func(_ context.Context, p, _ string) bool { return strings.HasSuffix(filepath.ToSlash(p), "/c03/f.go") })
	pack := s.collect(t, map[string]any{"symbols": []string{"hub/hub.go#Run"}})

	if len(pack.Related) < contextRelatedBodies+2 || !pack.Related[3].Withheld || pack.Related[3].Node.Path != "c03/f.go" {
		t.Fatalf("control: want a withheld node inside the top %d and more nodes behind the window, got %v", contextRelatedBodies, relatedIDs(pack))
	}
	top, attempted := pack.topRelated(), 0
	for i, r := range pack.Related {
		if r.Body.State == bodyNone {
			continue
		}
		attempted++
		if !top[i] || r.Withheld {
			t.Errorf("related[%d] %s has a body attempted outside its line's window (top %v, withheld %v)", i, r.Node.Path, top[i], r.Withheld)
		}
	}
	if attempted != contextRelatedBodies-1 {
		t.Errorf("%d bodies attempted, want the window's %d less the withheld node's", attempted, contextRelatedBodies-1)
	}

	related := 0
	for budget := contextMinMaxBytes; budget <= 16000; budget += 7 {
		p := pack
		p.MaxBytes = budget
		lines := strings.Split(renderContextPack(p), "\n")
		for i, l := range lines {
			if !bodyHeadRe.MatchString(l) {
				continue
			}
			prev := lines[i-1]
			if !strings.HasPrefix(prev, "  [e") && !strings.HasPrefix(prev, "  symbol ") {
				t.Fatalf("max_bytes %d: a body is not under its naming line (it follows %q):\n%s", budget, prev, strings.Join(lines, "\n"))
			}
			if strings.HasPrefix(prev, "  [e") {
				related++
			}
		}
	}
	if related == 0 {
		t.Error("the sweep never delivered a related body, so it proved nothing about related bodies")
	}
}

// S4: the bodies, the memories and the documents are read out of ONE budget of four
// times max_bytes, and each cut is disclosed. Positive control: with room, the same
// workspace delivers the document, the memories and their excerpts, and says nothing
// was cut.
func TestContextForTask_BodiesMemoriesAndDocumentsShareOneSourceReadBudget(t *testing.T) {
	const memBody = "paths: big/*.go\n"
	files := map[string]string{
		"go.mod":      chainModule,
		"docs/big.md": "# Big rules\n\n" + strings.Repeat("Big values must stay bounded in size. ", 12) + "\n",
	}
	for _, n := range []string{"m1", "m2", "m3", "m4", "m5"} {
		files[".plumb/memories/"+n+".md"] = "---\nname: " + n + "\n" + memBody + "---\n\nnote " + n + "\n"
	}
	root := t.TempDir()
	writeFiles(t, root, files)
	memSize := int64(len(files[".plumb/memories/m1.md"]))
	limit := int64(contextSourceReadFactor * contextMinMaxBytes)
	// big.go leaves room for exactly one memory head, and nothing for a second, for
	// the document, or for the excerpt of the memory that was read.
	pad := limit - memSize - 5 - int64(len("package big\n\n// Big is the seed.\nfunc Big() int { return 1 }\n"))
	writeFiles(t, root, map[string]string{"big/big.go": "package big\n\n// Big is the seed.\nfunc Big() int { return 1 }\n" + strings.Repeat("x", int(pad))})
	if info, err := os.Stat(filepath.Join(root, "big", "big.go")); err != nil || info.Size() != limit-memSize-5 {
		t.Fatalf("control: big.go is not sized to leave exactly the room the test needs (%v, %v)", info, err)
	}
	s := newShopTool(t, openContextStore(t, root, 2), root)
	args := map[string]any{"symbols": []string{"big/big.go#Big"}, "max_bytes": contextMinMaxBytes}

	pack := s.collect(t, args)
	if pack.SourceRead > limit {
		t.Errorf("the call read %d B of source, over the %d B budget (%dx max_bytes)", pack.SourceRead, limit, contextSourceReadFactor)
	}
	if want := limit - memSize - 5 + memSize; pack.SourceRead != want {
		t.Errorf("read %d B, want the body file plus one memory head = %d", pack.SourceRead, want)
	}
	for _, want := range []string{
		"document sections: docs/big.md was not read: the source-read budget (4x max_bytes, shared with the bodies and memories) was spent",
		"memories: 4 memory file(s) were not read",
		"memories: 1 matching memory shown without an excerpt: the source-read budget was spent",
	} {
		if !slices.ContainsFunc(pack.Gaps, func(g string) bool { return strings.Contains(g, want) }) {
			t.Errorf("gaps lack %q:\n%q", want, pack.Gaps)
		}
	}
	if len(pack.Constraints) != 1 || pack.Constraints[0].Corpus != corpusMemory || pack.Constraints[0].Excerpt != "" {
		t.Errorf("want only the memory that was read, without an excerpt: %+v", pack.Constraints)
	}

	args["max_bytes"] = contextMaxBytesCap
	roomy := s.collect(t, args)
	var docs, excerpts int
	for _, k := range roomy.Constraints {
		if k.Corpus == corpusDocs && k.Excerpt != "" {
			docs++
		}
		if k.Corpus == corpusMemory && k.Excerpt != "" {
			excerpts++
		}
	}
	if docs != 1 || excerpts != contextMaxMemories {
		t.Errorf("control: with room want 1 document section and %d memory excerpts, got %d and %d: %+v", contextMaxMemories, docs, excerpts, roomy.Constraints)
	}
	for _, g := range roomy.Gaps {
		if strings.Contains(g, "source-read budget") || strings.Contains(g, "were not read") {
			t.Errorf("control: with room the pack still reports a read cut: %q", g)
		}
	}
}

// The memory scan is bounded in files whatever the budget, and the cut is disclosed.
func TestContextForTask_TheMemoryScanIsCappedInFilesAndSaysSo(t *testing.T) {
	files := map[string]string{"go.mod": chainModule, "big/big.go": "package big\n\n// Big is the seed.\nfunc Big() int { return 1 }\n"}
	for i := range contextMemoryScan + 10 {
		files[fmt.Sprintf(".plumb/memories/m%03d.md", i)] = fmt.Sprintf("---\nname: m%03d\npaths: other/*.go\n---\n\nnote\n", i)
	}
	root := t.TempDir()
	writeFiles(t, root, files)
	s := newShopTool(t, openContextStore(t, root, 1), root)
	args := map[string]any{"symbols": []string{"big/big.go#Big"}, "max_bytes": contextMaxBytesCap}
	pack := s.collect(t, args)
	if !slices.ContainsFunc(pack.Gaps, func(g string) bool {
		return strings.HasPrefix(g, fmt.Sprintf("memories: 10 memory file(s) were not read (at most %d are scanned", contextMemoryScan))
	}) {
		t.Errorf("the scan cap is not disclosed:\n%q", pack.Gaps)
	}
	// Control: a directory under the cap loses nothing and says nothing.
	for i := contextMemoryScan - 10; i < contextMemoryScan+10; i++ {
		if err := os.Remove(filepath.Join(root, ".plumb", "memories", fmt.Sprintf("m%03d.md", i))); err != nil {
			t.Fatal(err)
		}
	}
	for _, g := range s.collect(t, args).Gaps {
		if strings.Contains(g, "were not read") {
			t.Errorf("control: a directory under the cap reports a cut: %q", g)
		}
	}
}

// S5: a sensitive caller is named and never searched on from. The importers of its
// package are gap candidates whose Via names the subject, so using the withheld caller
// as a subject would print its selector. Positive control: with nothing sensitive the
// same subject yields that candidate.
func TestContextForTask_ASensitiveCallerIsNotAGapSubject(t *testing.T) {
	args := map[string]any{"symbols": []string{"pricing/discount.go#Apply"}}
	const via = "imports package cart, home of (*Cart).Total"

	open := newShop(t)
	if pack := open.collect(t, args); !slices.ContainsFunc(pack.Related, func(r contextRelated) bool { return r.Gap && r.Via == via }) {
		t.Fatalf("control: without a sensitive decision Total is a subject and Checkout a candidate for it, got %v", relatedIDs(pack))
	}

	s := newShop(t)
	s.collector.WithSensitive(func(_ context.Context, p, _ string) bool {
		return strings.HasSuffix(filepath.ToSlash(p), "/cart/cart.go")
	})
	pack := s.collect(t, args)
	total, ok := pack.related(goldID("cart/cart.go", "(*Cart).Total"))
	if !ok || !total.Withheld || total.CallerOf == 0 {
		t.Fatalf("control: Total must be a withheld caller of Apply, got %+v (found %v)", total, ok)
	}
	for _, r := range pack.Related {
		if strings.Contains(r.Via, "(*Cart).Total") && r.Gap {
			t.Errorf("a gap candidate was found by searching on from a sensitive caller: %+v", r)
		}
	}
	out := renderContextPack(pack)
	if strings.Contains(out, via) {
		t.Errorf("the withheld caller's selector reached a candidate:\n%s", out)
	}
}

// NIT 1: an empty caller list from a walk that was cut reports the cut, not what the
// index holds. Positive control: the same seed, uncut, says none were resolved in the
// index.
func TestContextForTask_ACutWalkDoesNotClaimTheIndexHasNoCallers(t *testing.T) {
	s := newShop(t)
	args := map[string]any{"symbols": []string{"cart/cart.go#Cart.Total"}}
	whole, err := s.run(t, args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(whole, "callers of (*Cart).Total: none resolved in the index") {
		t.Fatalf("control: the uncut walk should speak of the index:\n%s", whole)
	}
	s.collector.deadline = time.Nanosecond
	cut, err := s.run(t, args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cut, "callers of (*Cart).Total: none reached before the walk was cut") || strings.Contains(cut, "none resolved in the index") {
		t.Errorf("a cut walk must say it was cut, and not describe the index:\n%s", cut)
	}
}

// A truncated importer list is a gap, in words of its own.
func TestExpansionStats_AnImporterListThatWasCutIsDisclosed(t *testing.T) {
	var none, cut expansionStats
	cut.ImportersCapped = 2
	if got := none.gaps(); len(got) != 0 {
		t.Fatalf("control: empty stats disclose %q", got)
	}
	want := fmt.Sprintf("2 importer list(s) exceeded %d and were cut; gap candidates may be missing", contextHopNodes)
	if got := cut.gaps(); !slices.Equal(got, []string{want}) {
		t.Errorf("gaps = %q, want %q", got, want)
	}
}
