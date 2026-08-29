package topology

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// newLangIndex builds an index holding one package node per named language.
func newLangIndex(t *testing.T, langs ...string) *sql.DB {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "topology.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, l := range langs {
		p := "src/" + l + "/file." + l
		fileID := insertLangFile(t, db, p, l)
		insertTestNode(t, db, fileID, p, Node{Kind: KindPackage, Name: "P" + l, Language: l})
	}
	return db
}

func admit(t *testing.T, db *sql.DB, lang string) CallGraphAdmission {
	t.Helper()
	a, err := AdmitCallGraph(context.Background(), db, CallGraphSubject{Language: lang})
	if err != nil {
		t.Fatalf("AdmitCallGraph(%s): %v", lang, err)
	}
	return a
}

// TestAdmitCallGraph_UnsupportedLanguageRefusesEvenWithItsOwnPackageNode is the
// regression guard for the four languages that DO emit package nodes. They must
// refuse because their language is not in the supported set — not because the Go
// signal happened to be missing, which is why the assertion is on the refusal
// text and not on a "has Go" boolean.
func TestAdmitCallGraph_UnsupportedLanguageRefusesEvenWithItsOwnPackageNode(t *testing.T) {
	for _, lang := range []string{"csharp", "php", "elixir", "scala"} {
		t.Run(lang, func(t *testing.T) {
			db := newLangIndex(t, lang)
			a := admit(t, db, lang)
			if a.Admitted {
				t.Fatalf("%s was admitted; only languages in supportedCallGraphLanguages may be", lang)
			}
			if !a.HasPackageSignal {
				t.Fatalf("the fixture is wrong: %s has no package node, so this proves nothing about the language term", lang)
			}
			if !strings.Contains(a.Refusal, lang) {
				t.Errorf("refusal does not name the language: %q", a.Refusal)
			}
		})
	}
}

// TestAdmitCallGraph_RefusalSplitsOnAdapterAvailability pins the correction the
// coverage spike forced: what plumb can offer instead differs by whether a
// language server exists, and a refusal that promises find_references for a
// language with no adapter is worse than one that says there is no answer.
func TestAdmitCallGraph_RefusalSplitsOnAdapterAvailability(t *testing.T) {
	withAdapter := admit(t, newLangIndex(t, "java"), "java")
	without := admit(t, newLangIndex(t, "csharp"), "csharp")

	if !strings.Contains(withAdapter.Refusal, "through the language server") {
		t.Errorf("java refusal does not offer the LSP path: %q", withAdapter.Refusal)
	}
	if strings.Contains(withAdapter.Refusal, "no java language server adapter") {
		t.Errorf("java refusal claims there is no adapter, but jdtls is wired: %q", withAdapter.Refusal)
	}
	if !strings.Contains(without.Refusal, "no csharp language server adapter") {
		t.Errorf("csharp refusal does not say the adapter is missing: %q", without.Refusal)
	}
	if strings.Contains(without.Refusal, "through the language server") {
		t.Errorf("csharp refusal offers a language server plumb does not ship: %q", without.Refusal)
	}
	if withAdapter.Refusal == without.Refusal {
		t.Error("both refusals are identical; the adapter split is not happening")
	}
}

// TestAdmitCallGraph_NeverOffersPackageReachability pins the second correction:
// mode="reachability" is itself gated to the same language set, so pointing a
// refused subject at it would send them to a second refusal.
func TestAdmitCallGraph_NeverOffersPackageReachability(t *testing.T) {
	for _, lang := range []string{"csharp", "java", "ruby", "swift", "python"} {
		a := admit(t, newLangIndex(t, lang), lang)
		if strings.Contains(strings.ToLower(a.Refusal), "reachability") {
			t.Errorf("%s refusal offers reachability, which is Go-gated too: %q", lang, a.Refusal)
		}
	}
}

// TestAdmitCallGraph_FutureSupportPromisedOnlyWhereItIsReal keeps the refusal
// from growing a queue it cannot honour: the design does not fit swift, c, objc,
// ruby, dart or bash at all, and saying "tracked" for them would be a lie with a
// long half-life.
func TestAdmitCallGraph_FutureSupportPromisedOnlyWhereItIsReal(t *testing.T) {
	const promise = "can support this"
	for _, lang := range []string{"csharp", "php", "python", "rust"} {
		if a := admit(t, newLangIndex(t, lang), lang); !strings.Contains(a.Refusal, promise) {
			t.Errorf("%s is listed as supportable with work but its refusal makes no such note: %q", lang, a.Refusal)
		}
	}
	for _, lang := range []string{"swift", "c", "ruby", "dart", "bash", "java"} {
		if a := admit(t, newLangIndex(t, lang), lang); strings.Contains(a.Refusal, promise) {
			t.Errorf("%s refusal promises future support the design cannot deliver: %q", lang, a.Refusal)
		}
	}
}

// TestAdmitCallGraph_BothTermsAreRequiredAndPositive pins the gate's shape. A
// supported language with no package node is not admitted, and an unsupported
// language with one is not either — and neither verdict consults an edge count.
func TestAdmitCallGraph_BothTermsAreRequiredAndPositive(t *testing.T) {
	goNoPackages := newLangIndex(t, "csharp")
	if a := admit(t, goNoPackages, "go"); a.Admitted {
		t.Error("go was admitted with no Go package node in the index")
	}
	goWithPackages := newLangIndex(t, "go")
	a := admit(t, goWithPackages, "go")
	if !a.Admitted {
		t.Fatal("go with a package node was refused")
	}
	if a.Refusal != "" {
		t.Errorf("an admitted language carries a refusal: %q", a.Refusal)
	}
	// No cross-file edges exist in this fixture, and that must produce a labelled
	// answer rather than a refusal.
	if a.EmptyResultNote == "" {
		t.Error("an admitted language with no resolver edges carries no empty-result note")
	}
}

// TestAdmitCallGraph_ScopeNoteNamesOtherLanguagesAsOutOfScope is the polyglot
// label. A Go answer in a mixed repo must say what it did not look at, in the
// words "out of scope" — never as an absence of callers.
func TestAdmitCallGraph_ScopeNoteNamesOtherLanguagesAsOutOfScope(t *testing.T) {
	db := newLangIndex(t, "go", "csharp", "python")
	a := admit(t, db, "go")
	if !a.Admitted {
		t.Fatal("go refused in a polyglot workspace")
	}
	for _, want := range []string{"out of scope", "not caller-free", "csharp", "python"} {
		if !strings.Contains(a.ScopeNote, want) {
			t.Errorf("scope note missing %q: %q", want, a.ScopeNote)
		}
	}
	if a.OutOfScope["csharp"] != 1 || a.OutOfScope["python"] != 1 {
		t.Errorf("out-of-scope counts = %v, want one file each for csharp and python", a.OutOfScope)
	}
	if _, ok := a.OutOfScope["go"]; ok {
		t.Error("the admitted language is listed as out of scope of its own answer")
	}

	pureGo := newLangIndex(t, "go")
	if note := admit(t, pureGo, "go").ScopeNote; note != "" {
		t.Errorf("a single-language workspace carries a scope note: %q", note)
	}
}

// TestCallGraphSubject_LanguageComesFromTheIndex pins the derivation half of the
// admission rule. The gate decides per subject language, so whatever picks that
// language is as load-bearing as the gate itself: a caller free to supply it can
// pass the workspace's "primary" language and re-create the one-boolean answer
// the per-subject gate exists to remove — and no test of the gate can catch that,
// because the gate would still be behaving correctly on the input it was given.
func TestCallGraphSubject_LanguageComesFromTheIndex(t *testing.T) {
	f := newResolverFixture(t)
	csFile := insertLangFile(t, f.db, "src/Alpha/Alpha.cs", "csharp")
	csDo := insertTestNode(t, f.db, csFile, "src/Alpha/Alpha.cs",
		Node{Kind: KindFunction, Name: "Do", Language: "csharp"})
	ctx := context.Background()

	for _, tc := range []struct {
		path string
		want string
	}{
		{"internal/caller/caller.go", "go"},
		{"src/Alpha/Alpha.cs", "csharp"},
		{"does/not/exist.rb", ""},
	} {
		got, err := CallGraphSubjectForPath(ctx, f.db, tc.path)
		if err != nil {
			t.Fatalf("CallGraphSubjectForPath(%q): %v", tc.path, err)
		}
		if got.Language != tc.want {
			t.Errorf("subject language for %q = %q, want %q — it must be read from the index, not assumed",
				tc.path, got.Language, tc.want)
		}
		if got.Path != tc.path {
			t.Errorf("subject path = %q, want %q", got.Path, tc.path)
		}
	}

	// A node subject reads the NODE's language, and carries its file.
	nodeSubject, err := CallGraphSubjectForNode(ctx, f.db, csDo)
	if err != nil {
		t.Fatalf("CallGraphSubjectForNode: %v", err)
	}
	if nodeSubject.Language != "csharp" || nodeSubject.Path != "src/Alpha/Alpha.cs" {
		t.Errorf("node subject = %+v, want csharp at src/Alpha/Alpha.cs", nodeSubject)
	}
	goSubject, err := CallGraphSubjectForNode(ctx, f.db, f.alphaDo)
	if err != nil {
		t.Fatalf("CallGraphSubjectForNode(go): %v", err)
	}
	if goSubject.Language != "go" {
		t.Errorf("node subject language = %q, want go", goSubject.Language)
	}
	if nodeSubject.Language == goSubject.Language {
		t.Error("both node subjects derived the same language; the derivation is not reading the index")
	}

	// And the derived subjects reach opposite verdicts through the ordinary rule.
	goAdmission, err := AdmitCallGraph(ctx, f.db, goSubject)
	if err != nil {
		t.Fatal(err)
	}
	csAdmission, err := AdmitCallGraph(ctx, f.db, nodeSubject)
	if err != nil {
		t.Fatal(err)
	}
	if !goAdmission.Admitted {
		t.Errorf("the derived Go subject was refused: %q", goAdmission.Refusal)
	}
	if csAdmission.Admitted {
		t.Error("the derived C# subject was admitted")
	}
}
