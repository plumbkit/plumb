package cli

import (
	"context"
	"path/filepath"
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
	if info := sessionRecord(t, s.sessID); info.DetectedLanguage != "go" {
		t.Errorf("DetectedLanguage = %q, want exactly %q — a single-language repo must not "+
			"gain a comma-joined label", info.DetectedLanguage, "go")
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
