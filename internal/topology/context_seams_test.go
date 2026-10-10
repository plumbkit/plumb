package topology

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// context_seams_test.go covers the small seams context_for_task leans on: an
// exact path hint, a classifier callers can feed a narrowed set, and the three
// Store accessors that let a caller decide whether an index span can be trusted
// for bytes it already holds.

// seedSuffixPairs indexes the same function name in files that are suffixes,
// substrings or case variants of one another, the shapes a substring path hint
// confuses.
func seedSuffixPairs(t *testing.T) func(name string, hint NodeHint) ([]Node, error) {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "pairs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, p := range []string{"pkg/x.go", "data.go", "Cart/cart.go", "cart/cart.go", "cart/cart_test.go", "x.go.bak/x.go"} {
		f := insertTestFile(t, db, p)
		insertTestNode(t, db, f, p, Node{Kind: KindFunction, Name: "F", Qualified: "F", Language: "go"})
	}
	return func(name string, hint NodeHint) ([]Node, error) {
		return ResolveNodes(context.Background(), db, name, hint)
	}
}

func nodePaths(nodes []Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Path)
	}
	return out
}

// The exact Path hint selects one file (or a directory's contents) and nothing
// that merely contains the text, where PathSubstr selects every file that does.
func TestNodeHint_PathIsExactNotSubstring(t *testing.T) {
	resolve := seedSuffixPairs(t)

	// Control: the substring hint reaches the files an exact path must not, or
	// the cases below would pass for any hint.
	for _, tc := range []struct {
		substr string
		want   []string
	}{
		{"x.go", []string{"pkg/x.go", "x.go.bak/x.go"}},
		{"a.go", []string{"data.go"}},
		{"cart/cart.go", []string{"Cart/cart.go", "cart/cart.go"}},
	} {
		loose, err := resolve("F", NodeHint{PathSubstr: tc.substr})
		if err != nil || !sameSet(nodePaths(loose), tc.want) {
			t.Fatalf("control: substring %q matched %v (%v), want %v", tc.substr, nodePaths(loose), err, tc.want)
		}
	}
	for _, tc := range []struct {
		name string
		hint string
		want []string
	}{
		{"a file", "pkg/x.go", []string{"pkg/x.go"}},
		{"a directory", "cart", []string{"cart/cart.go", "cart/cart_test.go"}},
		{"a directory with a trailing slash", "cart/", []string{"cart/cart.go", "cart/cart_test.go"}},
		{"case-sensitive", "Cart/cart.go", []string{"Cart/cart.go"}},
		{"a prefix of a file name is not a directory", "cart/cart", nil},
	} {
		got, err := resolve("F", NodeHint{Path: tc.hint})
		if tc.want == nil {
			var mismatch *HintMismatchError
			if !errors.As(err, &mismatch) || len(mismatch.Candidates) != 6 {
				t.Errorf("%s: got %v, %v; want a mismatch listing every candidate", tc.name, nodePaths(got), err)
			}
			continue
		}
		if err != nil || !sameSet(nodePaths(got), tc.want) {
			t.Errorf("%s: Path %q matched %v (%v), want %v", tc.name, tc.hint, nodePaths(got), err, tc.want)
		}
	}
	// A path that is only a suffix or a tail of indexed paths is a mismatch,
	// never a hit.
	for _, p := range []string{"x.go", "a.go"} {
		_, err := resolve("F", NodeHint{Path: p})
		var mismatch *HintMismatchError
		if !errors.As(err, &mismatch) {
			t.Fatalf("Path %q must not match pkg/x.go or data.go, got %v", p, err)
		}
		if !strings.Contains(mismatch.Error(), `exact path: "`+p+`"`) {
			t.Errorf("the mismatch does not name the exact path: %v", mismatch)
		}
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

// ClassifyNodes is the classifier ResolveSelector uses, so a caller that narrows
// the candidates first reaches the answer ResolveSelector would have on that set.
func TestClassifyNodes_AgreesWithResolveSelector(t *testing.T) {
	db := seedTwoSameName(t)
	defer db.Close()
	ctx := context.Background()
	for _, sel := range []string{"Nowhere", "AChild", "Target"} {
		nodes, err := ResolveNodes(ctx, db, sel, NodeHint{})
		if err != nil {
			t.Fatal(err)
		}
		want, err := ResolveSelector(ctx, db, sel, NodeHint{})
		if err != nil {
			t.Fatal(err)
		}
		got := ClassifyNodes(nodes)
		if got.Kind != want.Kind || got.Node.ID != want.Node.ID || len(got.Candidates) != len(want.Candidates) || len(got.Shadowed) != len(want.Shadowed) {
			t.Errorf("%q: ClassifyNodes = %+v, ResolveSelector = %+v", sel, got, want)
		}
	}
	if one := ClassifyNodes([]Node{{Kind: KindFunction, Name: "F"}}); one.Kind != ResolutionOne {
		t.Errorf("a single declaration classified as %v", one.Kind)
	}
	if !IsReference(KindImport) || !IsReference(KindPackage) || !IsReference(KindFile) || IsReference(KindFunction) {
		t.Error("IsReference does not separate references from declarations")
	}
}

func TestStoreRoot(t *testing.T) {
	if got := (&Store{workspace: "/work/a"}).Root(); got != "/work/a" {
		t.Errorf("Root() = %q, want the workspace the store indexes", got)
	}
}

func TestStoreIndexedContentHash(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "hash.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	insertTestFile(t, db, "a.go") // content_hash "abc"
	if _, err := db.Exec(`INSERT INTO topology_files(path, mtime_ns, content_hash, indexed_at, error_msg) VALUES ('bad.go', 0, '', 0, 'parse failed')`); err != nil {
		t.Fatal(err)
	}
	s := &Store{workspace: "/ws", db: db}
	ctx := context.Background()
	if h, ok, err := s.IndexedContentHash(ctx, "a.go"); err != nil || !ok || h != "abc" {
		t.Errorf("indexed file = %q, %v, %v; want abc", h, ok, err)
	}
	if h, ok, err := s.IndexedContentHash(ctx, "/ws/a.go"); err != nil || !ok || h != "abc" {
		t.Errorf("absolute spelling = %q, %v, %v; want the same row", h, ok, err)
	}
	if h, ok, err := s.IndexedContentHash(ctx, "missing.go"); err != nil || ok || h != "" {
		t.Errorf("unindexed file = %q, %v, %v; want not ok and no error", h, ok, err)
	}
	if h, ok, err := s.IndexedContentHash(ctx, "bad.go"); err != nil || ok || h != "" {
		t.Errorf("file recorded without a parse = %q, %v, %v; want not ok: its spans describe nothing", h, ok, err)
	}
	db.Close()
	if _, _, err := s.IndexedContentHash(ctx, "a.go"); err == nil {
		t.Error("a database error was swallowed")
	}
}

// lineExtractor is an extractor whose single node's span comes from the bytes it
// is given, so a test can tell whether ExtractSource parsed the supplied bytes or
// re-read the file.
type lineExtractor struct{}

func (lineExtractor) Language() string     { return "toy" }
func (lineExtractor) Extensions() []string { return []string{".toy"} }
func (lineExtractor) Extract(_ context.Context, path string, src []byte) ([]Node, []Edge, error) {
	line := 1 + strings.Count(string(src[:max(0, strings.Index(string(src), "decl"))]), "\n")
	return []Node{{Kind: KindFunction, Name: "decl", Qualified: "decl", StartLine: line, EndLine: line, Language: "toy", Path: path}}, nil, nil
}

func TestStoreExtractSource_ParsesTheSuppliedBytesNotTheFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.toy"), []byte("decl\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Store{workspace: root, idx: &Indexer{extractors: []Extractor{lineExtractor{}}}}
	ctx := context.Background()

	// The file on disk has decl on line 1; the snapshot the caller holds has it
	// on line 4. The answer must describe the snapshot.
	nodes, err := s.ExtractSource(ctx, "a.toy", []byte("\n\n\ndecl\n"))
	if err != nil || len(nodes) != 1 || nodes[0].StartLine != 4 || nodes[0].Path != "a.toy" {
		t.Fatalf("ExtractSource = %+v, %v; want the node on line 4 of the supplied bytes", nodes, err)
	}
	if abs, err := s.ExtractSource(ctx, filepath.Join(root, "a.toy"), []byte("decl\n")); err != nil || len(abs) != 1 || abs[0].Path != "a.toy" {
		t.Errorf("an absolute spelling must be indexed as the workspace-relative path, got %+v, %v", abs, err)
	}
	// A path no extractor handles is (nil, nil), as ExtractFile reports it.
	if nodes, err := s.ExtractSource(ctx, "notes.txt", []byte("decl")); nodes != nil || err != nil {
		t.Errorf("unhandled extension = %+v, %v; want nil, nil", nodes, err)
	}
	// And it never touches the file system: a path that does not exist parses.
	if nodes, err := s.ExtractSource(ctx, "ghost.toy", []byte("decl")); err != nil || len(nodes) != 1 {
		t.Errorf("a path with no file on disk = %+v, %v; want the supplied bytes parsed", nodes, err)
	}
}
