package tools

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

func TestOffsetForPosition(t *testing.T) {
	data := []byte("hello\nworld\n")
	cases := []struct {
		line, char uint32
		want       int
		ok         bool
	}{
		{0, 0, 0, true},
		{0, 5, 5, true},
		{1, 0, 6, true},
		{1, 5, 11, true},
		{2, 0, 12, true},
		{99, 0, 0, false},
	}
	for _, c := range cases {
		got, ok := offsetForPosition(data, protocol.Position{Line: c.line, Character: c.char})
		if ok != c.ok {
			t.Errorf("offsetForPosition(%d,%d) ok=%v, want %v", c.line, c.char, ok, c.ok)
		}
		if c.ok && got != c.want {
			t.Errorf("offsetForPosition(%d,%d) = %d, want %d", c.line, c.char, got, c.want)
		}
	}
}

func TestApplyTextEditsToFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.txt")
	if err := os.WriteFile(path, []byte("hello world\nfoo bar\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Two edits: replace "world" with "Go" and "foo" with "FOO".
	edits := []protocol.TextEdit{
		{
			Range:   protocol.Range{Start: protocol.Position{Line: 0, Character: 6}, End: protocol.Position{Line: 0, Character: 11}},
			NewText: "Go",
		},
		{
			Range:   protocol.Range{Start: protocol.Position{Line: 1, Character: 0}, End: protocol.Position{Line: 1, Character: 3}},
			NewText: "FOO",
		},
	}
	if err := applyTextEditsToFile(path, edits); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "hello Go\nFOO bar\n" {
		t.Errorf("applyTextEditsToFile result wrong:\ngot  %q\nwant %q", got, "hello Go\nFOO bar\n")
	}
}

func TestApplyWorkspaceEdit_MultipleFiles(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(a, []byte("aaa\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("bbb\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	we := &protocol.WorkspaceEdit{
		Changes: map[string][]protocol.TextEdit{
			"file://" + a: {{Range: protocol.Range{Start: protocol.Position{Line: 0, Character: 0}, End: protocol.Position{Line: 0, Character: 3}}, NewText: "AAA"}},
			"file://" + b: {{Range: protocol.Range{Start: protocol.Position{Line: 0, Character: 0}, End: protocol.Position{Line: 0, Character: 3}}, NewText: "BBB"}},
		},
	}
	mod, err := applyWorkspaceEdit(we)
	if err != nil {
		t.Fatal(err)
	}
	if len(mod) != 2 {
		t.Errorf("expected 2 modified files, got %d", len(mod))
	}
	if got, _ := os.ReadFile(a); string(got) != "AAA\n" {
		t.Errorf("a.txt: %q", got)
	}
	if got, _ := os.ReadFile(b); string(got) != "BBB\n" {
		t.Errorf("b.txt: %q", got)
	}
}

// Several valid files sort BEFORE the one file whose edit cannot be applied, and
// preparation runs in sorted path order, so an implementation that wrote each
// file as it validated would have committed every valid file by the time it
// reached the broken one. The assertion therefore fails deterministically rather
// than depending on Go's map-iteration order to put the invalid file last.
func TestApplyWorkspaceEdit_ValidatesAllFilesBeforeWriting(t *testing.T) {
	dir := t.TempDir()
	line0 := protocol.Range{Start: protocol.Position{Line: 0, Character: 0}, End: protocol.Position{Line: 0, Character: 3}}
	pastEOF := protocol.Range{Start: protocol.Position{Line: 99, Character: 0}, End: protocol.Position{Line: 99, Character: 3}}

	valid := []string{"a.txt", "b.txt", "c.txt", "d.txt"}
	changes := map[string][]protocol.TextEdit{}
	for _, name := range valid {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("aaa\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		changes["file://"+p] = []protocol.TextEdit{{Range: line0, NewText: "AAA"}}
	}
	// Sorts last, so every valid file is prepared before this one fails.
	broken := filepath.Join(dir, "z.txt")
	if err := os.WriteFile(broken, []byte("zzz\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changes["file://"+broken] = []protocol.TextEdit{{Range: pastEOF, NewText: "ZZZ"}}

	if _, err := applyWorkspaceEdit(&protocol.WorkspaceEdit{Changes: changes}); err == nil {
		t.Fatal("expected the out-of-range edit to fail the whole apply")
	}
	for _, name := range valid {
		if got, _ := os.ReadFile(filepath.Join(dir, name)); string(got) != "aaa\n" {
			t.Fatalf("%s was written before all files validated: %q", name, got)
		}
	}
	if got, _ := os.ReadFile(broken); string(got) != "zzz\n" {
		t.Fatalf("z.txt changed unexpectedly: %q", got)
	}
}

// workspaceEditTargets must hand preparation a stable, sorted order whatever the
// WorkspaceEdit's map iteration does.
func TestWorkspaceEditTargets_SortedByPath(t *testing.T) {
	we := &protocol.WorkspaceEdit{Changes: map[string][]protocol.TextEdit{
		"file:///z.txt": {{NewText: "z"}},
		"file:///a.txt": {{NewText: "a"}},
		"file:///m.txt": {{NewText: "m"}},
	}}
	want := []string{"/a.txt", "/m.txt", "/z.txt"}
	for range 20 { // map order varies per iteration; the result must not
		got, err := workspaceEditTargets(we)
		if err != nil {
			t.Fatalf("three distinct files must not be refused: %v", err)
		}
		for i, w := range want {
			if got[i].path != w {
				t.Fatalf("targets[%d].path = %q, want %q", i, got[i].path, w)
			}
		}
	}
}

// symlinkedSpellings returns two spellings of ONE file — one through a real
// directory, one through a symlink to it — plus the real path and the bytes it
// was seeded with. Skips when the platform will not make a symlink.
func symlinkedSpellings(t *testing.T, content string) (viaReal, viaLink, realPath string) {
	t.Helper()
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}
	realPath = filepath.Join(realDir, "f.txt")
	if err := os.WriteFile(realPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return realPath, filepath.Join(dir, "link", "f.txt"), realPath
}

// Two URIs naming ONE file are refused, not applied (issue #314). Before the
// fix both spellings became their own target, both were prepared from the same
// pre-edit bytes, and both were written in turn — so the second write silently
// discarded the first, inside an apply whose contract is that it is atomic.
// lockPaths collapses the pair to one mutex (issue #290), so the bug never
// deadlocked and never errored: it just lost an edit and reported success.
func TestApplyWorkspaceEdit_RefusesTwoSpellingsOfOneFile(t *testing.T) {
	const original = "alpha\nbeta\n"
	viaReal, viaLink, realPath := symlinkedSpellings(t, original)

	// Two DIFFERENT edits, so a lost update is visible in the bytes: line 0 via
	// the real path, line 1 via the symlink.
	we := &protocol.WorkspaceEdit{Changes: map[string][]protocol.TextEdit{
		"file://" + viaReal: {{
			Range:   protocol.Range{Start: protocol.Position{Line: 0, Character: 0}, End: protocol.Position{Line: 0, Character: 5}},
			NewText: "ALPHA",
		}},
		"file://" + viaLink: {{
			Range:   protocol.Range{Start: protocol.Position{Line: 1, Character: 0}, End: protocol.Position{Line: 1, Character: 4}},
			NewText: "BETA",
		}},
	}}

	modified, err := applyWorkspaceEdit(we)
	if err == nil {
		t.Fatalf("expected a refusal; the apply reported success, modified=%v", modified)
	}
	if len(modified) != 0 {
		t.Errorf("a refused apply must report no modified files, got %v", modified)
	}
	if !isEditLogicError(err) {
		t.Errorf("refusal is not marked editLogicErr, so callers will retry it: %v", err)
	}
	if !strings.Contains(err.Error(), "same file under two paths") {
		t.Errorf("error does not name the defect: %v", err)
	}
	// The point of refusing: no byte lands, rather than one edit landing and the
	// other being silently dropped.
	got, readErr := os.ReadFile(realPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != original {
		t.Fatalf("file was written despite the refusal: %q, want %q", got, original)
	}
}

// The refusal names the same pair of spellings on every run. Iterating the URI
// map directly would let Go's randomised map order pick a different "first"
// spelling each time, so the same broken WorkspaceEdit would report a different
// error message run to run and look unreproducible.
func TestWorkspaceEditTargets_DuplicateErrorIsDeterministic(t *testing.T) {
	viaReal, viaLink, _ := symlinkedSpellings(t, "alpha\n")
	we := &protocol.WorkspaceEdit{Changes: map[string][]protocol.TextEdit{
		"file://" + viaReal: {{NewText: "a"}},
		"file://" + viaLink: {{NewText: "b"}},
	}}

	var first string
	for i := range 20 {
		targets, err := workspaceEditTargets(we)
		if err == nil {
			t.Fatalf("two spellings of one file were not refused: %v", targets)
		}
		if i == 0 {
			first = err.Error()
			continue
		}
		if err.Error() != first {
			t.Fatalf("error text varies with map order:\n first: %s\n now:   %s", first, err.Error())
		}
	}
}

// The SAME spelling in both Changes and DocumentChanges used to be MERGED into
// one edit list, so a server emitting its edits under both forms for capability
// compatibility had each edit applied twice. That is not idempotent:
// applyTextEdits threads the buffer through its loop, so the second application
// of a length-changing edit lands on already-rewritten bytes. Replacing "foo"
// with "X" once gives "X bar"; twice gives "Xar" — silent corruption reported
// as success. This is the second half of issue #314, and the reason merging the
// lists was the wrong fix.
func TestApplyWorkspaceEdit_RefusesOneSpellingInBothForms(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	const original = "foo bar\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	uri := "file://" + path
	edit := protocol.TextEdit{
		Range:   protocol.Range{Start: protocol.Position{Line: 0, Character: 0}, End: protocol.Position{Line: 0, Character: 3}},
		NewText: "X",
	}
	we := &protocol.WorkspaceEdit{
		Changes: map[string][]protocol.TextEdit{uri: {edit}},
		DocumentChanges: []protocol.TextDocumentEdit{{
			TextDocument: protocol.VersionedTextDocumentIdentifier{URI: uri},
			Edits:        []protocol.TextEdit{edit},
		}},
	}

	modified, err := applyWorkspaceEdit(we)
	if err == nil {
		got, _ := os.ReadFile(path)
		t.Fatalf("one file carrying edits in both forms was applied, not refused: modified=%v, content=%q", modified, got)
	}
	if !strings.Contains(err.Error(), "twice") {
		t.Errorf("error does not say the file was named twice: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != original {
		t.Fatalf("file was written despite the refusal: %q, want %q", got, original)
	}
}

// A bare DocumentChanges entry carrying NO edits alongside a real Changes entry
// is the compatibility shape servers actually emit, and it cannot lose or
// duplicate anything — so it must still MERGE rather than trip the guard.
// TestRenameSymbol_DeduplicatesTargetsAcrossEditForms depends on this; without
// the carve-out the guard would break a working case to fix a broken one.
func TestWorkspaceEditTargets_BareMentionStillMerges(t *testing.T) {
	uri := "file:///only.txt"
	edit := protocol.TextEdit{NewText: "a"}
	for _, tc := range []struct {
		name string
		we   *protocol.WorkspaceEdit
	}{
		{"bare entry second", &protocol.WorkspaceEdit{
			Changes:         map[string][]protocol.TextEdit{uri: {edit}},
			DocumentChanges: []protocol.TextDocumentEdit{{TextDocument: protocol.VersionedTextDocumentIdentifier{URI: uri}}},
		}},
		{"bare entry first", &protocol.WorkspaceEdit{
			Changes: map[string][]protocol.TextEdit{uri: {}},
			DocumentChanges: []protocol.TextDocumentEdit{{
				TextDocument: protocol.VersionedTextDocumentIdentifier{URI: uri},
				Edits:        []protocol.TextEdit{edit},
			}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			targets, err := workspaceEditTargets(tc.we)
			if err != nil {
				t.Fatalf("a bare mention must merge, not be refused: %v", err)
			}
			if len(targets) != 1 {
				t.Fatalf("got %d targets, want 1", len(targets))
			}
			// The real edit must survive the merge in either order.
			if len(targets[0].edits) != 1 {
				t.Fatalf("got %d edits, want the one real edit", len(targets[0].edits))
			}
		})
	}
}

// Targets must be sorted by path even when the edit set uses BOTH forms.
// workspaceEditEntries sorts the Changes map and then appends DocumentChanges
// in server order, so the final sort.Slice is what actually orders the result
// here — on every platform, not only where URIToPath rewrites separators.
// TestWorkspaceEditTargets_SortedByPath uses Changes alone and is satisfied by
// the URI sort, so without this case the final sort is unpinned.
func TestWorkspaceEditTargets_SortedAcrossBothForms(t *testing.T) {
	we := &protocol.WorkspaceEdit{
		Changes: map[string][]protocol.TextEdit{
			"file:///z.txt": {{NewText: "z"}},
		},
		DocumentChanges: []protocol.TextDocumentEdit{
			{TextDocument: protocol.VersionedTextDocumentIdentifier{URI: "file:///a.txt"}, Edits: []protocol.TextEdit{{NewText: "a"}}},
			{TextDocument: protocol.VersionedTextDocumentIdentifier{URI: "file:///m.txt"}, Edits: []protocol.TextEdit{{NewText: "m"}}},
		},
	}
	targets, err := workspaceEditTargets(we)
	if err != nil {
		t.Fatalf("three distinct files must not be refused: %v", err)
	}
	for i, want := range []string{"/a.txt", "/m.txt", "/z.txt"} {
		if targets[i].path != want {
			t.Fatalf("targets[%d].path = %q, want %q — DocumentChanges arrive in server order, so the final sort is load-bearing", i, targets[i].path, want)
		}
	}
}

// workspaceEditTargets must not alias the caller's WorkspaceEdit: applyTextEdits
// sorts its argument in place, so handing it the server's own slice would
// reorder a structure the caller still owns. The merge this replaced always
// allocated, so the contract was previously satisfied by accident.
func TestWorkspaceEditTargets_DoesNotAliasTheCallersEdits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("1 bbb 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	edits := []protocol.TextEdit{
		{Range: protocol.Range{Start: protocol.Position{Line: 0, Character: 0}, End: protocol.Position{Line: 0, Character: 1}}, NewText: "A"},
		{Range: protocol.Range{Start: protocol.Position{Line: 0, Character: 6}, End: protocol.Position{Line: 0, Character: 7}}, NewText: "C"},
	}
	we := &protocol.WorkspaceEdit{Changes: map[string][]protocol.TextEdit{"file://" + path: edits}}
	before := slices.Clone(edits)

	if _, err := applyWorkspaceEdit(we); err != nil {
		t.Fatalf("applyWorkspaceEdit: %v", err)
	}
	if !slices.Equal(before, we.Changes["file://"+path]) {
		t.Errorf("the caller's edit slice was reordered in place:\n before %v\n after  %v", before, we.Changes["file://"+path])
	}
}

// The bare-mention carve-out applies only to the SAME spelling. Two spellings
// of one file are the defect this guard exists for; a bare second mention does
// not make the pair benign, it only makes it harmless today — and admitting it
// would leave one target carrying one spelling while collectRenameTargets
// counts both, breaking the invariant that file list and plans agree.
func TestWorkspaceEditTargets_TwoSpellingsRefusedEvenWhenOneIsBare(t *testing.T) {
	viaReal, viaLink, _ := symlinkedSpellings(t, "alpha\n")
	for _, tc := range []struct {
		name       string
		bare, real string
	}{
		{"bare spelling first", viaReal, viaLink},
		{"bare spelling second", viaLink, viaReal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			we := &protocol.WorkspaceEdit{
				Changes: map[string][]protocol.TextEdit{"file://" + tc.bare: {}},
				DocumentChanges: []protocol.TextDocumentEdit{{
					TextDocument: protocol.VersionedTextDocumentIdentifier{URI: "file://" + tc.real},
					Edits:        []protocol.TextEdit{{NewText: "x"}},
				}},
			}
			if targets, err := workspaceEditTargets(we); err == nil {
				t.Fatalf("two spellings were admitted because one was bare: %v", targets)
			}
		})
	}
}

// The duplicate scan must compare against EVERY earlier entry, not just the
// previous one. A pair that is not adjacent in sorted order (here link/f.txt,
// link/g.txt, real/f.txt) slips past an adjacent-only check while every other
// test in this file still passes, because they all place the pair side by side.
func TestWorkspaceEditTargets_RefusesNonAdjacentDuplicate(t *testing.T) {
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}
	we := &protocol.WorkspaceEdit{Changes: map[string][]protocol.TextEdit{
		"file://" + filepath.Join(dir, "link", "f.txt"): {{NewText: "a"}},
		"file://" + filepath.Join(dir, "link", "g.txt"): {{NewText: "b"}},
		"file://" + filepath.Join(realDir, "f.txt"):     {{NewText: "c"}},
	}}
	if targets, err := workspaceEditTargets(we); err == nil {
		t.Fatalf("a duplicate pair separated by another file was not refused: %v", targets)
	}
}

// The refusal must fire on the documentChanges form too, and across the two
// forms: workspaceEditEntries flattens both into one entry list, so a server that sends
// one spelling in changes and the other in documentChanges produces exactly the
// pair of targets this guard exists to catch.
func TestWorkspaceEditTargets_RefusesAcrossChangesAndDocumentChanges(t *testing.T) {
	viaReal, viaLink, _ := symlinkedSpellings(t, "alpha\n")
	we := &protocol.WorkspaceEdit{
		Changes: map[string][]protocol.TextEdit{
			"file://" + viaReal: {{NewText: "a"}},
		},
		DocumentChanges: []protocol.TextDocumentEdit{{
			TextDocument: protocol.VersionedTextDocumentIdentifier{URI: "file://" + viaLink},
			Edits:        []protocol.TextEdit{{NewText: "b"}},
		}},
	}
	if targets, err := workspaceEditTargets(we); err == nil {
		t.Fatalf("two spellings split across changes/documentChanges were not refused: %v", targets)
	}
}

// The guard must not fire on the ordinary case it sits in front of: distinct
// files reached through a symlinked parent are still one target each. Without
// this, keying the dedup on anything coarser than the file itself (the parent
// directory, say) would pass every other test here.
func TestWorkspaceEditTargets_DistinctFilesUnderOneSymlinkAreKept(t *testing.T) {
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}
	we := &protocol.WorkspaceEdit{Changes: map[string][]protocol.TextEdit{
		"file://" + filepath.Join(realDir, "one.txt"):       {{NewText: "1"}},
		"file://" + filepath.Join(dir, "link", "two.txt"):   {{NewText: "2"}},
		"file://" + filepath.Join(dir, "link", "three.txt"): {{NewText: "3"}},
	}}
	targets, err := workspaceEditTargets(we)
	if err != nil {
		t.Fatalf("three distinct files must not be refused: %v", err)
	}
	if len(targets) != 3 {
		t.Fatalf("got %d targets, want 3: %v", len(targets), targets)
	}
}

// When a rollback cannot restore a file, the caller is told which files it could
// not restore — those bytes are left modified on disk, and the apply's own
// bookkeeping (LSP notify, undo, write tracker) never runs for them.
func TestRollbackWorkspaceEdit_ReportsUnrestorableFiles(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("read-only directory is not enforced for root")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(dir, "good.txt")
	stuck := filepath.Join(locked, "stuck.txt")
	if err := os.WriteFile(good, []byte("GOOD\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stuck, []byte("STUCK\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Both files are already written ("modified"); restoring stuck.txt fails
	// because its parent directory refuses the rename.
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	plans := []workspaceEditPlan{
		{path: good, before: []byte("good\n"), after: []byte("GOOD\n"), mode: 0o644},
		{path: stuck, before: []byte("stuck\n"), after: []byte("STUCK\n"), mode: 0o644},
	}
	err := rollbackWorkspaceEdit(plans, []string{good, stuck})
	if err == nil {
		t.Fatal("expected the unrestorable file to be reported")
	}
	if !strings.Contains(err.Error(), stuck) {
		t.Errorf("the error must name the file it could not restore, got: %v", err)
	}
	if got, _ := os.ReadFile(good); string(got) != "good\n" {
		t.Errorf("a restorable file must still be rolled back: %q", got)
	}
	if got, _ := os.ReadFile(stuck); string(got) != "STUCK\n" {
		t.Errorf("the unrestorable file is left modified, as the error says: %q", got)
	}
}

// Two spellings of the same file (via a symlinked directory) canonicalise to
// one lock key; a raw-string dedup would acquire the same non-reentrant mutex
// twice and deadlock while holding it.
func TestLockPaths_DedupsAliasedPaths(t *testing.T) {
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	f := filepath.Join(realDir, "a.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	done := make(chan []func(), 1)
	go func() { done <- lockPaths([]string{f, filepath.Join(link, "a.txt")}) }()
	select {
	case unlocks := <-done:
		if len(unlocks) != 1 {
			t.Errorf("aliased spellings must collapse to one lock, got %d", len(unlocks))
		}
		unlockAll(unlocks)
	case <-time.After(10 * time.Second):
		t.Fatal("lockPaths deadlocked on two spellings of the same file")
	}
}

func TestApplyWorkspaceEdit_RollsBackOnMidWriteFailure(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("read-only directory is not enforced for root")
	}
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(a, []byte("aaa\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	zdir := filepath.Join(dir, "z")
	if err := os.Mkdir(zdir, 0o755); err != nil {
		t.Fatal(err)
	}
	z := filepath.Join(zdir, "z.txt")
	if err := os.WriteFile(z, []byte("zzz\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Writes land in sorted path order (a.txt first). A read-only parent
	// directory makes z.txt's staged temp file fail, after a.txt has been
	// written — the mid-sequence failure the rollback must undo.
	if err := os.Chmod(zdir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(zdir, 0o755) })

	line0 := protocol.Range{Start: protocol.Position{Line: 0, Character: 0}, End: protocol.Position{Line: 0, Character: 3}}
	we := &protocol.WorkspaceEdit{
		Changes: map[string][]protocol.TextEdit{
			"file://" + a: {{Range: line0, NewText: "AAA"}},
			"file://" + z: {{Range: line0, NewText: "ZZZ"}},
		},
	}
	if _, err := applyWorkspaceEdit(we); err == nil {
		t.Fatal("expected the unwritable second target to fail the apply")
	}
	if got, _ := os.ReadFile(a); string(got) != "aaa\n" {
		t.Fatalf("a.txt was not rolled back after the mid-apply failure: %q", got)
	}
	if got, _ := os.ReadFile(z); string(got) != "zzz\n" {
		t.Fatalf("z.txt changed despite its write failing: %q", got)
	}
}

func TestFindSymbolByPath(t *testing.T) {
	syms := []protocol.DocumentSymbol{
		{Name: "Foo", Children: []protocol.DocumentSymbol{
			{Name: "Bar"},
			{Name: "Baz", Children: []protocol.DocumentSymbol{{Name: "Inner"}}},
		}},
		{Name: "Top"},
	}
	if findSymbolByPath(syms, "Top") == nil {
		t.Error("expected Top")
	}
	if findSymbolByPath(syms, "Foo/Bar") == nil {
		t.Error("expected Foo/Bar")
	}
	if findSymbolByPath(syms, "Foo/Baz/Inner") == nil {
		t.Error("expected Foo/Baz/Inner")
	}
	if findSymbolByPath(syms, "Missing") != nil {
		t.Error("Missing should not be found")
	}
	if findSymbolByPath(syms, "") != nil {
		t.Error("empty path should not match")
	}
}

// TestFindSymbolByPath_StripsArgList guards that the semantic-edit tools'
// by-name addressing resolves a member a server reports with its signature
// (sourcekit-lsp names Swift methods "show()" / "load(from:)") from a plain
// name path — the same base-name match the read/query tools use, so editing a
// Swift member by name no longer silently degrades to the topology fallback.
func TestFindSymbolByPath_StripsArgList(t *testing.T) {
	syms := []protocol.DocumentSymbol{
		{Name: "PanelController", Children: []protocol.DocumentSymbol{
			{Name: "show()"},
			{Name: "load(from:)"},
		}},
	}
	if got := findSymbolByPath(syms, "PanelController/show"); got == nil || got.Name != "show()" {
		t.Errorf("plain name should resolve the signatured member, got %v", got)
	}
	if got := findSymbolByPath(syms, "PanelController/load"); got == nil || got.Name != "load(from:)" {
		t.Errorf("argument-label member should resolve by base name, got %v", got)
	}
	if findSymbolByPath(syms, "PanelController/show()") == nil {
		t.Error("the exact signatured name should still resolve")
	}
	if findSymbolByPath(syms, "PanelController/sho") != nil {
		t.Error("a non-matching prefix must not resolve")
	}
}

func TestApplyTextEdits_PureMatchesFileWrite(t *testing.T) {
	const src = "line0\nline1\nline2\nline3\n"
	edits := []protocol.TextEdit{
		{Range: protocol.Range{
			Start: protocol.Position{Line: 1, Character: 0},
			End:   protocol.Position{Line: 1, Character: 5},
		}, NewText: "LINE1"},
		{Range: protocol.Range{
			Start: protocol.Position{Line: 3, Character: 0},
			End:   protocol.Position{Line: 3, Character: 0},
		}, NewText: "X"},
	}

	// Pure result.
	pure, err := applyTextEdits([]byte(src), append([]protocol.TextEdit(nil), edits...))
	if err != nil {
		t.Fatalf("applyTextEdits: %v", err)
	}

	// File-write result must agree byte-for-byte.
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := applyTextEditsToFile(path, append([]protocol.TextEdit(nil), edits...)); err != nil {
		t.Fatalf("applyTextEditsToFile: %v", err)
	}
	onDisk, _ := os.ReadFile(path)

	want := "line0\nLINE1\nline2\nXline3\n"
	if string(pure) != want {
		t.Errorf("pure result\n got: %q\nwant: %q", pure, want)
	}
	if string(onDisk) != string(pure) {
		t.Errorf("file write diverged from pure result\n file: %q\n pure: %q", onDisk, pure)
	}
}
