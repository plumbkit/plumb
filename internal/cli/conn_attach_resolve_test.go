package cli

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
)

// resolvePrimaryLSP's LanguageNone tail has four exits — discovered primary,
// discovered-primary acquire failure, the last-resort sniff, and plain
// LanguageNone — and before these tests only the first had coverage through the
// function itself. The census made that gap load-bearing: it changes which exit
// a workspace takes, so the ones it does NOT change need pinning too.

// censusPool builds a pool whose typescript and python are both active, with
// the SHIPPED root markers, pointing both commands at a binary that certainly
// exists. Mirrors enableTestPool's reasoning: the markers under test have to be
// the real ones, the servers must not be.
func censusPool(languages ...string) *workspacePool {
	defaults := config.Defaults()
	cfg := config.Config{LSP: map[string]config.LSPConfig{}}
	langs := make([]langConfig, 0, len(languages))
	for _, name := range languages {
		c := defaults.LSP[name]
		c.Command = "go"
		c.Enabled = true
		cfg.LSP[name] = c
		langs = append(langs, langConfig{name: name, cfg: c})
	}
	return &workspacePool{
		entries:    make(map[poolKey]*poolEntry),
		baseCtx:    context.Background(),
		cacheTTL:   time.Minute,
		langs:      langs,
		baseConfig: cfg,
	}
}

// markerlessPythonBesideTS lays out the reported repo: a TypeScript app with its
// own tsconfig.json, and Python sources in sibling directories with no manifest
// anywhere.
func markerlessPythonBesideTS(t *testing.T) string {
	t.Helper()
	root := freshTempDir(t)
	mustWrite(t, filepath.Join(root, ".plumb", "config.toml"), "")
	mustWrite(t, filepath.Join(root, "app", "tsconfig.json"), "{}")
	writeN(t, root, "app/src", "mod", ".ts", 10)
	writeN(t, root, "ism_core", "a", ".py", 12)
	writeN(t, root, "server", "b", ".py", 9)
	return root
}

// TestResolvePrimaryLSP_CensusJoinsDiscovery is the end-to-end pin for the
// reported bug, at the level the user sees it: the session record's
// DetectedLanguage is what the TUI badges and workspace_sessions show, and it
// listed TypeScript alone.
func TestResolvePrimaryLSP_CensusJoinsDiscovery(t *testing.T) {
	pool := censusPool("typescript", "python")
	root := markerlessPythonBesideTS(t)
	installEntryLang(pool, filepath.Join(root, "app"), "typescript", &stubClient{id: "ts"})
	installEntryLang(pool, root, "python", &stubClient{id: "py"})

	s := newRefreshSession(t, pool)
	s.attachWorkspace(context.Background(), "file://"+root)

	info := sessionRecord(t, s.sessID)
	if want := "python, typescript"; info.DetectedLanguage != want {
		t.Errorf("session DetectedLanguage = %q, want %q — the Python tree carries no "+
			"manifest, and before the census nothing nominated it", info.DetectedLanguage, want)
	}
	if got := s.acquiredLanguageName(); got != "typescript" {
		t.Errorf("primary = %q, want typescript — a censused language must not displace "+
			"the marker-backed primary, however many files it owns", got)
	}
}

// TestResolvePrimaryLSP_CensusSurvivesChildScanDisabled: child_scan_depth = 0
// says "do not hunt for subproject markers", which is a different question from
// "do not look at what this root is written in". With the scan off, the marker
// child is not discovered and the census still answers for the remainder.
func TestResolvePrimaryLSP_CensusSurvivesChildScanDisabled(t *testing.T) {
	pool := censusPool("typescript", "python")
	root := markerlessPythonBesideTS(t)
	installEntryLang(pool, root, "python", &stubClient{id: "py"})

	cfg := config.Defaults()
	cfg.Workspace.ChildScanDepth = 0
	s := newConnSession(context.Background(), pool, nil, config.NewStore(cfg), nil, nil, newSharedBudgets())
	t.Cleanup(s.close)
	s.attachWorkspace(context.Background(), "file://"+root)

	if got := s.acquiredLanguageName(); got != "python" {
		t.Errorf("primary = %q, want python — the census is not gated on child_scan_depth", got)
	}
}

// TestResolvePrimaryLSP_SniffBranchAttachesMarkerlessRoot pins the branch the
// census must NOT have swallowed. extLangAt applies no threshold, so a repo of
// three .py files attaches python today; the census floors govern only the
// "join a set someone else populated" path. Nothing covered this branch through
// resolvePrimaryLSP before.
func TestResolvePrimaryLSP_SniffBranchAttachesMarkerlessRoot(t *testing.T) {
	pool := censusPool("typescript", "python")
	root := freshTempDir(t)
	mustWrite(t, filepath.Join(root, ".plumb", "config.toml"), "")
	writeN(t, root, "", "s", ".py", 3) // below censusMinFiles, deliberately
	installEntryLang(pool, root, "python", &stubClient{id: "py"})

	s := newRefreshSession(t, pool)
	s.attachWorkspace(context.Background(), "file://"+root)

	if got := s.acquiredLanguageName(); got != "python" {
		t.Errorf("primary = %q, want python — a small single-language markerless repo "+
			"must keep the unthresholded last-resort sniff it has always had", got)
	}
}

// TestResolvePrimaryLSP_NothingToDiscoverStaysNone: the plain LanguageNone exit.
// A root with no markers and no material source tree attaches with no LSP rather
// than acquiring something on the strength of a stray file.
func TestResolvePrimaryLSP_NothingToDiscoverStaysNone(t *testing.T) {
	pool := censusPool("typescript", "python")
	root := freshTempDir(t)
	mustWrite(t, filepath.Join(root, ".plumb", "config.toml"), "")
	mustWrite(t, filepath.Join(root, "README.md"), "# docs\n")

	s := newRefreshSession(t, pool)
	s.attachWorkspace(context.Background(), "file://"+root)

	if got := s.acquiredLanguageName(); got != "" {
		t.Errorf("primary = %q, want none", got)
	}
	if got := s.workspace(); got != root {
		t.Errorf("workspace = %q, want %q — a languageless root still attaches", got, root)
	}
}

// TestResolvePrimaryLSP_DiscoveredAcquireFailureStillListsLanguages: when the
// elected primary's server cannot start, the discovered set is still published,
// because the lazy routing path retries on first file and the session would
// otherwise report no languages at all. Untested through this function before,
// and the census widened what that set contains.
func TestResolvePrimaryLSP_DiscoveredAcquireFailureStillListsLanguages(t *testing.T) {
	pool := censusPool("typescript", "python")
	// The typescript command is a binary that does not exist, so acquiring the
	// elected primary fails; python is never installed either.
	tsCfg := pool.baseConfig.LSP["typescript"]
	tsCfg.Command = "plumb-no-such-language-server"
	pool.baseConfig.LSP["typescript"] = tsCfg
	for i := range pool.langs {
		if pool.langs[i].name == "typescript" {
			pool.langs[i].cfg = tsCfg
		}
	}
	root := markerlessPythonBesideTS(t)

	s := newRefreshSession(t, pool)
	s.attachWorkspace(context.Background(), "file://"+root)

	if got := s.acquiredLanguageName(); got != "" {
		t.Errorf("primary = %q, want none — the server could not start", got)
	}
	info := sessionRecord(t, s.sessID)
	if want := "python, typescript"; info.DetectedLanguage != want {
		t.Errorf("session DetectedLanguage = %q, want %q — a failed acquire must not erase "+
			"what was discovered; routing retries on first file", info.DetectedLanguage, want)
	}
}
