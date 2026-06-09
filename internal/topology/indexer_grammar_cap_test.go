package topology

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/langsupport"
)

// fakeMarkdownExtractor stands in for the real Markdown tree-sitter extractor so
// the per-grammar cap can be exercised deterministically: it emits exactly one
// node for any non-empty source. Its Language() is "markdown", which carries a
// MaxParseBytes cap in the langsupport registry.
type fakeMarkdownExtractor struct{}

func (fakeMarkdownExtractor) Language() string     { return "markdown" }
func (fakeMarkdownExtractor) Extensions() []string { return []string{".md", ".markdown"} }

func (fakeMarkdownExtractor) Extract(_ context.Context, relPath string, src []byte) ([]Node, []Edge, error) {
	if len(src) == 0 {
		return nil, nil, nil
	}
	return []Node{{Kind: KindFunction, Name: "doc", Language: "markdown", Path: relPath}}, nil, nil
}

func TestMarkdownRegistryHasParseCap(t *testing.T) {
	l, ok := langsupport.ByName("markdown")
	if !ok {
		t.Fatal("markdown not in registry")
	}
	if l.MaxParseBytes != 256<<10 {
		t.Errorf("markdown MaxParseBytes = %d, want %d", l.MaxParseBytes, 256<<10)
	}
}

// TestIndexer_OversizedGrammarSkippedButRecorded proves the fix-3 behaviour: a
// Markdown file above the per-grammar cap is recorded (with this content's hash)
// but parsed to zero symbols, while a small one below the cap yields nodes. The
// global maxSize (512 KiB) stays larger than the per-grammar cap so it is not
// what excludes the large file.
func TestIndexer_OversizedGrammarSkippedButRecorded(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, ".plumb", "topo.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	small := "# title\n\nsome prose\n"
	large := "# title\n\n" + strings.Repeat("word ", 100*1024) // ~500 KB, above the 256 KiB cap, below 512 KiB global
	if err := os.WriteFile(filepath.Join(dir, "small.md"), []byte(small), 0o644); err != nil {
		t.Fatalf("write small: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "large.md"), []byte(large), 0o644); err != nil {
		t.Fatalf("write large: %v", err)
	}

	idx := newIndexer(dir, db, []Extractor{fakeMarkdownExtractor{}}, 512*1024, 0)
	for _, p := range []string{"small.md", "large.md"} {
		if err := idx.processUpsert(context.Background(), p); err != nil {
			t.Fatalf("processUpsert(%s): %v", p, err)
		}
	}

	nodeCount := func(path string) int {
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM topology_nodes n JOIN topology_files f ON n.file_id = f.id WHERE f.path = ?`, path).Scan(&n) //nolint:errcheck
		return n
	}
	fileRecorded := func(path string) bool {
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM topology_files WHERE path = ?`, path).Scan(&n) //nolint:errcheck
		return n == 1
	}

	if got := nodeCount("small.md"); got == 0 {
		t.Error("small.md: expected nodes (below cap), got 0")
	}
	if got := nodeCount("large.md"); got != 0 {
		t.Errorf("large.md: expected 0 nodes (above per-grammar cap), got %d", got)
	}
	if !fileRecorded("large.md") {
		t.Error("large.md: expected a file record (skip-and-record, not skip-entirely)")
	}
}

func TestShouldReclaimAfterBurst(t *testing.T) {
	cases := []struct {
		n    int
		want bool
	}{
		{0, false},
		{1, false},
		{reclaimAfterOps - 1, false},
		{reclaimAfterOps, true},
		{reclaimAfterOps + 100, true},
	}
	for _, c := range cases {
		if got := shouldReclaimAfterBurst(c.n); got != c.want {
			t.Errorf("shouldReclaimAfterBurst(%d) = %v, want %v", c.n, got, c.want)
		}
	}
}
