package tools

import (
	"os"
	"path/filepath"
	"testing"

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
