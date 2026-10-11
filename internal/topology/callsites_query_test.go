package topology

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type siteSeed struct {
	kind, callee, qualifier string // qualifier "" is stored as NULL
	startByte               int
	enclosing               int64
}

// seedCallSiteIndex builds an index shaped like a small repository: one package
// spread over several files, a test package beside it, a subdirectory, another
// language and the workspace root. It returns the db and the ids of the functions
// A (pkg/a.go) and B (pkg/b.go).
func seedCallSiteIndex(t *testing.T) (db *sql.DB, aFn, bFn int64) {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "sites.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	file := func(p, lang, pkg string) int64 {
		id := insertTestFile(t, db, p)
		if _, err := db.Exec(`UPDATE topology_files SET language = ? WHERE id = ?`, lang, id); err != nil {
			t.Fatal(err)
		}
		if pkg != "" {
			insertTestNode(t, db, id, p, Node{Kind: KindPackage, Name: pkg, Language: lang})
		}
		return id
	}
	sites := func(fileID int64, ss ...siteSeed) {
		for _, s := range ss {
			var qualifier, enclosing any
			if s.qualifier != "" {
				qualifier = s.qualifier
			}
			if s.enclosing != 0 {
				enclosing = s.enclosing
			}
			if _, err := db.Exec(`INSERT INTO topology_call_sites(file_id, enclosing_id, language, site_kind, callee, qualifier, start_byte, start_line)
				VALUES (?, ?, 'go', ?, ?, ?, ?, 1)`, fileID, enclosing, s.kind, s.callee, qualifier, s.startByte); err != nil {
				t.Fatal(err)
			}
		}
	}
	a := file("pkg/a.go", "go", "pkg")
	aFn = insertTestNode(t, db, a, "pkg/a.go", Node{Kind: KindFunction, Name: "A", Qualified: "A", Language: "go", StartLine: 3, EndLine: 5})
	insertTestNode(t, db, a, "pkg/a.go", Node{Kind: KindType, Name: "B", Qualified: "B", Language: "go", StartLine: 7, EndLine: 7})
	sites(a,
		siteSeed{"call", "B", "", 40, aFn},
		siteSeed{"call", "Println", "fmt", 20, aFn},
		siteSeed{"field", "Use", "cobra.Command", 60, aFn}, // not a call
		siteSeed{"call", "B", "", 5, 0},                    // outside every declaration
	)
	b := file("pkg/b.go", "go", "pkg")
	bFn = insertTestNode(t, db, b, "pkg/b.go", Node{Kind: KindFunction, Name: "B", Qualified: "B", Language: "go"})
	insertTestNode(t, db, b, "pkg/b.go", Node{Kind: KindMethod, Name: "B", Qualified: "(*T).B", Language: "go"})
	sites(b, siteSeed{"call", "A", "", 10, bFn})
	bt := file("pkg/b_test.go", "go", "pkg_test")
	sites(bt, siteSeed{"call", "A", "pkg", 10, 0})
	c := file("pkg/sub/c.go", "go", "sub")
	insertTestNode(t, db, c, "pkg/sub/c.go", Node{Kind: KindFunction, Name: "B", Qualified: "B", Language: "go"})
	sites(c, siteSeed{"call", "B", "", 1, 0})
	py := file("pkg/x.py", "python", "")
	sites(py, siteSeed{"call", "B", "", 1, 0})
	d := file("other/d.go", "go", "other")
	sites(d, siteSeed{"call", "B", "", 1, 0})
	root := file("main.go", "go", "main")
	sites(root, siteSeed{"call", "A", "", 1, 0})
	return db, aFn, bFn
}

func TestCallSitesInDir_ReturnsTheDirectoryCallsWithTheirPackage(t *testing.T) {
	db, aFn, bFn := seedCallSiteIndex(t)
	got, truncated, err := callSitesInDir(context.Background(), db, "pkg", "go", nil, 0)
	if err != nil || truncated {
		t.Fatalf("err=%v truncated=%v", err, truncated)
	}
	want := []CallSiteRef{
		{Path: "pkg/a.go", Package: "pkg", Callee: "B", StartByte: 5, StartLine: 1},
		{Path: "pkg/a.go", Package: "pkg", Callee: "Println", Qualifier: "fmt", StartByte: 20, StartLine: 1, EnclosingID: aFn},
		{Path: "pkg/a.go", Package: "pkg", Callee: "B", StartByte: 40, StartLine: 1, EnclosingID: aFn},
		{Path: "pkg/b.go", Package: "pkg", Callee: "A", StartByte: 10, StartLine: 1, EnclosingID: bFn},
		{Path: "pkg/b_test.go", Package: "pkg_test", Callee: "A", Qualifier: "pkg", StartByte: 10, StartLine: 1},
	}
	if !slices.Equal(got, want) {
		t.Errorf("sites =\n %+v\nwant\n %+v", got, want)
	}
}

func TestCallSitesInDir_ScopesToTheDirectoryAndLanguage(t *testing.T) {
	db, _, _ := seedCallSiteIndex(t)
	for _, tc := range []struct {
		dir, lang string
		want      []string
	}{
		{"pkg", "go", []string{"pkg/a.go", "pkg/b.go", "pkg/b_test.go"}},
		{"pkg/sub", "go", []string{"pkg/sub/c.go"}},
		{"pkg", "python", []string{"pkg/x.py"}},
		{".", "go", []string{"main.go"}},
		{"nothing", "go", nil},
		{"pk", "go", nil}, // a prefix of a directory name is not the directory
	} {
		sites, _, err := callSitesInDir(context.Background(), db, tc.dir, tc.lang, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		var paths []string
		for _, s := range sites {
			if !slices.Contains(paths, s.Path) {
				paths = append(paths, s.Path)
			}
		}
		if !slices.Equal(paths, tc.want) {
			t.Errorf("dir %q (%s): files %v, want %v", tc.dir, tc.lang, paths, tc.want)
		}
	}
}

// Naming callees reads only the calls of those names, however many names there are:
// more than one query carries are read in chunks and come back together.
func TestCallSitesInDir_NamingCalleesReadsOnlyThoseCalls(t *testing.T) {
	db, _, _ := seedCallSiteIndex(t)
	callees := func(sites []CallSiteRef) []string {
		out := make([]string, 0, len(sites))
		for _, s := range sites {
			out = append(out, s.Path+":"+s.Callee)
		}
		return out
	}
	got, _, err := callSitesInDir(context.Background(), db, "pkg", "go", []string{"B"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"pkg/a.go:B", "pkg/a.go:B"}; !slices.Equal(callees(got), want) {
		t.Errorf("calls of B = %v, want %v", callees(got), want)
	}
	names := make([]string, 0, callSiteNames+3)
	for i := range callSiteNames + 1 {
		names = append(names, fmt.Sprintf("unused%03d", i))
	}
	names = append(names, "A", "Println")
	got, _, err = callSitesInDir(context.Background(), db, "pkg", "go", names, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"pkg/a.go:Println", "pkg/b.go:A", "pkg/b_test.go:A"}; !slices.Equal(callees(got), want) {
		t.Errorf("calls named across two chunks = %v, want %v", callees(got), want)
	}
	if none, _, _ := callSitesInDir(context.Background(), db, "pkg", "go", []string{"Absent"}, 0); len(none) != 0 {
		t.Errorf("a callee nothing calls returned %v", none)
	}
}

func TestCallSitesInDir_ALimitIsReportedAsTruncation(t *testing.T) {
	db, _, _ := seedCallSiteIndex(t)
	got, truncated, err := callSitesInDir(context.Background(), db, "pkg", "go", nil, 3)
	if err != nil || !truncated || len(got) != 3 {
		t.Errorf("limit 3: %d sites, truncated=%v, err=%v; want 3 and truncated", len(got), truncated, err)
	}
	if _, truncated, _ := callSitesInDir(context.Background(), db, "pkg", "go", nil, 5); truncated {
		t.Error("control: a limit equal to the count was reported as a truncation")
	}
}

// A directory of hundreds of files is read whole, in path order.
func TestCallSitesInDir_ReadsALargeDirectoryWhole(t *testing.T) {
	db, _, _ := seedCallSiteIndex(t)
	const files = 450
	for i := range files {
		p := fmt.Sprintf("wide/f%04d.go", i)
		id := insertTestFile(t, db, p)
		if _, err := db.Exec(`UPDATE topology_files SET language = 'go' WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO topology_call_sites(file_id, language, site_kind, callee, start_byte, start_line) VALUES (?, 'go', 'call', 'X', 1, 1)`, id); err != nil {
			t.Fatal(err)
		}
	}
	got, truncated, err := callSitesInDir(context.Background(), db, "wide", "go", nil, 0)
	if err != nil || truncated || len(got) != files {
		t.Fatalf("%d sites, truncated=%v, err=%v; want %d", len(got), truncated, err, files)
	}
	if !slices.IsSortedFunc(got, func(a, b CallSiteRef) int { return strings.Compare(a.Path, b.Path) }) {
		t.Error("sites are not in path order")
	}
}

func TestDeclsNamedInDir_ListsFunctionsAndMethodsOfTheDirectoryOnly(t *testing.T) {
	db, _, _ := seedCallSiteIndex(t)
	got, err := declsNamedInDir(context.Background(), db, "pkg", "go", "B")
	if err != nil {
		t.Fatal(err)
	}
	kinds := make([]string, 0, len(got))
	for _, n := range got {
		kinds = append(kinds, n.Path+":"+string(n.Kind))
	}
	// The type B in a.go, and the function B in the subdirectory, are not declarations
	// of the package that a call could mean.
	if want := []string{"pkg/b.go:function", "pkg/b.go:method"}; !slices.Equal(kinds, want) {
		t.Errorf("declarations = %v, want %v", kinds, want)
	}
}
