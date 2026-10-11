package topology

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// samePackageFixture is the index shape the Go extractor produces for one
// package split across files, plus the neighbours that make each same-package
// rule fail loudly if it is loosened:
//
//   - internal/other declares helperB too, so a resolver that ignored the
//     directory would link the wrong one;
//   - svc_ext_test.go is `package svc_test` in the SAME directory, so a resolver
//     that ignored the package clause would let it reach svc's unexported
//     helperB;
//   - (*Client).stopB shares a name with (*Server).stopB, so a receiver-self
//     resolver that ignored the receiver type would link both;
//   - platform has two build-tag variants, which must both be linked.
type samePackageFixture struct {
	db *sql.DB

	runA, localHelper, helperB      int64
	platformLinux, platformDarwin   int64
	start, serverStopB, clientStopB int64
	useExt, extShared, otherHelperB int64
	aFile, bFile                    int64
}

func newSamePackageFixture(t *testing.T) *samePackageFixture {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "samepkg.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	f := &samePackageFixture{db: db}

	node := func(file int64, p string, n Node) int64 {
		n.Language = "go"
		return insertTestNode(t, db, file, p, n)
	}
	goFile := func(p, pkg string) int64 {
		id := insertLangFile(t, db, p, "go")
		node(id, p, Node{Kind: KindPackage, Name: pkg})
		return id
	}

	const a, b = "internal/svc/a.go", "internal/svc/b.go"
	f.aFile = goFile(a, "svc")
	f.runA = node(f.aFile, a, Node{Kind: KindFunction, Name: "runA", Signature: "func runA()"})
	f.localHelper = node(f.aFile, a, Node{Kind: KindFunction, Name: "localHelper", Signature: "func localHelper()"})
	f.start = node(f.aFile, a, Node{
		Kind: KindMethod, Name: "Start", Qualified: "(*Server).Start",
		Signature: "func (s *Server) Start()",
	})

	f.bFile = goFile(b, "svc")
	f.helperB = node(f.bFile, b, Node{Kind: KindFunction, Name: "helperB", Signature: "func helperB()"})
	f.serverStopB = node(f.bFile, b, Node{
		Kind: KindMethod, Name: "stopB", Qualified: "(*Server).stopB",
		Signature: "func (s *Server) stopB()",
	})
	f.clientStopB = node(f.bFile, b, Node{
		Kind: KindMethod, Name: "stopB", Qualified: "(*Client).stopB",
		Signature: "func (c *Client) stopB()",
	})

	linux := goFile("internal/svc/platform_linux.go", "svc")
	f.platformLinux = node(linux, "internal/svc/platform_linux.go", Node{Kind: KindFunction, Name: "platform"})
	darwin := goFile("internal/svc/platform_darwin.go", "svc")
	f.platformDarwin = node(darwin, "internal/svc/platform_darwin.go", Node{Kind: KindFunction, Name: "platform"})

	const ext, ext2 = "internal/svc/svc_ext_test.go", "internal/svc/svc_ext2_test.go"
	extFile := goFile(ext, "svc_test")
	f.useExt = node(extFile, ext, Node{Kind: KindFunction, Name: "useExt"})
	ext2File := goFile(ext2, "svc_test")
	f.extShared = node(ext2File, ext2, Node{Kind: KindFunction, Name: "extShared"})

	other := goFile("internal/other/other.go", "other")
	f.otherHelperB = node(other, "internal/other/other.go", Node{Kind: KindFunction, Name: "helperB"})

	site := func(file, enclosing int64, qualifier, callee string) {
		insertSite(t, db, file, enclosing, "go", CallSiteCall, qualifier, callee)
	}
	site(f.aFile, f.runA, "", "helperB")     // cross-file, same package: edge
	site(f.aFile, f.runA, "", "helperB")     // repeat of that edge
	site(f.aFile, f.runA, "", "localHelper") // same file: the extractor's edge
	site(f.aFile, f.runA, "", "len")         // builtin: no function of the package
	site(f.aFile, f.runA, "", "platform")    // two build-tag variants: two edges, one site
	site(f.aFile, f.start, "s", "stopB")     // receiver-self: (*Server).stopB only
	site(f.aFile, f.start, "x", "stopB")     // a local receiver: unresolved
	site(f.aFile, f.start, "s", "promoted")  // receiver-self with no such method: unresolved
	site(extFile, f.useExt, "", "helperB")   // external test package: cannot reach svc
	site(extFile, f.useExt, "", "extShared") // its own package across files: edge
	site(f.aFile, 0, "", "helperB")          // package-level initialiser: no caller node
	return f
}

func (f *samePackageFixture) resolve(t *testing.T) {
	t.Helper()
	if err := (&Indexer{db: f.db}).resolveCalls(context.Background()); err != nil {
		t.Fatalf("resolveCalls: %v", err)
	}
}

func (f *samePackageFixture) edge(t *testing.T, from, to int64) (int, float64) {
	t.Helper()
	var n int
	var conf sql.NullFloat64
	if err := f.db.QueryRow(
		`SELECT COUNT(*), MAX(confidence) FROM topology_edges WHERE from_id=? AND to_id=? AND kind=? AND source=?`,
		from, to, string(EdgeCalls), callResolverSource).Scan(&n, &conf); err != nil {
		t.Fatal(err)
	}
	return n, conf.Float64
}

func TestResolveCalls_BareCallLinksTheSamePackageFunctionInASiblingFile(t *testing.T) {
	f := newSamePackageFixture(t)
	f.resolve(t)

	n, conf := f.edge(t, f.runA, f.helperB)
	if n != 1 {
		t.Fatalf("runA→helperB edges = %d, want 1", n)
	}
	if conf != samePackageCallConfidence {
		t.Errorf("same-package edge confidence = %v, want %v", conf, samePackageCallConfidence)
	}
	if n, _ := f.edge(t, f.runA, f.otherHelperB); n != 0 {
		t.Errorf("runA→other.helperB edges = %d, want 0 — a different directory is a different package", n)
	}
}

func TestResolveCalls_BareCallRespectsThePackageClause(t *testing.T) {
	f := newSamePackageFixture(t)
	f.resolve(t)

	if n, _ := f.edge(t, f.useExt, f.helperB); n != 0 {
		t.Errorf("svc_test useExt→svc helperB edges = %d, want 0 — an external test package cannot call it bare", n)
	}
	// Positive control: the clause check must not refuse the external test
	// package's own cross-file calls.
	if n, _ := f.edge(t, f.useExt, f.extShared); n != 1 {
		t.Errorf("useExt→extShared edges = %d, want 1", n)
	}
}

func TestResolveCalls_BareCallLinksEveryBuildTagVariant(t *testing.T) {
	f := newSamePackageFixture(t)
	f.resolve(t)

	for name, id := range map[string]int64{"linux": f.platformLinux, "darwin": f.platformDarwin} {
		if n, _ := f.edge(t, f.runA, id); n != 1 {
			t.Errorf("runA→platform (%s) edges = %d, want 1", name, n)
		}
	}
}

func TestResolveCalls_SameFileBareCallIsLeftToTheExtractor(t *testing.T) {
	f := newSamePackageFixture(t)
	f.resolve(t)

	if n, _ := f.edge(t, f.runA, f.localHelper); n != 0 {
		t.Errorf("resolver runA→localHelper edges = %d, want 0", n)
	}
}

func TestResolveCalls_ReceiverSelfCallLinksOnlyTheReceiversOwnType(t *testing.T) {
	f := newSamePackageFixture(t)
	f.resolve(t)

	if n, _ := f.edge(t, f.start, f.serverStopB); n != 1 {
		t.Errorf("(*Server).Start→(*Server).stopB edges = %d, want 1", n)
	}
	if n, _ := f.edge(t, f.start, f.clientStopB); n != 0 {
		t.Errorf("(*Server).Start→(*Client).stopB edges = %d, want 0 — same name, different receiver type", n)
	}
	var fromStart int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM topology_edges WHERE from_id=? AND source=?`,
		f.start, callResolverSource).Scan(&fromStart); err != nil {
		t.Fatal(err)
	}
	if fromStart != 1 {
		t.Errorf("edges from Start = %d, want 1 — x.stopB() on a local and s.promoted() must stay unresolved", fromStart)
	}
}

// TestResolveCalls_SamePackageCensus pins every bucket the fixture exercises, so
// a site moving between buckets shows up as a specific number, not just a sum.
func TestResolveCalls_SamePackageCensus(t *testing.T) {
	f := newSamePackageFixture(t)
	f.resolve(t)

	want := map[string]int{
		metaCallSites:            11,
		metaCallNoCallerNode:     1,
		metaCallQualifiedSites:   3,
		metaCallResolved:         4, // helperB, platform (one site, two edges), s.stopB, extShared
		metaCallResolvedSamePkg:  3,
		metaCallResolvedRecvSelf: 1,
		metaCallRepeatOfEdge:     1,
		metaCallSameFile:         1,
		metaCallBareUnmatched:    2, // len, and helperB from the external test package
		metaCallUnresolvedRecv:   2, // x.stopB, s.promoted
	}
	for key, n := range want {
		if got := callMeta(t, f.db, key); got != n {
			t.Errorf("%s = %d, want %d", key, got, n)
		}
	}
	var edges int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM topology_edges WHERE source=?`, callResolverSource).Scan(&edges); err != nil {
		t.Fatal(err)
	}
	if edges != 5 {
		t.Errorf("resolver edges = %d, want 5 (four resolved sites, platform twice)", edges)
	}
}

// TestResolveCalls_ScopedPassReachesAnUnchangedSiblingCaller is the lifecycle
// case the same-package tiers add: a function appears in b.go, and a caller in
// a.go — which did not change — must gain its edge on the SCOPED pass for b.go,
// without a full rebuild.
func TestResolveCalls_ScopedPassReachesAnUnchangedSiblingCaller(t *testing.T) {
	f := newSamePackageFixture(t)
	insertSite(t, f.db, f.aFile, f.runA, "go", CallSiteCall, "", "addedLater")
	f.resolve(t)
	if got := callMeta(t, f.db, metaCallBareUnmatched); got != 3 {
		t.Fatalf("before the addition, bare unmatched = %d, want 3", got)
	}

	added := insertTestNode(t, f.db, f.bFile, "internal/svc/b.go",
		Node{Kind: KindFunction, Name: "addedLater", Language: "go"})
	changes := indexChanges{paths: map[string]struct{}{"internal/svc/b.go": {}}}
	if err := (&Indexer{db: f.db}).resolveCallsContext(context.Background(), rebuildScoped, changes); err != nil {
		t.Fatalf("scoped resolve: %v", err)
	}
	if n, _ := f.edge(t, f.runA, added); n != 1 {
		t.Errorf("unchanged a.go runA→new b.go addedLater edges = %d, want 1 after a scoped pass for b.go", n)
	}
	// The sibling's existing edges must survive its re-resolution, once each.
	if n, _ := f.edge(t, f.runA, f.helperB); n != 1 {
		t.Errorf("runA→helperB edges after the scoped pass = %d, want 1", n)
	}
	if n, _ := f.edge(t, f.start, f.serverStopB); n != 1 {
		t.Errorf("Start→stopB edges after the scoped pass = %d, want 1", n)
	}
}

func TestReceiverVarAndBase(t *testing.T) {
	for sig, want := range map[string]string{
		"func (idx *Indexer) resolveCalls(ctx context.Context) error": "idx",
		"func (s Server) Run()":    "s",
		"func (t *Tree) Walk()":    "t", // the extractor drops type parameters
		"func (*Indexer) anon()":   "",
		"func (_ *Indexer) anon()": "",
		"func plain()":             "",
	} {
		if got := receiverVar(sig); got != want {
			t.Errorf("receiverVar(%q) = %q, want %q", sig, got, want)
		}
	}
	for q, want := range map[string]string{
		"(*Indexer).resolveCalls": "Indexer",
		"(Server).Run":            "Server",
		"resolveCalls":            "",
		"(_).x":                   "",
	} {
		got, _ := receiverBase(q)
		if got != want {
			t.Errorf("receiverBase(%q) = %q, want %q", q, got, want)
		}
	}
}
