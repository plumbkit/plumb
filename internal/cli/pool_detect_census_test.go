package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// The census exists because detection had two contributors that each stood down
// when the other had answered: child discovery matches markers, the last-resort
// sniff fires only when nothing else resolved, and so one app/tsconfig.json hid
// a forty-file Python tree from every surface that reads the discovered set.
// These tests pin the remainder-is-evidence rule and the two floors that keep a
// stray file from starting a language server.

// writeN writes n files named <stem><i><ext> under dir/sub.
func writeN(t *testing.T, dir, sub, stem, ext string, n int) {
	t.Helper()
	for i := range n {
		mustWrite(t, filepath.Join(dir, sub, fmt.Sprintf("%s%d%s", stem, i, ext)), "x\n")
	}
}

// orderedLangs names the languages of a discovered set IN THE ORDER RETURNED,
// where the shared langsOf sorts. The census's own ordering is a property under
// test — it reaches the session's language label — so the determinism assertion
// needs a view that a sort would hide.

// TestCensusMarkerlessLanguages_PythonBesideClaimedTypescript is the reported
// shape: a TypeScript app with its own tsconfig.json under a root whose Python
// sources are spread across sibling directories with no manifest anywhere. Before
// the census this resolved typescript alone.
func TestCensusMarkerlessLanguages_PythonBesideClaimedTypescript(t *testing.T) {
	dir := freshTempDir(t)
	mustWrite(t, filepath.Join(dir, "app", "tsconfig.json"), "{}")
	writeN(t, dir, "app/src", "mod", ".ts", 12)
	writeN(t, dir, "ism", "a", ".py", 8)
	writeN(t, dir, "ism_core", "b", ".py", 9)
	writeN(t, dir, "server", "c", ".py", 6)

	claimed := []discoveredRoot{{root: filepath.Join(dir, "app"), language: "typescript"}}
	got := defaultsPool(t, "typescript", "python").censusMarkerlessLanguages(dir, claimed)

	if len(got) != 1 || got[0].language != "python" {
		t.Fatalf("census = %v, want exactly one python entry — 23 .py files across three "+
			"sibling dirs are the project's evidence, and no marker speaks for them", langsOf(got))
	}
	if got[0].root != dir {
		t.Errorf("census root = %q, want the workspace root %q — per-file routing resolves "+
			"the same root for those files, and both must name one pool key", got[0].root, dir)
	}
	if !got[0].sniffed {
		t.Error("census entry must be flagged sniffed, or it outranks the marker-backed " +
			"typescript root in election and silently takes the primary")
	}
}

// TestCensusMarkerlessLanguages_PrunesClaimedSubtree pins the skipPath parameter:
// the TypeScript app's own directory is already served, so files inside it are
// not part of the remainder the census measures. Without pruning, a .py file
// vendored under app/ would vote for a root that does not hold it.
func TestCensusMarkerlessLanguages_PrunesClaimedSubtree(t *testing.T) {
	dir := freshTempDir(t)
	mustWrite(t, filepath.Join(dir, "app", "tsconfig.json"), "{}")
	writeN(t, dir, "app/scripts", "gen", ".py", 40)
	mustWrite(t, filepath.Join(dir, "README.md"), "#\n")

	claimed := []discoveredRoot{{root: filepath.Join(dir, "app"), language: "typescript"}}
	got := defaultsPool(t, "typescript", "python").censusMarkerlessLanguages(dir, claimed)

	if len(got) != 0 {
		t.Fatalf("census = %v, want none — every .py sits inside the claimed app/ subtree, "+
			"which a language server already covers", langsOf(got))
	}
}

// TestCensusMarkerlessLanguages_SkipsAlreadyDiscoveredLanguage: a marker is a
// declaration and a file count is an inference, so a language a marker already
// named must not also arrive as a sniffed entry — that would list it twice and
// start a second server at a second root.
func TestCensusMarkerlessLanguages_SkipsAlreadyDiscoveredLanguage(t *testing.T) {
	dir := freshTempDir(t)
	mustWrite(t, filepath.Join(dir, "svc", "pyproject.toml"), "")
	writeN(t, dir, "tools", "t", ".py", 30)

	claimed := []discoveredRoot{{root: filepath.Join(dir, "svc"), language: "python"}}
	got := defaultsPool(t, "python", "typescript").censusMarkerlessLanguages(dir, claimed)

	if len(got) != 0 {
		t.Fatalf("census = %v, want none — python is already discovered at svc/", langsOf(got))
	}
}

// TestCensusMarkerlessLanguages_BelowAbsoluteFloor: four files is under
// censusMinFiles even at a commanding share. One incidental script plus its
// __init__.py scaffolding must never start a language server.
func TestCensusMarkerlessLanguages_BelowAbsoluteFloor(t *testing.T) {
	dir := freshTempDir(t)
	mustWrite(t, filepath.Join(dir, "app", "tsconfig.json"), "{}")
	writeN(t, dir, "app/src", "mod", ".ts", 5)
	writeN(t, dir, "scripts", "deploy", ".py", 4)

	claimed := []discoveredRoot{{root: filepath.Join(dir, "app"), language: "typescript"}}
	got := defaultsPool(t, "typescript", "python").censusMarkerlessLanguages(dir, claimed)

	if len(got) != 0 {
		t.Fatalf("census = %v, want none — 4 files is below the absolute floor however "+
			"large its share of the remainder", langsOf(got))
	}
}

// TestCensusMarkerlessLanguages_BelowShareFloor is the case the absolute floor
// alone cannot reject, and is therefore what pins the conjunction: six .py
// helpers clear the floor, but against a large remainder they are noise, and a
// big repo is exactly where a wrongly-started server is most expensive.
func TestCensusMarkerlessLanguages_BelowShareFloor(t *testing.T) {
	dir := freshTempDir(t)
	writeN(t, dir, "web", "page", ".html", 200)
	writeN(t, dir, "scripts", "helper", ".py", 6)

	got := defaultsPool(t, "html", "python").censusMarkerlessLanguages(dir, nil)

	if langs := langsOf(got); contains(langs, "python") {
		t.Fatalf("census = %v, want no python — 6 of 206 counted files is under the share "+
			"floor, and the absolute floor alone would have admitted it", langs)
	}
}

// TestCensusThenElect_MarkerlessPolyglotKeepsTheDominantLanguage joins the
// census to election, which the unit tests either side of it do not: the census
// orders by count, election re-orders, and a regression in the second silently
// undoes the first. Review found exactly that — the sniffed tier was ordered
// alphabetically, so this repo elected html.
//
// It also pins the no-silent-change property against the path this replaces:
// before the census a markerless root went through extLangAt, so whatever
// extLangAt answers here is what the workspace used to get, and must still get.
func TestCensusThenElect_MarkerlessPolyglotKeepsTheDominantLanguage(t *testing.T) {
	dir := freshTempDir(t)
	writeN(t, dir, "svc", "a", ".py", 100)
	writeN(t, dir, "templates", "p", ".html", 30)

	p := defaultsPool(t, "python", "html")
	before := p.extLangAt(dir)
	if before != "python" {
		t.Fatalf("precondition: extLangAt = %q, want python — the fixture is meant to have "+
			"an unambiguous dominant language", before)
	}

	got := electPrimary(p.censusMarkerlessLanguages(dir, nil))

	if got.language != before {
		t.Errorf("elected %q, want %q — a markerless root must not change the language it "+
			"attaches just because the census now nominates more than one", got.language, before)
	}
}

// TestCensusMarkerlessLanguages_AtTheFloors pins both thresholds from ABOVE,
// which the below-the-floor tests cannot do: they bound the constants loosely
// from one side, so raising either would have gone unnoticed. Exactly
// censusMinFiles files at exactly censusMinShare of the remainder must qualify.
// 5 python files against 45 typescript ones is 5/50 — the boundary itself.
func TestCensusMarkerlessLanguages_AtTheFloors(t *testing.T) {
	dir := freshTempDir(t)
	writeN(t, dir, "web", "m", ".ts", 45)
	writeN(t, dir, "scripts", "s", ".py", 5)

	got := defaultsPool(t, "typescript", "python").censusMarkerlessLanguages(dir, nil)

	if !contains(langsOf(got), "python") {
		t.Fatalf("census = %v, want python — 5 files is exactly censusMinFiles and 5/50 is "+
			"exactly censusMinShare; the floors are inclusive", langsOf(got))
	}
}

// TestCensusMarkerlessLanguages_UncountedFileTypesDoNotDilute: the denominator is
// restricted to languages that could themselves be nominated. langsupport
// recognises json/yaml/markdown, none of which has a language server, so counting
// them would let a fixture tree veto a real nomination — 40 .py beside 400 .json
// is 9% of everything and 100% of the code.
func TestCensusMarkerlessLanguages_UncountedFileTypesDoNotDilute(t *testing.T) {
	dir := freshTempDir(t)
	writeN(t, dir, "ism", "a", ".py", 40)
	writeN(t, dir, "fixtures", "f", ".json", 400)

	got := defaultsPool(t, "python", "typescript").censusMarkerlessLanguages(dir, nil)

	if !contains(langsOf(got), "python") {
		t.Fatalf("census = %v, want python — a JSON fixture tree has no language server "+
			"and must not dilute the share of the code", langsOf(got))
	}
}

// TestCensusMarkerlessLanguages_PrunesClaimedSubtreeViaNonCanonicalRoot: the
// prune compares a claimed root against paths built from the census root, so the
// two have to agree on a spelling. Canonicalising the root once is what gives
// that agreement; a caller reaching the census through a symlinked spelling must
// still prune the subtree a server already covers.
func TestCensusMarkerlessLanguages_PrunesClaimedSubtreeViaNonCanonicalRoot(t *testing.T) {
	actual := freshTempDir(t)
	mustWrite(t, filepath.Join(actual, "app", "tsconfig.json"), "{}")
	writeN(t, actual, "app/scripts", "gen", ".py", 40)

	link := filepath.Join(freshTempDir(t), "alias")
	if err := os.Symlink(actual, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	claimed := []discoveredRoot{{root: filepath.Join(link, "app"), language: "typescript"}}
	got := defaultsPool(t, "typescript", "python").censusMarkerlessLanguages(link, claimed)

	if len(got) != 0 {
		t.Fatalf("census = %v, want none — every .py sits under the claimed app/, reached "+
			"here by a symlinked spelling of the root", langsOf(got))
	}
}

// TestCensusMarkerlessLanguages_InactiveLanguageExcluded: the install -> on gate.
// A language the project has disabled (or whose server is not installed) is
// counted by the walk but never nominated by it.
func TestCensusMarkerlessLanguages_InactiveLanguageExcluded(t *testing.T) {
	dir := freshTempDir(t)
	mustWrite(t, filepath.Join(dir, "app", "tsconfig.json"), "{}")
	writeN(t, dir, "ism", "a", ".py", 30)

	claimed := []discoveredRoot{{root: filepath.Join(dir, "app"), language: "typescript"}}
	got := defaultsPool(t, "typescript").censusMarkerlessLanguages(dir, claimed)

	if len(got) != 0 {
		t.Fatalf("census = %v, want none — python is not in the effective set", langsOf(got))
	}
}

// TestCensusMarkerlessLanguages_DepthBound pins censusScanDepth from both sides:
// sources at the limit are seen, sources one level below it are not. Asserting
// only the positive half would keep passing if the bound were raised without
// anyone deciding to raise it.
func TestCensusMarkerlessLanguages_DepthBound(t *testing.T) {
	// A directory at depth <= censusScanDepth has its files counted, so with a
	// bound of 4 that is a/b/c/d, and a/b/c/d/e sits one level past it — the same
	// reading of the bound TestExtLangAt_DepthBound applies to extScanDepth.
	atLimit := filepath.Join("a", "b", "c", "d")
	tooDeep := filepath.Join("a", "b", "c", "d", "e")

	t.Run("at the limit", func(t *testing.T) {
		dir := freshTempDir(t)
		writeN(t, dir, atLimit, "m", ".py", 10)
		got := defaultsPool(t, "python", "typescript").censusMarkerlessLanguages(dir, nil)
		if len(got) != 1 || got[0].language != "python" {
			t.Fatalf("census = %v, want python — %s is within censusScanDepth", langsOf(got), atLimit)
		}
	})

	t.Run("below the limit", func(t *testing.T) {
		dir := freshTempDir(t)
		writeN(t, dir, tooDeep, "m", ".py", 10)
		got := defaultsPool(t, "python", "typescript").censusMarkerlessLanguages(dir, nil)
		if len(got) != 0 {
			t.Fatalf("census = %v, want none — %s is past censusScanDepth", langsOf(got), tooDeep)
		}
	})
}

// TestCensusMarkerlessLanguages_Deterministic guards the map iteration. The
// result reaches the session's language label and adapter list, so two attaches
// of one unchanged workspace must not disagree about their order.
//
// THREE languages, with counts that contradict alphabetical order in both
// directions, and the exact expected order asserted. Two would not do it: with
// html and python alone, "most files first" and plain reverse-alphabetical
// produce the SAME sequence, so a mutant that sorted by name instead of by count
// survived the assertion — the test proved only that the order was stable, never
// that it was the right order. Counted 30/20/10 against an alphabetical
// html/python/typescript, no sort by name in either direction can reproduce
// html/typescript/python.
func TestCensusMarkerlessLanguages_Deterministic(t *testing.T) {
	dir := freshTempDir(t)
	writeN(t, dir, "site", "p", ".html", 30)
	writeN(t, dir, "web", "m", ".ts", 20)
	writeN(t, dir, "svc", "a", ".py", 10)
	want := []string{"html", "typescript", "python"}

	p := defaultsPool(t, "python", "html", "typescript")
	first := orderedLangs(p.censusMarkerlessLanguages(dir, nil))
	if !equalStrings(first, want) {
		t.Fatalf("census order = %v, want %v — most files first, which is neither "+
			"alphabetical nor reverse-alphabetical for this fixture", first, want)
	}
	for range 12 {
		if got := orderedLangs(p.censusMarkerlessLanguages(dir, nil)); !equalStrings(got, first) {
			t.Fatalf("census order drifted: %v then %v — a map range reached the result", first, got)
		}
	}
}

// TestSniffCountsIn_SkipPathNilIsUnchanged is the regression guard on the
// signature change: every pre-existing caller passes nil, and nil must mean
// "prune nothing by path" rather than "prune everything".
func TestSniffCountsIn_SkipPathNilIsUnchanged(t *testing.T) {
	dir := freshTempDir(t)
	writeN(t, dir, "pkg", "m", ".py", 3)

	p := defaultsPool(t, "python")
	counts, _ := p.sniffCountsIn(p.effectiveLanguages(dir), dir, extScanDepth, extScanMaxFiles, nil, skipChildDir, nil)
	if counts["python"] != 3 {
		t.Fatalf("counts[python] = %d, want 3 — a nil skipPath must prune nothing", counts["python"])
	}
}

func orderedLangs(ds []discoveredRoot) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.language
	}
	return out
}
