package tools_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/sqlitex"
	"github.com/plumbkit/plumb/internal/topology"
	goext "github.com/plumbkit/plumb/internal/topology/extractors/golang"
	"github.com/plumbkit/plumb/internal/topology/extractors/treesitter"
)

// These two fixtures are the two directions the call-graph gate can fail in, and
// they are here rather than in a unit test because both need REAL extraction:
// the gate reads what an extractor put in the index, so a hand-built index would
// be testing the test's idea of C# rather than plumb's.

func writeFixtureFile(t *testing.T, ws, rel, src string) {
	t.Helper()
	full := filepath.Join(ws, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
}

// openCallGraphStore indexes ws with the Go and C# extractors and waits until
// the indexer has settled — specifically until the call-SITES table has stopped
// growing, because the resolver pass runs after the whole file batch.
func openCallGraphStore(t *testing.T, ws string, wantFiles int) (*topology.Store, *sql.DB) {
	t.Helper()
	s, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
		[]topology.Extractor{goext.New(), treesitter.NewCSharp()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	db, err := sqlitex.Open(topology.DBPath(ws), sqlitex.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var indexed int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM topology_files WHERE content_hash <> ''`).Scan(&indexed); err == nil && indexed >= wantFiles {
			var meta int
			if err := db.QueryRow(
				`SELECT COUNT(*) FROM topology_meta WHERE key LIKE 'callgraph.%'`).Scan(&meta); err == nil && meta > 0 {
				return s, db
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the call-graph fixture to be indexed")
	return nil, nil
}

func countInt(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// TestCallGraphGating_SmallPureGoWorkspaceIsAdmitted catches OVER-EAGER gating.
// One file, package main, whose only calls are to the standard library and to a
// local function: zero cross-file call edges are derivable from it. It must
// still be ADMITTED and get a labelled real answer — the shipped bug this
// replaces refused exactly this workspace because it had no edges to fold.
func TestCallGraphGating_SmallPureGoWorkspaceIsAdmitted(t *testing.T) {
	ws := t.TempDir()
	writeFixtureFile(t, ws, "main.go", `package main

import "fmt"

func main() { greet() }

func greet() { fmt.Println("hi") }
`)
	s, db := openCallGraphStore(t, ws, 1)

	a, err := s.AdmitCallGraph(context.Background(), topology.CallGraphSubject{Language: "go", Path: "main.go"})
	if err != nil {
		t.Fatalf("AdmitCallGraph: %v", err)
	}
	if !a.Admitted {
		t.Fatalf("a one-file pure-Go workspace was refused: %q", a.Refusal)
	}
	if a.Refusal != "" {
		t.Errorf("an admitted workspace carries a refusal: %q", a.Refusal)
	}
	if a.IntraFileCalls == 0 {
		t.Error("no intra-file call edges reported; main→greet is a real edge and the answer must carry it")
	}
	if !strings.Contains(a.EmptyResultNote, "no cross-file call edges") {
		t.Errorf("the empty-result label is missing: %q", a.EmptyResultNote)
	}
	if strings.Contains(a.EmptyResultNote, "not available for") {
		t.Errorf("the empty-result label reads as a refusal: %q", a.EmptyResultNote)
	}
	if got := countInt(t, db, `SELECT COUNT(*) FROM topology_edges WHERE source='call-resolver'`); got != 0 {
		t.Errorf("resolver edges = %d, want 0 — the fixture has no cross-file call to resolve", got)
	}
	if got := countInt(t, db, `SELECT COUNT(*) FROM topology_call_sites`); got == 0 {
		t.Error("no call sites recorded; the admission passed over an empty table, which proves nothing")
	}
}

const csharpAlpha = `namespace Alpha;

using System;

public class Widget {
    public void Build() { Console.WriteLine("alpha"); }
}
`

const csharpBeta = `namespace Beta;

using Alpha;

public class Runner {
    public void Run() { var w = new Widget(); w.Build(); }
}
`

const csharpGamma = `namespace Gamma;

using Beta;

public class Entry {
    public void Main() { var r = new Runner(); r.Run(); }
}
`

const goGen = `package main

import "example.com/tool/internal/gen"

func main() { gen.Generate() }
`

const goGenLib = `package gen

func Generate() {}
`

// buildPolyglotFixture is three real C# namespaces plus a real, non-vendored,
// non-testdata Go file. withGo controls whether the Go file exists, which is how
// assertion 4 below checks that one stray Go file cannot change the C# answer.
func buildPolyglotFixture(t *testing.T, withGo bool) (*topology.Store, *sql.DB) {
	t.Helper()
	ws := t.TempDir()
	writeFixtureFile(t, ws, "src/Alpha/Widget.cs", csharpAlpha)
	writeFixtureFile(t, ws, "src/Beta/Runner.cs", csharpBeta)
	writeFixtureFile(t, ws, "src/Gamma/Entry.cs", csharpGamma)
	want := 3
	if withGo {
		writeFixtureFile(t, ws, "scripts/gen.go", goGen)
		writeFixtureFile(t, ws, "internal/gen/gen.go", goGenLib)
		want = 5
	}
	return openCallGraphStore(t, ws, want)
}

// TestCallGraphGating_PolyglotDoesNotLeakAcrossLanguages catches UNDER-EAGER
// gating and the one-stray-file spoof in a single fixture. All four assertions
// are the same failure seen from four sides: a workspace-wide boolean has only
// two outcomes, so whichever it picks is wrong for one half of a polyglot repo.
func TestCallGraphGating_PolyglotDoesNotLeakAcrossLanguages(t *testing.T) {
	s, db := buildPolyglotFixture(t, true)
	ctx := context.Background()

	// 1. The C# subject refuses, with the no-adapter wording, and says nothing
	//    about any C# symbol being caller-free or unreachable.
	cs, err := s.AdmitCallGraph(ctx, topology.CallGraphSubject{Language: "csharp", Path: "src/Beta/Runner.cs"})
	if err != nil {
		t.Fatal(err)
	}
	if cs.Admitted {
		t.Fatal("a C# subject was admitted to function-level call answers")
	}
	if !strings.Contains(cs.Refusal, "no csharp language server adapter") {
		t.Errorf("C# refusal is not the no-adapter variant: %q", cs.Refusal)
	}
	for _, forbidden := range []string{"unreachable", "caller-free", "no callers", "Widget", "Runner"} {
		if strings.Contains(cs.Refusal, forbidden) {
			t.Errorf("C# refusal mentions %q; a refusal must not make claims about C# symbols: %q", forbidden, cs.Refusal)
		}
	}

	// 2. The Go subject gets a normal answer, scoped, naming C# as out of scope
	//    with its file count.
	gos, err := s.AdmitCallGraph(ctx, topology.CallGraphSubject{Language: "go", Path: "scripts/gen.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !gos.Admitted {
		t.Fatalf("the Go subject was refused in a mostly-C# workspace: %q", gos.Refusal)
	}
	if gos.OutOfScope["csharp"] != 3 {
		t.Errorf("out-of-scope csharp files = %d, want 3", gos.OutOfScope["csharp"])
	}
	for _, want := range []string{"csharp", "out of scope", "not caller-free"} {
		if !strings.Contains(gos.ScopeNote, want) {
			t.Errorf("Go scope note missing %q: %q", want, gos.ScopeNote)
		}
	}

	// 3. The Go answer's edges touch no non-Go node at all.
	if got := countInt(t, db, `
		SELECT COUNT(*) FROM topology_edges e
		  JOIN topology_nodes a ON a.id = e.from_id
		  JOIN topology_nodes b ON b.id = e.to_id
		 WHERE e.source = 'call-resolver' AND (a.language <> 'go' OR b.language <> 'go')`); got != 0 {
		t.Errorf("%d resolver edges touch a non-Go node", got)
	}
	if got := countInt(t, db, `SELECT COUNT(*) FROM topology_edges WHERE source='call-resolver'`); got == 0 {
		t.Fatal("no resolver edges at all; assertion 3 passed vacuously")
	}
	if got := countInt(t, db, `SELECT COUNT(*) FROM topology_nodes WHERE kind='package' AND language='csharp'`); got == 0 {
		t.Fatal("the C# extractor emitted no package node; this fixture cannot test the language term")
	}

	// 4. Deleting the stray Go file leaves the C# result byte-identical.
	sNoGo, _ := buildPolyglotFixture(t, false)
	csNoGo, err := sNoGo.AdmitCallGraph(ctx, topology.CallGraphSubject{Language: "csharp", Path: "src/Beta/Runner.cs"})
	if err != nil {
		t.Fatal(err)
	}
	if csNoGo.Refusal != cs.Refusal {
		t.Errorf("removing scripts/gen.go changed the C# answer:\n with go: %q\n without: %q", cs.Refusal, csNoGo.Refusal)
	}
	if csNoGo.Admitted != cs.Admitted || csNoGo.ScopeNote != cs.ScopeNote {
		t.Error("removing the stray Go file changed the C# verdict or its scope note")
	}
}
