package topology

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// insertLangFile is insertTestFile with a language, which the call resolver's
// scope and census queries read.
func insertLangFile(t *testing.T, db *sql.DB, path, lang string) int64 {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO topology_files(path, language, mtime_ns, content_hash, indexed_at, error_msg)
         VALUES (?,?,?,?,?,?)`, path, lang, 0, "abc", 0, "")
	if err != nil {
		t.Fatalf("insert file %q: %v", path, err)
	}
	id, _ := res.LastInsertId()
	return id
}

func insertSite(t *testing.T, db *sql.DB, fileID, enclosing int64, lang string, kind CallSiteKind, qualifier, callee string) {
	t.Helper()
	var q any
	if qualifier != "" {
		q = qualifier
	}
	var enc any
	if enclosing != 0 {
		enc = enclosing
	}
	if _, err := db.Exec(
		`INSERT INTO topology_call_sites(file_id, enclosing_id, language, site_kind, callee, qualifier)
         VALUES (?,?,?,?,?,?)`, fileID, enc, lang, string(kind), callee, q); err != nil {
		t.Fatalf("insert call site %s.%s: %v", qualifier, callee, err)
	}
}

func callMeta(t *testing.T, db *sql.DB, key string) int {
	t.Helper()
	var v int
	if err := db.QueryRow(`SELECT CAST(value AS INTEGER) FROM topology_meta WHERE key=?`, key).Scan(&v); err != nil {
		t.Fatalf("meta %s: %v", key, err)
	}
	return v
}

// resolverFixture builds the index shape the real extractor produces for three
// packages, and returns the ids the assertions need.
//
// It is adversarial in both directions on purpose. internal/beta declares a
// function with the SAME name as internal/alpha's, so a resolver that matched on
// name alone would produce a false edge; and internal/alpha genuinely IS
// imported and called, so a resolver that refused everything would lose a real
// one.
type resolverFixture struct {
	db          *sql.DB
	alphaDo     int64
	betaDo      int64
	alphaHidden int64
	run         int64
}

func newResolverFixture(t *testing.T) *resolverFixture {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "calls.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	alphaFile := insertLangFile(t, db, "internal/alpha/alpha.go", "go")
	insertTestNode(t, db, alphaFile, "internal/alpha/alpha.go", Node{Kind: KindPackage, Name: "alpha", Language: "go"})
	alphaDo := insertTestNode(t, db, alphaFile, "internal/alpha/alpha.go", Node{Kind: KindFunction, Name: "Do", Language: "go"})
	alphaHidden := insertTestNode(t, db, alphaFile, "internal/alpha/alpha.go", Node{Kind: KindFunction, Name: "hidden", Language: "go"})

	betaFile := insertLangFile(t, db, "internal/beta/beta.go", "go")
	insertTestNode(t, db, betaFile, "internal/beta/beta.go", Node{Kind: KindPackage, Name: "beta", Language: "go"})
	betaDo := insertTestNode(t, db, betaFile, "internal/beta/beta.go", Node{Kind: KindFunction, Name: "Do", Language: "go"})

	callerFile := insertLangFile(t, db, "internal/caller/caller.go", "go")
	insertTestNode(t, db, callerFile, "internal/caller/caller.go", Node{Kind: KindPackage, Name: "caller", Language: "go"})
	insertTestNode(t, db, callerFile, "internal/caller/caller.go",
		Node{Kind: KindImport, Name: "alpha", Qualified: "example.com/m/internal/alpha", Language: "go"})
	insertTestNode(t, db, callerFile, "internal/caller/caller.go",
		Node{Kind: KindImport, Name: "strings", Qualified: "strings", Language: "go"})
	run := insertTestNode(t, db, callerFile, "internal/caller/caller.go", Node{Kind: KindFunction, Name: "Run", Language: "go"})

	insertSite(t, db, callerFile, run, "go", CallSiteCall, "alpha", "Do")      // resolves
	insertSite(t, db, callerFile, run, "go", CallSiteCall, "c", "Method")      // receiver
	insertSite(t, db, callerFile, run, "go", CallSiteCall, "strings", "Join")  // external
	insertSite(t, db, callerFile, run, "go", CallSiteCall, "alpha", "hidden")  // unexported behind a qualifier
	insertSite(t, db, callerFile, run, "go", CallSiteCall, "alpha", "Missing") // no such target
	insertSite(t, db, callerFile, run, "go", CallSiteCall, "", "helper")       // bare: not a qualified site
	insertSite(t, db, callerFile, run, "go", CallSiteField, "cobra.Command", "Use")

	return &resolverFixture{db: db, alphaDo: alphaDo, betaDo: betaDo, alphaHidden: alphaHidden, run: run}
}

func (f *resolverFixture) resolve(t *testing.T) {
	t.Helper()
	idx := &Indexer{db: f.db}
	if err := idx.resolveCalls(context.Background()); err != nil {
		t.Fatalf("resolveCalls: %v", err)
	}
}

func (f *resolverFixture) edgeCount(t *testing.T, from, to int64) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow(
		`SELECT COUNT(*) FROM topology_edges WHERE from_id=? AND to_id=? AND kind=? AND source=?`,
		from, to, string(EdgeCalls), callResolverSource).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestResolveCalls_ResolvesPackageQualifiedCallAcrossFiles is the missing-edge
// direction: the one call this design is meant to reach must be reached, and the
// edge must actually cross a file boundary.
func TestResolveCalls_ResolvesPackageQualifiedCallAcrossFiles(t *testing.T) {
	f := newResolverFixture(t)
	f.resolve(t)

	if got := f.edgeCount(t, f.run, f.alphaDo); got != 1 {
		t.Errorf("Run→alpha.Do edges = %d, want 1", got)
	}
	var crossFile int
	if err := f.db.QueryRow(`
		SELECT COUNT(*) FROM topology_edges e
		  JOIN topology_nodes a ON a.id = e.from_id
		  JOIN topology_nodes b ON b.id = e.to_id
		 WHERE e.source = ? AND a.file_id <> b.file_id`, callResolverSource).Scan(&crossFile); err != nil {
		t.Fatal(err)
	}
	if crossFile != 1 {
		t.Errorf("cross-file resolver edges = %d, want 1 — an edge that does not cross a file is not what this pass is for", crossFile)
	}
}

// TestResolveCalls_DoesNotLinkASameNamedFunctionInAnotherPackage is the
// false-edge direction, and it is the failure mode name-only resolution has:
// this workspace holds 2,588 callables sharing a name with another.
func TestResolveCalls_DoesNotLinkASameNamedFunctionInAnotherPackage(t *testing.T) {
	f := newResolverFixture(t)
	f.resolve(t)

	if got := f.edgeCount(t, f.run, f.betaDo); got != 0 {
		t.Errorf("Run→beta.Do edges = %d, want 0 — beta is never imported by the caller", got)
	}
	if f.edgeCount(t, f.run, f.alphaDo) == 0 {
		t.Error("the guard passed only because NOTHING resolved; alpha.Do must still be linked")
	}
}

// TestResolveCalls_UnexportedCalleeBehindAQualifierIsNotAPackageCall pins the
// shadowing guard: a local variable can share a name with an import, and without
// the exported-name requirement `topology.walk()` in a file that also imports
// .../topology would resolve to that package's unexported walk.
func TestResolveCalls_UnexportedCalleeBehindAQualifierIsNotAPackageCall(t *testing.T) {
	f := newResolverFixture(t)
	f.resolve(t)

	if got := f.edgeCount(t, f.run, f.alphaHidden); got != 0 {
		t.Errorf("Run→alpha.hidden edges = %d, want 0 — an unexported callee is never a package-qualified call", got)
	}
}

// TestResolveCalls_BucketsAccountForEveryQualifiedSite is the honesty check. The
// four outcome buckets must SUM to the qualified-site count: if they do not, a
// site was dropped somewhere and "how much of the call graph is this" stops
// being answerable.
func TestResolveCalls_BucketsAccountForEveryQualifiedSite(t *testing.T) {
	f := newResolverFixture(t)
	f.resolve(t)

	qualified := callMeta(t, f.db, metaCallQualifiedSites)
	sum := callMeta(t, f.db, metaCallResolved) +
		callMeta(t, f.db, metaCallUnresolvedRecv) +
		callMeta(t, f.db, metaCallExternal) +
		callMeta(t, f.db, metaCallUnmatched)
	if sum != qualified {
		t.Errorf("buckets sum to %d but %d qualified sites were examined; a site went unaccounted for", sum, qualified)
	}
	if qualified != 5 {
		t.Errorf("qualified sites = %d, want 5 — the bare call and the field site must not be counted", qualified)
	}
	// The receiver bucket carries BOTH the genuine method call and the shadowed
	// qualifier, which is the correct reading of each.
	if got := callMeta(t, f.db, metaCallUnresolvedRecv); got != 2 {
		t.Errorf("unresolved receiver = %d, want 2", got)
	}
	if got := callMeta(t, f.db, metaCallExternal); got != 1 {
		t.Errorf("external package = %d, want 1", got)
	}
	if got := callMeta(t, f.db, metaCallUnmatched); got != 1 {
		t.Errorf("unmatched target = %d, want 1", got)
	}
	// The denominator the reach percentage is published against counts EVERY
	// call site, including the bare ones this resolver does not attempt.
	if got := callMeta(t, f.db, metaCallSites); got != 6 {
		t.Errorf("call sites = %d, want 6 — bare calls belong in the denominator, the field site does not", got)
	}
}

// TestResolveCalls_MethodCallProducesNoEdge states the deliberate absence in the
// form a consumer would notice: the receiver call is present in the table and
// contributes nothing to the graph.
func TestResolveCalls_MethodCallProducesNoEdge(t *testing.T) {
	f := newResolverFixture(t)
	f.resolve(t)

	var total int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM topology_edges WHERE source=?`, callResolverSource).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Errorf("resolver edges = %d, want exactly 1 — only the package-qualified call may resolve", total)
	}
	var recorded int
	if err := f.db.QueryRow(
		`SELECT COUNT(*) FROM topology_call_sites WHERE qualifier='c' AND callee='Method'`).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded != 1 {
		t.Error("the method call was not even recorded; an unresolved call must still be countable")
	}
}

// TestResolveCalls_IntraFileCountExcludesResolverEdges pins that the number the
// refusal offers as "intra-file call edges only" really is only those. It is
// counted by source, not by whichever edges happen to exist.
func TestResolveCalls_IntraFileCountExcludesResolverEdges(t *testing.T) {
	f := newResolverFixture(t)
	before, err := AdmitCallGraph(context.Background(), f.db, CallGraphSubject{Language: "go", Path: "internal/caller/caller.go"})
	if err != nil {
		t.Fatal(err)
	}
	f.resolve(t)
	after, err := AdmitCallGraph(context.Background(), f.db, CallGraphSubject{Language: "go", Path: "internal/caller/caller.go"})
	if err != nil {
		t.Fatal(err)
	}
	var resolverEdgesFromThisFile int
	if err := f.db.QueryRow(`
		SELECT COUNT(*) FROM topology_edges e
		  JOIN topology_nodes n ON n.id = e.from_id
		  JOIN topology_files fl ON fl.id = n.file_id
		 WHERE e.source = ? AND fl.path = ?`,
		callResolverSource, "internal/caller/caller.go").Scan(&resolverEdgesFromThisFile); err != nil {
		t.Fatal(err)
	}
	if resolverEdgesFromThisFile == 0 {
		t.Fatal("the resolver produced no edge from this file; the guard would be vacuous")
	}
	if after.IntraFileCalls != before.IntraFileCalls {
		t.Errorf("intra-file count moved from %d to %d when %d resolver edges appeared; "+
			"the refusal would overstate what the index holds intra-file",
			before.IntraFileCalls, after.IntraFileCalls, resolverEdgesFromThisFile)
	}
}

// TestResolveCalls_RebuildsRatherThanAppends pins the derived-edge contract these
// edges share with the import resolver: a second pass must not duplicate.
func TestResolveCalls_RebuildsRatherThanAppends(t *testing.T) {
	f := newResolverFixture(t)
	f.resolve(t)
	f.resolve(t)

	var total int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM topology_edges WHERE source=?`, callResolverSource).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Errorf("resolver edges after two passes = %d, want 1", total)
	}
}

// TestResolveCalls_DoesNotCrossIntoAnotherLanguage pins the scoping half of the
// gating rule at the resolver: a language that is not admitted contributes no
// sites, no targets and no edges, even when its own extractor emitted package
// nodes and its call sites sit in the same index.
func TestResolveCalls_DoesNotCrossIntoAnotherLanguage(t *testing.T) {
	f := newResolverFixture(t)

	csFile := insertLangFile(t, f.db, "src/Alpha/Alpha.cs", "csharp")
	insertTestNode(t, f.db, csFile, "src/Alpha/Alpha.cs", Node{Kind: KindPackage, Name: "Alpha", Language: "csharp"})
	csDo := insertTestNode(t, f.db, csFile, "src/Alpha/Alpha.cs", Node{Kind: KindFunction, Name: "Do", Language: "csharp"})
	csCaller := insertLangFile(t, f.db, "src/Beta/Beta.cs", "csharp")
	insertTestNode(t, f.db, csCaller, "src/Beta/Beta.cs", Node{Kind: KindPackage, Name: "Beta", Language: "csharp"})
	insertTestNode(t, f.db, csCaller, "src/Beta/Beta.cs",
		Node{Kind: KindImport, Name: "Alpha", Qualified: "src/Alpha", Language: "csharp"})
	csRun := insertTestNode(t, f.db, csCaller, "src/Beta/Beta.cs", Node{Kind: KindFunction, Name: "Run", Language: "csharp"})
	insertSite(t, f.db, csCaller, csRun, "csharp", CallSiteCall, "Alpha", "Do")

	f.resolve(t)

	if got := f.edgeCount(t, csRun, csDo); got != 0 {
		t.Errorf("a C# call resolved to %d edges; C# is not in the supported set", got)
	}
	var nonGo int
	if err := f.db.QueryRow(`
		SELECT COUNT(*) FROM topology_edges e
		  JOIN topology_nodes a ON a.id = e.from_id
		  JOIN topology_nodes b ON b.id = e.to_id
		 WHERE e.source = ? AND (a.language <> 'go' OR b.language <> 'go')`,
		callResolverSource).Scan(&nonGo); err != nil {
		t.Fatal(err)
	}
	if nonGo != 0 {
		t.Errorf("%d resolver edges touch a non-Go node; an admitted traversal must not leave its language", nonGo)
	}
	if got := callMeta(t, f.db, metaCallQualifiedSites); got != 5 {
		t.Errorf("qualified sites = %d, want 5 — the C# site must not enter the Go tally", got)
	}
	if f.edgeCount(t, f.run, f.alphaDo) == 0 {
		t.Error("the guard passed only because NOTHING resolved; the Go edge must survive alongside the C# files")
	}
}
