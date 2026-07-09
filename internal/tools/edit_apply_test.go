package tools

import (
	"os"
	"path/filepath"
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
		got := workspaceEditTargets(we)
		for i, w := range want {
			if got[i].path != w {
				t.Fatalf("targets[%d].path = %q, want %q", i, got[i].path, w)
			}
		}
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
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	f := filepath.Join(real, "a.txt")
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
