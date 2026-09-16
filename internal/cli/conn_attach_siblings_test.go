package cli

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
)

// The mirror of the markerless census: a root that HAS a language of its own
// used to stop discovering there, so `go.mod` beside `web/tsconfig.json` and a
// `tools/` Python tree reported "Go" alone. These pin that the siblings are
// surfaced, that the primary does NOT change, and that the feature can be turned
// off.

// goRootWithSiblings lays out the reported shape: a Go root of its own, a
// marker-carrying TypeScript child, and a manifest-less Python tree.
func goRootWithSiblings(t *testing.T) string {
	t.Helper()
	root := freshTempDir(t)
	mustWrite(t, filepath.Join(root, "go.mod"), "module x\n")
	writeN(t, root, "", "m", ".go", 8)
	mustWrite(t, filepath.Join(root, "web", "tsconfig.json"), "{}")
	writeN(t, root, "web/src", "c", ".ts", 10)
	writeN(t, root, "tools", "t", ".py", 12)
	return root
}

func siblingSession(t *testing.T, pool *workspacePool, discoverSiblings bool) *connSession {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	cfg := config.Defaults()
	cfg.Workspace.DiscoverSiblings = discoverSiblings
	s := newConnSession(context.Background(), pool, nil, config.NewStore(cfg), nil, nil, newSharedBudgets())
	t.Cleanup(s.close)
	return s
}

// TestSiblingLanguages_MarkerRootSurfacesItsOtherLanguages is the defect: both a
// marker-carrying child and a markerless tree must be found beneath a root that
// already has a language.
func TestSiblingLanguages_MarkerRootSurfacesItsOtherLanguages(t *testing.T) {
	pool := censusPool("go", "typescript", "python")
	root := goRootWithSiblings(t)

	got := langsOf(siblingSession(t, pool, true).siblingLanguages(root, "go"))

	for _, want := range []string{"python", "typescript"} {
		if !contains(got, want) {
			t.Errorf("siblings = %v, want %s — a root with its own marker still has to "+
				"discover what else it holds", got, want)
		}
	}
	if contains(got, "go") {
		t.Errorf("siblings = %v, must not include the primary — its server is already bound "+
			"at the root and covers those files by containment", got)
	}
}

// TestSiblingLanguages_ClaimedRootDoesNotPruneTheWholeTree is a positive control
// for the riskiest assumption in siblingLanguages: it passes the WORKSPACE ROOT
// itself as a claimed root, and the census prunes claimed roots from its walk.
// If the root were tested against that prune the entire tree would be excluded,
// the census would always answer nothing, and every other test here would still
// pass on the strength of discoverChildLanguages alone — the failure would be
// silent and total. The fixture therefore has NO marker-carrying child, so only
// the census can produce this answer.
func TestSiblingLanguages_ClaimedRootDoesNotPruneTheWholeTree(t *testing.T) {
	pool := censusPool("go", "python")
	root := freshTempDir(t)
	mustWrite(t, filepath.Join(root, "go.mod"), "module x\n")
	writeN(t, root, "tools", "t", ".py", 30)

	got := langsOf(siblingSession(t, pool, true).siblingLanguages(root, "go"))

	if !contains(got, "python") {
		t.Fatalf("siblings = %v, want python — passing the root as claimed must prune only "+
			"that entry's own subtree matches, never the tree the walk starts from", got)
	}
}

// TestSiblingLanguages_TheMotivatingCase is the shape the whole feature is for,
// as written in the commit message, the CHANGELOG and both docs: a Go repo whose
// Python lives in a tools/ tree with no manifest. It nominated NOTHING, because
// the census denominator counted the primary's own 400 .go files and put python
// at 9% — the documented case rejected by its own threshold. Review caught it;
// no test here had a realistic ratio of primary sources to sibling sources.
func TestSiblingLanguages_TheMotivatingCase(t *testing.T) {
	pool := censusPool("go", "python")
	root := freshTempDir(t)
	mustWrite(t, filepath.Join(root, "go.mod"), "module x\n")
	writeN(t, root, "", "m", ".go", 400)
	writeN(t, root, "tools", "t", ".py", 40)

	got := langsOf(siblingSession(t, pool, true).siblingLanguages(root, "go"))

	if !contains(got, "python") {
		t.Fatalf("siblings = %v, want python — a language that already has a server is not "+
			"part of the remainder that decides whether to start another", got)
	}
}

// TestSiblingLanguages_FixtureTreesAreNotLanguages: plumb's own repository has
// testdata/*-fixture directories carrying real manifests, and before this was
// pruned attaching to plumb itself nominated python and typescript from them —
// starting pyright and tsserver against test assets and merging fixture symbols
// into workspace_symbols results. Latent while this walk ran only for markerless
// roots; sibling discovery made it the common path.
func TestSiblingLanguages_FixtureTreesAreNotLanguages(t *testing.T) {
	pool := censusPool("go", "typescript", "python")
	root := freshTempDir(t)
	mustWrite(t, filepath.Join(root, "go.mod"), "module x\n")
	writeN(t, root, "", "m", ".go", 200)
	for _, dir := range []string{"testdata", "fixtures", "third_party"} {
		mustWrite(t, filepath.Join(root, dir, "proj", "pyproject.toml"), "")
		writeN(t, root, dir+"/proj", "f", ".py", 20)
	}

	got := langsOf(siblingSession(t, pool, true).siblingLanguages(root, "go"))

	if len(got) != 0 {
		t.Errorf("siblings = %v, want none — a manifest inside testdata/fixtures/third_party "+
			"is a test asset, not a project to serve", got)
	}
}

// TestSiblingLanguages_DefaultsToOn pins the DEFAULT, which the other tests
// cannot: they set the flag explicitly, so flipping config.Defaults() changed no
// assertion and the shipped behaviour was unguarded. Mutation found this.
func TestSiblingLanguages_DefaultsToOn(t *testing.T) {
	if !config.Defaults().Workspace.DiscoverSiblings {
		t.Error("workspace.discover_siblings defaults to false — multi-language discovery " +
			"is the documented behaviour, and this is the half that never ran")
	}
}

// TestSiblingLanguages_MarkerChildTooSmallForTheCensus pins that CHILD MARKERS
// are consulted, not just the census. With ten .ts files beneath it the census
// nominates typescript on its own, so removing discoverChildLanguages entirely
// left every other test green — the marker path was riding on the census's
// answer. Two .ts files is below censusMinFiles, so only the marker can find it.
func TestSiblingLanguages_MarkerChildTooSmallForTheCensus(t *testing.T) {
	pool := censusPool("go", "typescript", "python")
	root := freshTempDir(t)
	mustWrite(t, filepath.Join(root, "go.mod"), "module x\n")
	writeN(t, root, "", "m", ".go", 40)
	mustWrite(t, filepath.Join(root, "web", "tsconfig.json"), "{}")
	writeN(t, root, "web/src", "c", ".ts", 2) // below the census floor

	got := langsOf(siblingSession(t, pool, true).siblingLanguages(root, "go"))

	if !contains(got, "typescript") {
		t.Errorf("siblings = %v, want typescript — a manifest names a language however few "+
			"files back it; that is what makes it a marker rather than a count", got)
	}
}

// TestSiblingLanguages_NestedRootOfThePrimaryLanguageIsDropped: a child module
// in the SAME language (root/go.mod plus sub/go.mod) is covered by the primary's
// server by containment, so listing it would add a duplicate adapter and a
// redundant fan-out target. Nothing else in this file reaches that filter — the
// other fixtures have no same-language child — so removing it changed no test.
func TestSiblingLanguages_NestedRootOfThePrimaryLanguageIsDropped(t *testing.T) {
	pool := censusPool("go", "typescript", "python")
	root := freshTempDir(t)
	mustWrite(t, filepath.Join(root, "go.mod"), "module x\n")
	writeN(t, root, "", "m", ".go", 20)
	mustWrite(t, filepath.Join(root, "sub", "go.mod"), "module x/sub\n")
	writeN(t, root, "sub", "s", ".go", 10)

	got := siblingSession(t, pool, true).siblingLanguages(root, "go")

	for _, d := range got {
		if d.language == "go" {
			t.Errorf("siblings = %v includes a go root at %s — the primary's server is bound "+
				"at the workspace root and already covers it", langsOf(got), d.root)
		}
	}
}

// TestSiblingLanguages_DisabledByConfig: the off switch actually switches it off.
// Without this the knob would be decoration.
func TestSiblingLanguages_DisabledByConfig(t *testing.T) {
	pool := censusPool("go", "typescript", "python")
	root := goRootWithSiblings(t)

	if got := siblingSession(t, pool, false).siblingLanguages(root, "go"); len(got) != 0 {
		t.Errorf("siblings = %v with discover_siblings=false, want none", langsOf(got))
	}
}

// TestSiblingLanguages_HomeRootIsNeverScanned: the LanguageNone path refuses to
// descend into a home directory, and this path reaches the same walk by a
// different route. A guard that protects only one of them is the shape of bug
// this whole change is about.
func TestSiblingLanguages_HomeRootIsNeverScanned(t *testing.T) {
	home := freshTempDir(t)
	t.Setenv("HOME", home)
	mustWrite(t, filepath.Join(home, "go.mod"), "module x\n")
	writeN(t, home, "projects", "a", ".py", 40)

	pool := censusPool("go", "python")
	if got := siblingSession(t, pool, true).siblingLanguages(home, "go"); len(got) != 0 {
		t.Errorf("siblings = %v for a home root, want none — the descent must not run there",
			langsOf(got))
	}
}

// TestResolvePrimaryLSP_MarkerRootKeepsItsPrimaryAndListsSiblings is the
// end-to-end assertion at the level the user sees: the session record's
// DetectedLanguage is what the TUI badge and workspace_sessions read, and the
// primary must NOT move to a sibling.
func TestResolvePrimaryLSP_MarkerRootKeepsItsPrimaryAndListsSiblings(t *testing.T) {
	pool := censusPool("go", "typescript", "python")
	root := goRootWithSiblings(t)
	installEntryLang(pool, root, "go", &stubClient{id: "go"})
	installEntryLang(pool, filepath.Join(root, "web"), "typescript", &stubClient{id: "ts"})
	installEntryLang(pool, root, "python", &stubClient{id: "py"})

	s := siblingSession(t, pool, true)
	s.attachWorkspace(context.Background(), "file://"+root)

	if got := s.acquiredLanguageName(); got != "go" {
		t.Errorf("primary = %q, want go — a sibling must never take the primary a marker "+
			"at the root already chose", got)
	}
	info := sessionRecord(t, s.sessID)
	for _, want := range []string{"go", "python", "typescript"} {
		if !strings.Contains(info.DetectedLanguage, want) {
			t.Errorf("DetectedLanguage = %q, want it to name %s", info.DetectedLanguage, want)
		}
	}
}

// TestResolvePrimaryLSP_SingleLanguageRootStillReportsOneLanguage: the common
// case must not gain a multi-language label. A plain Go repo has no siblings, so
// discovered stays nil and the session reports exactly what it always did.
func TestResolvePrimaryLSP_SingleLanguageRootStillReportsOneLanguage(t *testing.T) {
	pool := censusPool("go", "typescript", "python")
	root := freshTempDir(t)
	mustWrite(t, filepath.Join(root, "go.mod"), "module x\n")
	writeN(t, root, "", "m", ".go", 12)
	installEntryLang(pool, root, "go", &stubClient{id: "go"})

	s := siblingSession(t, pool, true)
	s.attachWorkspace(context.Background(), "file://"+root)

	if got := s.acquiredLanguageName(); got != "go" {
		t.Fatalf("primary = %q, want go", got)
	}
	info := sessionRecord(t, s.sessID)
	if info.DetectedLanguage != "go" {
		t.Errorf("DetectedLanguage = %q, want exactly %q — a single-language repo must not "+
			"gain a comma-joined label", info.DetectedLanguage, "go")
	}
	// Adapters as well as the label. Mutation showed the single-language
	// early-return could be deleted with no test noticing: adaptersFor("gopls")
	// returns [gopls] where adaptersForDiscovered(nil) returns nil, so every
	// single-language workspace would have silently reported no LSP at all.
	if !slices.Contains(info.Adapters, "gopls") {
		t.Errorf("Adapters = %v, want gopls listed — the session drives one and must say so",
			info.Adapters)
	}
}

// TestDiscoveredWithPrimary_IncludesThePrimaryExactlyOnce: the slice feeds the
// identity line, the badge, the adapter list and the fan-out, all of which
// describe the whole workspace. Omitting the primary would list every language
// except the one actually answering; duplicating it would add a redundant
// fan-out target and a repeated adapter.
func TestDiscoveredWithPrimary_IncludesThePrimaryExactlyOnce(t *testing.T) {
	root := "/w"
	siblings := []discoveredRoot{
		{root: "/w/web", language: "typescript"},
		{root: root, language: "go"}, // already the primary, by root and language
	}

	got := discoveredWithPrimary(root, "go", siblings)

	var goCount int
	for _, d := range got {
		if d.language == "go" {
			goCount++
		}
	}
	if goCount != 1 {
		t.Errorf("discoveredWithPrimary = %v, want go exactly once, got %d", got, goCount)
	}
	if !contains(langsOf(got), "typescript") {
		t.Errorf("discoveredWithPrimary = %v, want the sibling kept", got)
	}
}

// TestDiscoveredWithPrimary_NoSiblingsIsNil: with nothing else found the result
// must stay nil, not a one-entry slice naming the primary. A non-empty
// discovered set switches workspace_symbols from the primary-only path to
// fan-out and gives the session a comma-joined label, so a single-language repo
// would change behaviour for no reason.
func TestDiscoveredWithPrimary_NoSiblingsIsNil(t *testing.T) {
	if got := discoveredWithPrimary("/w", "go", nil); got != nil {
		t.Errorf("discoveredWithPrimary = %v with no siblings, want nil", got)
	}
}
