package topology

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
)

// TestOpenCallSites pins which calls of a name stay open after a resolution pass:
// each open kind with a site that produces it, and the controls that must not be
// open (a resolved call, a same-file call of a name its file declares once).
func TestOpenCallSites(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "open.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	node := func(file int64, p string, n Node) int64 {
		n.Language = "go"
		return insertTestNode(t, db, file, p, n)
	}
	goFile := func(p, pkg string) int64 {
		id := insertLangFile(t, db, p, "go")
		node(id, p, Node{Kind: KindPackage, Name: pkg})
		return id
	}
	site := func(file, enclosing int64, qualifier any, callee string) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO topology_call_sites(file_id, enclosing_id, language, site_kind, callee, qualifier)
			VALUES (?,?,?,?,?,?)`, file, enclosing, "go", string(CallSiteCall), callee, qualifier); err != nil {
			t.Fatal(err)
		}
	}
	const lib, twin, app, dot = "internal/lib/lib.go", "internal/lib/twin.go", "internal/app/app.go", "internal/app/dot.go"
	libFile := goFile(lib, "lib")
	node(libFile, lib, Node{Kind: KindFunction, Name: "Do", Signature: "func Do()"})
	node(libFile, lib, Node{Kind: KindMethod, Name: "Run", Qualified: "(*T).Run", Signature: "func (t *T) Run()"})
	node(libFile, lib, Node{Kind: KindFunction, Name: "unique", Signature: "func unique()"})
	libCaller := node(libFile, lib, Node{Kind: KindFunction, Name: "caller", Signature: "func caller()"})
	site(libFile, libCaller, nil, "unique") // control: same file, declared once

	twinFile := goFile(twin, "lib")
	node(twinFile, twin, Node{Kind: KindFunction, Name: "Close", Signature: "func Close()"})
	node(twinFile, twin, Node{Kind: KindMethod, Name: "Close", Qualified: "(B).Close", Signature: "func (b B) Close()"})
	twinCaller := node(twinFile, twin, Node{Kind: KindFunction, Name: "shut", Signature: "func shut()"})
	site(twinFile, twinCaller, nil, "Close") // same file, but the file declares Close twice

	appFile := goFile(app, "app")
	node(appFile, app, Node{Kind: KindImport, Name: "lib", Qualified: "example.com/m/internal/lib"})
	appCaller := node(appFile, app, Node{Kind: KindFunction, Name: "main", Signature: "func main()"})
	site(appFile, appCaller, "lib", "Do")          // control: resolves through the import
	site(appFile, appCaller, "x", "Run")           // a receiver no declaration types
	site(appFile, appCaller, nil, "Do")            // bare, and app declares no Do
	site(appFile, appCaller, QualifierLocal, "Do") // bare, and main also declares a Do

	dotFile := goFile(dot, "app")
	node(dotFile, dot, Node{Kind: KindImport, Name: ".", Qualified: "example.com/m/internal/lib"})
	dotCaller := node(dotFile, dot, Node{Kind: KindFunction, Name: "viaDot", Signature: "func viaDot()"})
	site(dotFile, dotCaller, nil, "Do") // bare in a file that dot-imports a package

	if err := (&Indexer{db: db}).resolveCalls(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := &Store{db: db}
	ctx := context.Background()
	kinds := func(name string) []string {
		t.Helper()
		open, total, err := s.OpenCallSites(ctx, name, "go", 0)
		if err != nil {
			t.Fatal(err)
		}
		if total != len(open) {
			t.Fatalf("%s: total %d, listed %d with no limit", name, total, len(open))
		}
		out := make([]string, 0, len(open))
		for _, o := range open {
			k := o.Path + " " + o.Kind
			if o.DotImport {
				k += " dot"
			}
			out = append(out, k)
		}
		slices.Sort(out)
		return out
	}
	for name, want := range map[string][]string{
		"Do":     {app + " bare", app + " local", dot + " bare dot"},
		"Run":    {app + " receiver"},
		"Close":  {twin + " same-file"},
		"unique": nil,
	} {
		if got := kinds(name); !slices.Equal(got, want) {
			t.Errorf("open calls of %s = %q, want %q", name, got, want)
		}
	}
	var resolved int
	if err := db.QueryRow(`SELECT outcome FROM topology_call_sites WHERE callee = 'Do' AND qualifier = 'lib'`).Scan(&resolved); err != nil {
		t.Fatal(err)
	}
	if resolveBucket(resolved) != bucketResolved {
		t.Errorf("lib.Do's recorded outcome = %d, want resolved (%d)", resolved, bucketResolved)
	}
	open, total, err := s.OpenCallSites(ctx, "Do", "go", 1)
	if err != nil || total != 3 || len(open) != 1 {
		t.Errorf("limit 1: listed %d of total %d (err %v), want 1 of 3", len(open), total, err)
	}
}
