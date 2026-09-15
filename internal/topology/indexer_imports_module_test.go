package topology

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// indexer_imports_module_test.go covers the module-path resolution that turns
// the import resolver's segment-count heuristic into an exact test for Go.
//
// The cases that matter are the ones the heuristic could not decide: a
// third-party path whose tail names a local package, a stdlib path long enough
// to pass a segment floor, and a one-segment dotless module path that a floor
// must refuse because it cannot be told from a stdlib root. All three are
// decided here by what go.mod says, not by counting.

func TestParseModulePath(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"plain", "module github.com/plumbkit/plumb\n\ngo 1.26\n", "github.com/plumbkit/plumb"},
		{"leading comment block", "// SPDX-License-Identifier: MIT\n// vim: ft=gomod\n\nmodule example.com/m\n", "example.com/m"},
		{"inline comment", "module example.com/m // the module, obviously\n", "example.com/m"},
		{"quoted", "module \"example.com/m\"\n", "example.com/m"},
		{"tab separated", "module\texample.com/m\n", "example.com/m"},
		{"one dotless segment", "module myapp\n", "myapp"},
		{"no trailing newline", "module example.com/m", "example.com/m"},
		{"crlf", "module example.com/m\r\n", "example.com/m"},
		// The FACTORED form, which is legal and which an earlier version of this
		// parser read as the module path "(". That is the shape of bug this whole
		// file has to be paranoid about: "(" is non-empty, so resolution becomes
		// DECIDED, and nothing starts with "(" — every Go import in the repository
		// is then refused. Measured on a real index: 77,450 import edges to zero.
		{"factored form", "module (\n\texample.com/m\n)\n", "example.com/m"},
		{"factored form with comments", "module ( // why\n\t// a note\n\texample.com/m\n)\n", "example.com/m"},
		{"factored form, empty block declares nothing", "module (\n)\n", ""},
		// A go.work has no module directive, and neither does a truncated file.
		// Both must read as "no module here" rather than as a wrong one.
		{"go.work", "go 1.26\n\nuse (\n\t./a\n\t./b\n)\n", ""},
		{"empty", "", ""},
		// "module" must be a directive, not a prefix of some other token. The first
		// is a bare line of the kind that sits inside a require block; the second is
		// a directive whose NAME merely starts with "module", which is what the
		// isSpace guard after CutPrefix exists to reject.
		{"bare require-block line", "modulefoo/bar v1.2.3\n", ""},
		{"modulefoo directive-lookalike", "modulefoo example.com/m\n", ""},
		// Anything not shaped like a module path must land on "", because a
		// non-empty wrong answer is catastrophic while an empty one is merely the
		// old behaviour. These are the spellings that would otherwise survive.
		{"stray paren", "module (oops\n", ""},
		{"two operands", "module example.com/m extra\n", ""},
		{"trailing slash", "module example.com/m/\n", ""},
		{"leading slash", "module /example.com/m\n", ""},
		{"dot segment", "module example.com/./m\n", ""},
		{"parent segment", "module example.com/../m\n", ""},
		// Not an empty segment: `//` opens a comment in go.mod, so this declares
		// `example.com` and comments out the rest. Verified against the toolchain
		// (`go list -m` prints example.com), because the tempting assumption — that
		// this is a malformed path to refuse — would make the parser disagree with
		// the compiler about which module a repository is.
		{"double slash is a comment, not an empty segment", "module example.com//m\n", "example.com"},
		{"bare module keyword", "module\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseModulePath(tc.src); got != tc.want {
				t.Errorf("parseModulePath(%q) = %q, want %q", tc.src, got, tc.want)
			}
		})
	}
}

// TestResolveGoImport_ThreeOutcomes pins the distinction the whole change rests
// on: "no module claims this" and "no module is declared here" are different
// answers, and collapsing them is how a third-party import reaches a local
// directory that shares its tail.
func TestResolveGoImport_ThreeOutcomes(t *testing.T) {
	mods := goModuleSet{complete: true, mods: []goModule{
		{dir: "sub", path: "example.com/m/sub"}, // nested, longest first
		{dir: ".", path: "example.com/m"},
	}}
	cases := []struct {
		name        string
		qualified   string
		mods        goModuleSet
		wantDir     string
		wantDecided bool
	}{
		{"module-internal import", "example.com/m/internal/stats", mods, "internal/stats", true},
		{"the module root itself", "example.com/m", mods, ".", true},
		// The nested module owns its own subtree, and its directory — not the
		// parent's — is what the remainder hangs off.
		{"nested module wins over its parent", "example.com/m/sub/pkg", mods, "sub/pkg", true},
		{"nested module root", "example.com/m/sub", mods, "sub", true},
		// Decided-and-refused. Each of these is a case the segment-count
		// heuristic could not settle; none of them may fall through.
		{"stdlib, one segment", "strings", mods, "", true},
		{"stdlib, two segments", "net/http", mods, "", true},
		{"stdlib, three segments", "net/http/httptest", mods, "", true},
		{"third-party sharing a tail", "github.com/boltdb/store", mods, "", true},
		{"a module path this repo does not declare", "example.org/other/pkg", mods, "", true},
		// A near-miss on the module path itself: a prefix test without the
		// separator would claim this and hand back "ools" as a directory.
		{"module path is a string prefix but not a path prefix", "example.com/mtools/x", mods, "", true},
		// Undecided: no module declared, so the suffix matcher still answers.
		{"no modules known", "example.com/m/internal/stats", goModuleSet{complete: true}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, decided := resolveGoImport(tc.qualified, tc.mods)
			if dir != tc.wantDir || decided != tc.wantDecided {
				t.Errorf("resolveGoImport(%q) = (%q, %v), want (%q, %v)",
					tc.qualified, dir, decided, tc.wantDir, tc.wantDecided)
			}
		})
	}
}

// TestResolveGoImport_OneSegmentModuleIsResolvable is the case PLAN-386 had to
// document as unfixable: `module myapp` spends one segment, so no count can
// tell myapp/stats from a stdlib root. go.mod settles it outright.
func TestResolveGoImport_OneSegmentModuleIsResolvable(t *testing.T) {
	mods := goModuleSet{complete: true, mods: []goModule{{dir: ".", path: "myapp"}}}
	if dir, decided := resolveGoImport("myapp/stats", mods); !decided || dir != "stats" {
		t.Errorf("resolveGoImport(\"myapp/stats\") = (%q, %v), want (\"stats\", true) — a "+
			"dotless single-segment module path is legal Go and its packages are local", dir, decided)
	}
	// And the shape it is indistinguishable from by counting alone must still go.
	if dir, decided := resolveGoImport("net/http", mods); !decided || dir != "" {
		t.Errorf("resolveGoImport(\"net/http\") = (%q, %v), want (\"\", true)", dir, decided)
	}
}

// TestImportTargetDir_GoDoesNotFallBack is the load-bearing wiring test: once
// the module set has decided, a Go import must not reach the suffix matcher.
// The map here is rigged so that falling through would SUCCEED — a local store/
// that github.com/boltdb/store would reach, and a local httptest/ that
// net/http/httptest would reach. A pass therefore means the fallback was not
// consulted, not merely that it found nothing.
func TestImportTargetDir_GoDoesNotFallBack(t *testing.T) {
	pkgs := map[string][]int64{
		"store":          {1},
		"httptest":       {2},
		"internal/stats": {3},
	}
	mods := goModuleSet{complete: true, mods: []goModule{{dir: ".", path: "example.com/m"}}}

	for _, q := range []string{"github.com/boltdb/store", "net/http/httptest"} {
		if dir, ok := importTargetDir(q, "go", pkgs, mods); ok {
			t.Errorf("importTargetDir(%q, go) reached %q through the suffix matcher; a Go "+
				"import the module set refused must not fall back", q, dir)
		}
	}
	if dir, ok := importTargetDir("example.com/m/internal/stats", "go", pkgs, mods); !ok || dir != "internal/stats" {
		t.Errorf("importTargetDir(module-internal) = (%q, %v), want (\"internal/stats\", true)", dir, ok)
	}
	// A module-claimed import pointing at a directory holding no indexed package
	// resolves to nothing — but it still must not fall back and find a tail.
	if dir, ok := importTargetDir("example.com/m/store/nope", "go", pkgs, mods); ok {
		t.Errorf("importTargetDir(unindexed module path) = (%q, true), want no match", dir)
	}
}

// TestImportTargetDir_NonGoStillUsesSuffixMatching guards the other side: the
// module rule is Go-only, and a language with no manifest this pass reads must
// keep the behaviour it has. Asserted with a Go module present, because that is
// the state in which a language check could be skipped by accident.
func TestImportTargetDir_NonGoStillUsesSuffixMatching(t *testing.T) {
	pkgs := map[string][]int64{"lib/format": {1}}
	mods := goModuleSet{complete: true, mods: []goModule{{dir: ".", path: "example.com/m"}}}

	if dir, ok := importTargetDir("./lib/format", "typescript", pkgs, mods); !ok || dir != "lib/format" {
		t.Errorf("importTargetDir(relative TS import) = (%q, %v), want (\"lib/format\", true)", dir, ok)
	}
	// The same path as a GO import is refused, since no module claims it. The
	// pair is the point: identical input, different answer, decided by language.
	if dir, ok := importTargetDir("./lib/format", "go", pkgs, mods); ok {
		t.Errorf("importTargetDir(same path as go) = (%q, true), want no match", dir)
	}
}

// TestGoModulesInIndex_OrdersByLongestModulePath drives the real function over a
// real index, and pins the ordering with a fixture where getting it wrong gives
// a WRONG ANSWER rather than the same one.
//
// The obvious fixture does not test the sort at all: with a module
// example.com/m/sub living at sub/, both orders resolve example.com/m/sub/pkg
// to "sub/pkg", because path.Join(".", "sub/pkg") and path.Join("sub", "pkg")
// are the same string. An earlier version of this test used exactly that and
// proved nothing — both sort mutants survived it.
//
// So the nested module lives at tools/, which is what `replace` produces and
// what makes the two orders disagree: longest-first gives tools/pkg, and
// parent-first gives sub/pkg, a directory nothing put there.
func TestGoModulesInIndex_OrdersByLongestModulePath(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	writeGoMod(t, ws, ".", "module example.com/m\n\ngo 1.26\n")
	writeGoMod(t, ws, "tools", "module example.com/m/sub\n\ngo 1.26\n")

	db, err := openDB(filepath.Join(ws, "index.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()
	// The index holds the PATHS; the contents are read from disk. Insert them in
	// the order that would leave the parent first if nothing sorted.
	insertTestFile(t, db, "go.mod")
	insertTestFile(t, db, "tools/go.mod")

	mods := goModulesInIndex(ctx, db, ws)
	if len(mods.mods) != 2 {
		t.Fatalf("goModulesInIndex returned %d modules, want 2: %v", len(mods.mods), mods.mods)
	}
	if !mods.complete {
		t.Error("both go.mod files parsed, so the set must be complete")
	}
	if mods.mods[0].path != "example.com/m/sub" {
		t.Errorf("longest module path must sort first; got %q then %q",
			mods.mods[0].path, mods.mods[1].path)
	}
	// The answer the order decides, which is the reason the order matters.
	if dir, decided := resolveGoImport("example.com/m/sub/pkg", mods); !decided || dir != "tools/pkg" {
		t.Errorf("resolveGoImport = (%q, %v), want (\"tools/pkg\", true) — the nested "+
			"module must claim its own subtree before its parent maps it elsewhere", dir, decided)
	}
	// And the parent still owns everything the nested module does not.
	if dir, _ := resolveGoImport("example.com/m/other", mods); dir != "other" {
		t.Errorf("parent module: got %q, want \"other\"", dir)
	}
}

// TestGoModulesInIndex_SkipsWhatItCannotBelieve pins the safe failure: a go.mod
// the index lists but whose directive this parser will not accept must leave NO
// module behind, so resolution stays undecided and the suffix matcher answers.
// A garbage entry here is the catastrophic case — decided, claiming nothing,
// refusing every Go import in the repository.
func TestGoModulesInIndex_SkipsWhatItCannotBelieve(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	writeGoMod(t, ws, ".", "module (oops\n")       // not a module path
	writeGoMod(t, ws, "b", "go 1.26\n\nuse ./x\n") // a go.work in disguise: no directive

	db, err := openDB(filepath.Join(ws, "index.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()
	insertTestFile(t, db, "go.mod")
	insertTestFile(t, db, "b/go.mod")
	// A path the index lists but no file backs, which is what a delete between
	// the walk and the link pass looks like.
	insertTestFile(t, db, "gone/go.mod")

	mods := goModulesInIndex(ctx, db, ws)
	if len(mods.mods) != 0 {
		t.Fatalf("goModulesInIndex returned %v; an unbelievable directive must leave "+
			"NO module, or every Go import in the repository is refused", mods.mods)
	}
	if mods.complete {
		t.Error("a go.mod the parser declined must mark the set INCOMPLETE; a complete " +
			"empty set would licence refusing every Go import in the workspace")
	}
}

// TestResolverSurfaceFingerprint_TracksTheModuleSet is the invalidation half,
// and it exists because go.mod is the one resolver input that produces no nodes.
//
// The fingerprint decides whether a derived rebuild happens at all. It hashes
// node identities, and no extractor handles .mod — go.mod's topology_files row
// carries a path and nothing else. So without the module set folded in, two
// things were reproducible on a live store: `go mod init` never took effect (the
// false positives this whole change exists to kill survived every rebuild), and
// `go mod edit -module` left every edge resolved under a module path the
// repository no longer declared.
func TestResolverSurfaceFingerprint_TracksTheModuleSet(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	db, err := openDB(filepath.Join(ws, "index.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()
	insertTestFile(t, db, "a/a.go")

	// No go.mod yet: the state a repository is in before `go mod init`.
	before, err := resolverSurfaceFingerprint(ctx, db, ws)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}

	// `go mod init example.com/m`, and the walk picks the file up.
	writeGoMod(t, ws, ".", "module example.com/m\n")
	insertTestFile(t, db, "go.mod")
	initialised, err := resolverSurfaceFingerprint(ctx, db, ws)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	if initialised == before {
		t.Error("declaring a module left the resolver fingerprint unchanged, so no " +
			"rebuild is scheduled and every import keeps the answer it had before go.mod existed")
	}

	// `go mod edit -module example.com/renamed`: same file, same row, new answer.
	writeGoMod(t, ws, ".", "module example.com/renamed\n")
	renamed, err := resolverSurfaceFingerprint(ctx, db, ws)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	if renamed == initialised {
		t.Error("renaming the module left the resolver fingerprint unchanged, so every " +
			"edge stays resolved under a module path the repository no longer declares")
	}
}

// TestResolveGoImport_IncompleteSetWithdrawsTheRefusal is the recall regression
// an independent review found in the first version of this change, reproduced
// live before it was fixed.
//
// With one go.mod parsed and another declined, the set was silently short a
// module. An import into the UNPARSED module then matched nothing — and
// "matched nothing" was read as "is not local", so the edge was refused and
// lost, where before this change the suffix matcher would have found it.
//
// The refusal is only sound when the module set is known to be whole. An
// incomplete set still resolves what it can, and is undecided about the rest.
func TestResolveGoImport_IncompleteSetWithdrawsTheRefusal(t *testing.T) {
	known := goModule{dir: ".", path: "example.com/m"}

	complete := goModuleSet{complete: true, mods: []goModule{known}}
	partial := goModuleSet{complete: false, mods: []goModule{known}}

	// What a resolved module claims is unaffected by the gap elsewhere.
	if dir, decided := resolveGoImport("example.com/m/a", partial); !decided || dir != "a" {
		t.Errorf("a module that DID parse must still resolve its own imports; got (%q, %v)", dir, decided)
	}
	// What nothing claims is the difference, and it is the whole point.
	if dir, decided := resolveGoImport("example.com/other/pkg", complete); !decided || dir != "" {
		t.Errorf("complete set: an unclaimed import must be refused; got (%q, %v)", dir, decided)
	}
	if dir, decided := resolveGoImport("example.com/other/pkg", partial); decided {
		t.Errorf("incomplete set: an unclaimed import must stay UNDECIDED so the suffix "+
			"matcher answers; got (%q, decided) — refusing here turns \"a module I could "+
			"not read\" into \"no local package exists\"", dir)
	}
	// And the dispatcher carries it through: with the gap, a Go import falls back
	// and can reach a directory again, exactly as it did before this change.
	pkgs := map[string][]int64{"other/pkg": {1}}
	if dir, ok := importTargetDir("example.com/other/pkg", "go", pkgs, partial); !ok || dir != "other/pkg" {
		t.Errorf("importTargetDir with an incomplete set = (%q, %v), want (\"other/pkg\", true)", dir, ok)
	}
	if _, ok := importTargetDir("example.com/other/pkg", "go", pkgs, complete); ok {
		t.Error("importTargetDir with a complete set must refuse, not fall back")
	}
}

func writeGoMod(t *testing.T, ws, dir, content string) {
	t.Helper()
	abs := filepath.Join(ws, filepath.FromSlash(dir))
	if err := os.MkdirAll(abs, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(abs, "go.mod"), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s/go.mod: %v", dir, err)
	}
}
