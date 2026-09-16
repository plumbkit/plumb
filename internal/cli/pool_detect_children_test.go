package cli

import (
	"context"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
)

// detectTestPoolMonorepo builds a pool with go, swift, and zig enabled — the
// languages exercised by the nested-monorepo child-discovery tests.
func detectTestPoolMonorepo() *workspacePool {
	return &workspacePool{
		entries:  make(map[poolKey]*poolEntry),
		baseCtx:  context.Background(),
		cacheTTL: time.Minute,
		langs: []langConfig{
			{name: "go", cfg: config.LSPConfig{RootMarkers: []string{"go.mod", "go.work"}, Enabled: true}},
			{name: "swift", cfg: config.LSPConfig{RootMarkers: []string{"Package.swift", "*.xcodeproj"}, Enabled: true}},
			{name: "zig", cfg: config.LSPConfig{RootMarkers: []string{"build.zig", "build.zig.zon"}, Enabled: true}},
		},
	}
}

// langsOf returns the sorted distinct languages of a discovered set, for stable
// comparison regardless of directory-walk order.
func langsOf(ds []discoveredRoot) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.language)
	}
	sort.Strings(out)
	return out
}

// TestDiscoverChildLanguages_Monorepo is the pauta repro: a .plumb/ root with no
// marker of its own, a zig project in core/ and a swift project in app/. Detect
// still resolves the root as LanguageNone (it only walks up), while
// discoverChildLanguages finds both children.
func TestDiscoverChildLanguages_Monorepo(t *testing.T) {
	root := freshTempDir(t)
	mustMkdir(t, filepath.Join(root, ".plumb"))
	mustMkdir(t, filepath.Join(root, "core"))
	mustWrite(t, filepath.Join(root, "core", "build.zig"), "")
	mustWrite(t, filepath.Join(root, "core", "build.zig.zon"), "")
	mustMkdir(t, filepath.Join(root, "app"))
	mustWrite(t, filepath.Join(root, "app", "Package.swift"), "")

	p := detectTestPoolMonorepo()

	if _, lang, err := p.Detect(root); err != nil || lang != LanguageNone {
		t.Fatalf("Detect(root) = (%q, %v), want (%q, nil) — detection must not descend", lang, err, LanguageNone)
	}

	got := p.discoverChildLanguages(root, 2)
	if want := []string{"swift", "zig"}; !equalStrings(langsOf(got), want) {
		t.Errorf("discovered languages = %v, want %v", langsOf(got), want)
	}
	for _, d := range got {
		switch d.language {
		case "zig":
			if d.root != filepath.Join(root, "core") {
				t.Errorf("zig root = %s, want %s", d.root, filepath.Join(root, "core"))
			}
		case "swift":
			if d.root != filepath.Join(root, "app") {
				t.Errorf("swift root = %s, want %s", d.root, filepath.Join(root, "app"))
			}
		}
	}
}

// TestDiscoverChildLanguages_DepthBoundary: a marker one level deeper than
// maxDepth is invisible; raising maxDepth finds it.
func TestDiscoverChildLanguages_DepthBoundary(t *testing.T) {
	root := freshTempDir(t)
	// build.zig sits in root/a/b/c — its dir is at depth 3 (a=1, b=2, c=3), one
	// level beyond maxDepth 2.
	deep := filepath.Join(root, "a", "b", "c")
	mustMkdir(t, deep)
	mustWrite(t, filepath.Join(deep, "build.zig"), "")

	p := detectTestPoolMonorepo()
	if got := p.discoverChildLanguages(root, 2); len(got) != 0 {
		t.Errorf("depth 2 found %v, want none (marker is below the bound)", got)
	}
	if got := p.discoverChildLanguages(root, 3); len(got) != 1 || got[0].language != "zig" {
		t.Errorf("depth 3 = %v, want one zig root", got)
	}
}

// TestDiscoverChildLanguages_SkipsNoiseDirs: a marker inside a pruned dir
// (node_modules, .build, zig-cache) is ignored.
func TestDiscoverChildLanguages_SkipsNoiseDirs(t *testing.T) {
	root := freshTempDir(t)
	for _, noise := range []string{"node_modules", ".build", "zig-cache", "build"} {
		d := filepath.Join(root, noise)
		mustMkdir(t, d)
		mustWrite(t, filepath.Join(d, "build.zig"), "")
	}
	p := detectTestPoolMonorepo()
	if got := p.discoverChildLanguages(root, 3); len(got) != 0 {
		t.Errorf("discovered %v inside pruned dirs, want none", got)
	}
}

// TestDiscoverChildLanguages_DuplicateLanguage: two subdirs naming the same
// language yield two distinct discovered roots (distinct pool entries).
func TestDiscoverChildLanguages_DuplicateLanguage(t *testing.T) {
	root := freshTempDir(t)
	for _, sub := range []string{"core", "tools"} {
		d := filepath.Join(root, sub)
		mustMkdir(t, d)
		mustWrite(t, filepath.Join(d, "build.zig"), "")
	}
	p := detectTestPoolMonorepo()
	got := p.discoverChildLanguages(root, 2)
	if len(got) != 2 {
		t.Fatalf("discovered %d roots, want 2", len(got))
	}
	if l := distinctLanguages(got); !equalStrings(l, []string{"zig"}) {
		t.Errorf("distinct languages = %v, want [zig]", l)
	}
}

// TestDiscoverChildLanguages_StopsAtMatchedRoot: a language project root is a
// boundary — the walk does not descend into it to find a nested marker.
func TestDiscoverChildLanguages_StopsAtMatchedRoot(t *testing.T) {
	root := freshTempDir(t)
	core := filepath.Join(root, "core")
	mustMkdir(t, core)
	mustWrite(t, filepath.Join(core, "build.zig"), "")
	// A nested Package.swift below the matched zig root must not be reported.
	nested := filepath.Join(core, "vendored")
	mustMkdir(t, nested)
	mustWrite(t, filepath.Join(nested, "Package.swift"), "")

	p := detectTestPoolMonorepo()
	got := p.discoverChildLanguages(root, 5)
	if len(got) != 1 || got[0].language != "zig" {
		t.Errorf("discovered %v, want a single zig root (the matched root is a boundary)", got)
	}
}

// TestDiscoverChildLanguages_DepthZeroDisables: maxDepth 0 disables discovery.
func TestDiscoverChildLanguages_DepthZeroDisables(t *testing.T) {
	root := freshTempDir(t)
	mustMkdir(t, filepath.Join(root, "core"))
	mustWrite(t, filepath.Join(root, "core", "build.zig"), "")
	if got := detectTestPoolMonorepo().discoverChildLanguages(root, 0); got != nil {
		t.Errorf("maxDepth 0 discovered %v, want nil", got)
	}
}

// TestElectPrimary_Order: go wins, else alphabetical by language, tie-broken by
// root path — matching newWorkspacePool's deterministic ordering.
func TestElectPrimary_Order(t *testing.T) {
	cases := []struct {
		name string
		in   []discoveredRoot
		want discoveredRoot
	}{
		{
			name: "alphabetical: swift beats zig",
			in:   []discoveredRoot{{root: "/w/core", language: "zig"}, {root: "/w/app", language: "swift"}},
			want: discoveredRoot{root: "/w/app", language: "swift"},
		},
		{
			name: "go wins over alphabetically-earlier",
			in:   []discoveredRoot{{root: "/w/app", language: "swift"}, {root: "/w/svc", language: "go"}},
			want: discoveredRoot{root: "/w/svc", language: "go"},
		},
		{
			name: "same language: lexicographic root",
			in:   []discoveredRoot{{root: "/w/tools", language: "zig"}, {root: "/w/core", language: "zig"}},
			want: discoveredRoot{root: "/w/core", language: "zig"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := electPrimary(c.in); got != c.want {
				t.Errorf("electPrimary = %+v, want %+v", got, c.want)
			}
		})
	}
}

// TestElectPrimary_MarkerBeatsSniffed pins the compatibility guarantee for the
// markerless census. Language order is alphabetical, so "python" sorts ahead of
// "typescript": without the tier a censused python would take the primary of
// every workspace whose marker-backed primary is typescript, swapping the server
// and the identity line of a project whose files did not move. Asserted in both
// slice orders, because electPrimary folds left and a rule that only held when
// the marker-backed entry happened to come first would be no rule at all.
func TestElectPrimary_MarkerBeatsSniffed(t *testing.T) {
	marker := discoveredRoot{root: "/w/app", language: "typescript"}
	sniffed := discoveredRoot{root: "/w", language: "python", sniffed: true}

	for _, in := range [][]discoveredRoot{{marker, sniffed}, {sniffed, marker}} {
		if got := electPrimary(in); got != marker {
			t.Errorf("electPrimary(%v) = %+v, want the marker-backed typescript root — "+
				"a file count must not displace a manifest", orderedLangs(in), got)
		}
	}
}

// TestElectPrimary_AllSniffedFollowsFileCount is the regression pin for a defect
// review caught in the first version of the census: with NO marker anywhere,
// every entry is sniffed, and ordering the tier alphabetically elected "html"
// over a "python" owning three times as many files — vscode-html-language-server
// started for a Django-shaped repo, its sources unserved. That is the defect
// weakLangAt's doc comment records as already fixed once, and it was also a
// silent behaviour change: this path used to be extLangAt, which picks the
// dominant language.
//
// Counts contradict alphabetical order deliberately, so the assertion cannot be
// satisfied by the order it is meant to rule out.
func TestElectPrimary_AllSniffedFollowsFileCount(t *testing.T) {
	in := []discoveredRoot{
		{root: "/w", language: "html", sniffed: true, files: 30},
		{root: "/w", language: "python", sniffed: true, files: 100},
	}
	want := in[1]
	for _, order := range [][]discoveredRoot{in, {in[1], in[0]}} {
		if got := electPrimary(order); got != want {
			t.Errorf("electPrimary = %+v, want the dominant python — \"html\" sorts first "+
				"alphabetically and must not win on that", got)
		}
	}
}

// TestElectPrimary_SniffedTieFallsBackToLanguageOrder: the count decides only
// when the counts DIFFER. On an equal count the usual go-first-then-alphabetical
// order still picks, or the choice would be arbitrary for that workspace.
func TestElectPrimary_SniffedTieFallsBackToLanguageOrder(t *testing.T) {
	in := []discoveredRoot{
		{root: "/w", language: "typescript", sniffed: true, files: 12},
		{root: "/w", language: "python", sniffed: true, files: 12},
	}
	if got := electPrimary(in); got != in[1] {
		t.Errorf("electPrimary = %+v, want python — on an equal count the language order stands", got)
	}
}

// TestElectPrimary_MarkerBeatsSniffedRegardlessOfCount: the tier outranks the
// count, not the other way round. A sniffed language owning far more files than
// the marker-backed one still does not take the primary — otherwise the count
// clause would quietly undo the compatibility guarantee the tier exists for.
func TestElectPrimary_MarkerBeatsSniffedRegardlessOfCount(t *testing.T) {
	marker := discoveredRoot{root: "/w/app", language: "typescript"}
	sniffed := discoveredRoot{root: "/w", language: "python", sniffed: true, files: 5000}
	if got := electPrimary([]discoveredRoot{sniffed, marker}); got != marker {
		t.Errorf("electPrimary = %+v, want the marker-backed typescript root", got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
