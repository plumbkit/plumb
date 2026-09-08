package cli

// project_lsp_policy_test.go — a project's [lsp.<lang>] block decides which
// language servers ITS workspace may run, and every question the pool asks about
// languages is answered against that same set.
//
// The seams covered here are the ones that each used to consult the daemon's
// global set independently: strong-marker detection, weak-marker detection, the
// content sniff, child-root discovery, per-file routing, the explicit
// session_start language override, and server acquisition. A fix applied to one
// of them and not the others presents as a workspace whose detection and routing
// disagree — the language attaches for some tools and not others.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/paths"
)

// projectPolicyPool builds a pool over a REAL config snapshot (so project
// resolution runs, unlike the narrow detection fixtures) whose named languages
// are installed but DISABLED globally. Enabling them is then something only a
// project config can do, which is the whole shape under test.
func projectPolicyPool(t *testing.T, disabled ...string) *workspacePool {
	t.Helper()
	cfg := config.Defaults()
	lsp := map[string]config.LSPConfig{}
	for _, name := range disabled {
		base := cfg.LSP[name]
		base.Command = os.Args[0] // installed, so only Enabled decides
		base.Enabled = false
		lsp[name] = base
	}
	cfg.LSP = lsp
	return newWorkspacePool(t.Context(), cfg)
}

// TestProjectLSP_EnableWidensStrongMarkerDetection is the reported defect at its
// simplest: a manifest IS present, the adapter IS installed, and the project
// asks for it — but the daemon's global config says no, and before the shared
// resolver the global answer was the only one detection ever heard.
func TestProjectLSP_EnableWidensStrongMarkerDetection(t *testing.T) {
	pool := projectPolicyPool(t, "python")
	root := paths.Canonical(t.TempDir())
	mustWrite(t, filepath.Join(root, "pyproject.toml"), "")

	if _, language, _ := pool.Detect(root); language == "python" {
		t.Fatal("premise broken: python resolved before the project enabled it")
	}
	writePoolProjectConfig(t, root, "[lsp.python]\nenabled = true\n")
	_, language, err := pool.Detect(root)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if language != "python" {
		t.Errorf("Detect = %q, want python — the project's own config enables it", language)
	}
}

// TestProjectLSP_EnableWidensWeakMarkerDetection covers the other marker class.
// Weak markers run through a different tie-break path (keepPartialCount), so a
// resolver threaded through the strong path alone leaves this one global.
func TestProjectLSP_EnableWidensWeakMarkerDetection(t *testing.T) {
	pool := projectPolicyPool(t, "typescript")
	root := paths.Canonical(t.TempDir())
	writePoolProjectConfig(t, root, "[lsp.typescript]\nenabled = true\n")
	mustWrite(t, filepath.Join(root, "package.json"), "{}")
	mustWrite(t, filepath.Join(root, "index.ts"), "export const a = 1")

	if _, language, _ := pool.Detect(root); language != "typescript" {
		t.Errorf("Detect = %q, want typescript from the project-enabled weak marker", language)
	}
}

// TestProjectLSP_DisableFallsBackToAnotherEligibleLanguage is the narrowing
// direction, and it has to run BEFORE detection rather than filter its answer:
// filtering afterwards yields "none", where the project actually still has a
// language plumb can serve.
func TestProjectLSP_DisableFallsBackToAnotherEligibleLanguage(t *testing.T) {
	cfg := config.Defaults()
	lsp := map[string]config.LSPConfig{}
	for _, name := range []string{"go", "python"} {
		c := cfg.LSP[name]
		c.Command = os.Args[0]
		c.Enabled = true
		lsp[name] = c
	}
	cfg.LSP = lsp
	pool := newWorkspacePool(t.Context(), cfg)

	root := paths.Canonical(t.TempDir())
	mustWrite(t, filepath.Join(root, "go.mod"), "module x\n")
	mustWrite(t, filepath.Join(root, "pyproject.toml"), "")
	writePoolProjectConfig(t, root, "[lsp.go]\nenabled = false\n")

	if _, language, _ := pool.Detect(root); language != "python" {
		t.Errorf("Detect = %q, want python — go is disabled here, and python's marker is right beside it", language)
	}
}

// TestProjectLSP_DisableRefusesAnAlreadyPooledEntry pins the acquisition gate's
// ORDER. Checking eligibility only on the map miss made the refusal depend on
// whether the daemon happened to have started the server before the config
// changed — the same project, the same request, two answers.
func TestProjectLSP_DisableRefusesAnAlreadyPooledEntry(t *testing.T) {
	pool := projectPolicyPool(t, "python")
	root := paths.Canonical(t.TempDir())
	writePoolProjectConfig(t, root, "[lsp.python]\nenabled = true\n")
	installEntryLang(pool, root, "python", &stubClient{id: "python"})

	if _, _, err := pool.startOrReuse(root, "python", false); err != nil {
		t.Fatalf("premise broken: the pooled entry was not reusable while enabled: %v", err)
	}
	writePoolProjectConfig(t, root, "[lsp.python]\nenabled = false\n")
	if _, _, err := pool.startOrReuse(root, "python", false); err == nil {
		t.Error("a project that disabled python was still handed the pooled python server")
	}
}

// TestProjectLSP_NestedBoundaryDoesNotInheritEnables is the containment half of
// the parent-governs-children rule. A vendored checkout or a submodule under the
// workspace declares its own boundary, and a project cannot enable a language
// server on behalf of code it merely contains.
func TestProjectLSP_NestedBoundaryDoesNotInheritEnables(t *testing.T) {
	pool := projectPolicyPool(t, "python", "typescript")
	root := paths.Canonical(t.TempDir())
	writePoolProjectConfig(t, root, "[lsp.python]\nenabled = true\n[lsp.typescript]\nenabled = true\n")

	inherited := filepath.Join(root, "app")
	mustMkdir(t, inherited)
	mustWrite(t, filepath.Join(inherited, "tsconfig.json"), "{}")

	vendored := filepath.Join(root, "vendored")
	mustMkdir(t, vendored)
	mustMkdir(t, filepath.Join(vendored, ".git"))
	mustWrite(t, filepath.Join(vendored, "pyproject.toml"), "")

	got := pool.discoverChildLanguages(root, 2)
	roots := make([]string, 0, len(got))
	for _, d := range got {
		roots = append(roots, d.root)
	}
	if !slices.Contains(roots, inherited) {
		t.Errorf("child discovery = %v, want the language subroot %s — it inherits the parent project's enables", got, inherited)
	}
	if slices.Contains(roots, vendored) {
		t.Errorf("child discovery = %v, want %s excluded — its own boundary means it does not inherit", got, vendored)
	}
}

// TestProjectLSP_KnownRootKeepsItsOwnPolicyWhileAscending is requirement 6. The
// ancestor walk that names a language for an ALREADY-FIXED root must keep that
// root's policy: re-resolving per ancestor would let a directory above the
// workspace decide what the workspace's own config had settled.
func TestProjectLSP_KnownRootKeepsItsOwnPolicyWhileAscending(t *testing.T) {
	pool := projectPolicyPool(t, "python")
	outer := paths.Canonical(t.TempDir())
	mustMkdir(t, filepath.Join(outer, ".git"))
	mustWrite(t, filepath.Join(outer, "pyproject.toml"), "")
	writePoolProjectConfig(t, outer, "[lsp.python]\nenabled = false\n")

	inner := filepath.Join(outer, "inner")
	mustMkdir(t, inner)
	writePoolProjectConfig(t, inner, "[lsp.python]\nenabled = true\n")

	// The marker lives in the ANCESTOR; the policy must come from inner, which is
	// the root being resolved.
	if got := pool.languageForRoot(inner); got != "python" {
		t.Errorf("languageForRoot(inner) = %q, want python — inner enables it, and inner is the root being resolved", got)
	}
	if got := pool.languageForRoot(outer); got != LanguageNone {
		t.Errorf("languageForRoot(outer) = %q, want none — outer disables python for itself", got)
	}
}

// TestProjectLSP_ConfigCreateChangeDeleteTakeEffect is the no-watcher case: a
// CLI invocation, a detection-only path, or a child root nothing has pinned has
// no live watcher to invalidate anything, so the resolver must notice the file
// moving on its own. All three transitions, because a cache keyed on "the file's
// mtime when it existed" gets creation and deletion wrong in opposite ways.
func TestProjectLSP_ConfigCreateChangeDeleteTakeEffect(t *testing.T) {
	pool := projectPolicyPool(t, "python", "typescript")
	root := paths.Canonical(t.TempDir())
	mustMkdir(t, filepath.Join(root, ".git"))
	mustWrite(t, filepath.Join(root, "pyproject.toml"), "")
	mustWrite(t, filepath.Join(root, "tsconfig.json"), "{}")

	if _, language, _ := pool.Detect(root); language != LanguageNone {
		t.Fatalf("premise broken: language %q resolved with nothing enabled", language)
	}
	writePoolProjectConfig(t, root, "[lsp.python]\nenabled = true\n")
	if _, language, _ := pool.Detect(root); language != "python" {
		t.Errorf("after create: Detect = %q, want python", language)
	}
	writePoolProjectConfig(t, root, "[lsp.typescript]\nenabled = true\n")
	if _, language, _ := pool.Detect(root); language != "typescript" {
		t.Errorf("after change: Detect = %q, want typescript", language)
	}
	if err := os.Remove(filepath.Join(root, ".plumb", "config.toml")); err != nil {
		t.Fatal(err)
	}
	if _, language, _ := pool.Detect(root); language != LanguageNone {
		t.Errorf("after delete: Detect = %q, want none — the enable went with the file", language)
	}
}

// TestProjectLSP_TrustChangeInvalidatesTheResolution is why the cached
// resolution is stamped with the TRUST STORE and not only the project file.
// LoadProject forces a project's exec-deciding [lsp.<lang>] fields back to the
// global config until the user trusts that exact content, so `plumb trust`
// changes the resolved adapter with the project file untouched. A cache keyed on
// the file alone keeps serving the untrusted projection after the grant.
func TestProjectLSP_TrustChangeInvalidatesTheResolution(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	pool := projectConfigPool(t)
	root := paths.Canonical(t.TempDir())
	writePoolProjectConfig(t, root, "[lsp.go]\ncommand = \"/bin/echo\"\n")

	got, ok := pool.cfgForWorkspace(root, "go")
	if !ok {
		t.Fatal("go adapter was not resolved")
	}
	if got.Command != os.Args[0] {
		t.Fatalf("premise broken: untrusted project command = %q, want the global %q", got.Command, os.Args[0])
	}

	spec, err := config.ProjectPolicySpecFor(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.NewTrustStore().SetTrustedForProject(root, nil, spec); err != nil {
		t.Fatal(err)
	}
	got, ok = pool.cfgForWorkspace(root, "go")
	if !ok {
		t.Fatal("go adapter was not resolved after the grant")
	}
	if got.Command != "/bin/echo" {
		t.Errorf("trusted project command = %q, want /bin/echo — the grant moved, the cache did not", got.Command)
	}
}

// TestProjectLSP_EnableLanguageInvalidatesProjectResolutions covers the other
// direction of staleness: every cached per-project resolution was merged onto
// the base config, so a live `enable-lsp` makes all of them stale at once with
// no project file moving.
func TestProjectLSP_EnableLanguageInvalidatesProjectResolutions(t *testing.T) {
	cfg := config.Defaults()
	goCfg := cfg.LSP["go"]
	goCfg.Command = os.Args[0]
	goCfg.Enabled = true
	htmlCfg := cfg.LSP["html"]
	htmlCfg.Command = os.Args[0]
	htmlCfg.Enabled = false
	cfg.LSP = map[string]config.LSPConfig{"go": goCfg, "html": htmlCfg}
	pool := newWorkspacePool(t.Context(), cfg)

	root := paths.Canonical(t.TempDir())
	writePoolProjectConfig(t, root, "[lsp.go]\ndiagnostics = \"pull\"\n")
	if _, ok := pool.cfgForWorkspace(root, "html"); ok {
		t.Fatal("premise broken: html resolved before enable-lsp")
	}
	if _, err := pool.enableLanguage("html"); err != nil {
		t.Fatalf("enableLanguage: %v", err)
	}
	if _, ok := pool.cfgForWorkspace(root, "html"); !ok {
		t.Error("enable-lsp did not reach this project's cached resolution")
	}
	// The project's own knob must survive the invalidation, not be replaced by
	// the global config it was merged onto.
	if got, _ := pool.cfgForWorkspace(root, "go"); got.Diagnostics != "pull" {
		t.Errorf("project diagnostics = %q after enable-lsp, want the project's pull", got.Diagnostics)
	}
}

// TestProjectLSP_ResolutionRacesEnableLanguage is the concurrency guard for the
// two locks this fix added around a config the daemon can replace live. Run
// under -race; the assertion is that nothing tears, deadlocks, or resolves an
// empty set.
func TestProjectLSP_ResolutionRacesEnableLanguage(t *testing.T) {
	cfg := config.Defaults()
	goCfg := cfg.LSP["go"]
	goCfg.Command = os.Args[0]
	goCfg.Enabled = true
	htmlCfg := cfg.LSP["html"]
	htmlCfg.Command = os.Args[0]
	htmlCfg.Enabled = false
	cfg.LSP = map[string]config.LSPConfig{"go": goCfg, "html": htmlCfg}
	pool := newWorkspacePool(context.Background(), cfg)

	roots := make([]string, 4)
	for i := range roots {
		roots[i] = paths.Canonical(t.TempDir())
		writePoolProjectConfig(t, roots[i], fmt.Sprintf("[lsp.go]\nmax_workspaces = %d\n", i+1))
	}

	var wg sync.WaitGroup
	for _, root := range roots {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				if langs := pool.effectiveLanguages(root); len(langs) == 0 {
					t.Errorf("effectiveLanguages(%s) resolved an empty set", root)
					return
				}
				_ = pool.fileLanguage(filepath.Join(root, "main.go"))
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := pool.enableLanguage("html"); err != nil {
			t.Errorf("enableLanguage: %v", err)
		}
	}()
	wg.Wait()

	if _, ok := pool.cfgForWorkspace(roots[0], "html"); !ok {
		t.Error("html was not visible to project resolution after the concurrent enable")
	}
}

// TestProjectLSP_OverrideAcceptedWhenTheProjectEnablesIt pins the session_start
// language override against the same set. Refusing here would tell an agent to
// edit config that already says exactly what it asked for.
func TestProjectLSP_OverrideAcceptedWhenTheProjectEnablesIt(t *testing.T) {
	pool := projectPolicyPool(t, "python")
	root := paths.Canonical(t.TempDir())
	writePoolProjectConfig(t, root, "[lsp.python]\nenabled = true\n")
	s := &connSession{store: config.NewStore(config.Defaults()), ctx: t.Context(), pool: pool}

	if err := s.languageOverrideErr(root, "python"); err != nil {
		t.Errorf("override refused for a language this project enables: %v", err)
	}
}

// TestProjectLSP_OverrideRefusalNamesTheProjectWhenItDisables is the fourth
// refusal case the project-scoped set creates. Reusing the global wording here
// would send the caller to a config file that is already correct.
func TestProjectLSP_OverrideRefusalNamesTheProjectWhenItDisables(t *testing.T) {
	cfg := config.Defaults()
	goCfg := cfg.LSP["go"]
	goCfg.Command = os.Args[0]
	goCfg.Enabled = true
	cfg.LSP = map[string]config.LSPConfig{"go": goCfg}
	pool := newWorkspacePool(t.Context(), cfg)

	root := paths.Canonical(t.TempDir())
	writePoolProjectConfig(t, root, "[lsp.go]\nenabled = false\n")
	s := &connSession{store: config.NewStore(cfg), ctx: t.Context(), pool: pool}

	err := s.languageOverrideErr(root, "go")
	if err == nil {
		t.Fatal("expected a refusal: this project turned the language off")
	}
	if !strings.Contains(err.Error(), ".plumb/config.toml") {
		t.Errorf("refusal %q should point at the project config, not the global one", err)
	}
	if strings.Contains(err.Error(), "not installed") || strings.Contains(err.Error(), "enable-lsp") {
		t.Errorf("refusal %q must not report a state that is not true", err)
	}
}

// TestProjectLSP_InvalidConfigIsCachedUnderItsOwnStamp guards a regression this
// fix could easily have shipped. The old cfgForWorkspace ran only when a server
// was being STARTED; the shared resolver runs several times per request —
// detection, per-file routing, and the acquisition gate all ask. An uncached
// failure therefore reparsed a broken .plumb/config.toml and logged a warning on
// every one of them, for the life of the daemon.
//
// The cache entry IS the assertion: a resolver that refuses to cache a failure
// leaves the map empty no matter how many times it is asked. Recovery is
// asserted in the same test, because caching a failure is only safe if the
// stamp still expires it.
func TestProjectLSP_InvalidConfigIsCachedUnderItsOwnStamp(t *testing.T) {
	pool := projectPolicyPool(t, "python")
	root := paths.Canonical(t.TempDir())
	mustWrite(t, filepath.Join(root, "pyproject.toml"), "")
	writePoolProjectConfig(t, root, "[lsp.python\nnot valid toml")

	for range 3 {
		if _, language, _ := pool.Detect(root); language != LanguageNone {
			t.Fatalf("Detect = %q with an unparseable project config, want the global answer", language)
		}
	}
	pool.languageConfig.mu.RLock()
	cached, ok := pool.languageConfig.cache[root]
	pool.languageConfig.mu.RUnlock()
	if !ok {
		t.Fatal("an unparseable project config was not cached — every request reparses it and re-warns")
	}
	if len(cached.langs) != 0 {
		t.Errorf("cached fallback = %v, want the global set (python is globally disabled there)", cached.langs)
	}

	// Repairing the file must beat the cached failure. Nothing else invalidates a
	// project no session is attached to, so the stamp is the only thing that can.
	writePoolProjectConfig(t, root, "[lsp.python]\nenabled = true\n")
	if _, language, _ := pool.Detect(root); language != "python" {
		t.Errorf("after repair: Detect = %q, want python — the cached failure outlived the fix", language)
	}
}

// TestProjectLSP_RoutingResolvesThePolicyOnce is the regression guard for a cost
// that is invisible in every return value, and which this fix shipped once.
//
// Resolving a project's language set walks ancestors to find the policy root and
// stats the config and trust files — on a cache HIT as much as a miss; the cache
// only elides the TOML parse. Routing originally asked twice per request, once
// inside Detect and again in the bare fileLanguage, which doubled that walk and
// bought nothing: Detect had already resolved the very set fileLanguage needed.
// An independent review measured it at ~135µs on a deep file where the old
// global-slice lookup was ~30ns.
//
// Asserted with a counter rather than a benchmark, deliberately. On a loaded
// machine the run-to-run variance of this path is larger than the regression —
// measured at 1.4ms to 5.9ms per op with the before/after ordering inverting
// between runs — so a timing assertion would be either flaky or asleep. The
// number of resolutions is exact and does not care what else the machine is
// doing.
func TestProjectLSP_RoutingResolvesThePolicyOnce(t *testing.T) {
	pool := projectPolicyPool(t, "python")
	root := paths.Canonical(t.TempDir())
	writePoolProjectConfig(t, root, "[lsp.python]\nenabled = true\n")
	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(deep, "mod.py")
	mustWrite(t, file, "value = 1")

	// Warm the cache first, so this counts resolutions and not first-parse work.
	pool.resolveFileTarget(file) //nolint:errcheck // asserted below

	before := pool.langResolves.Load()
	gotRoot, language, err := pool.resolveFileTarget(file)
	if err != nil {
		t.Fatalf("resolveFileTarget: %v", err)
	}
	if gotRoot != root || language != "python" {
		t.Fatalf("resolveFileTarget = (%q, %q), want (%q, python)", gotRoot, language, root)
	}
	if got := pool.langResolves.Load() - before; got != 1 {
		t.Errorf("one routing decision cost %d policy resolutions, want exactly 1 — "+
			"each extra one re-walks the ancestor chain and re-stats the project config", got)
	}
}

// TestProjectLanguageStamp_CoversBothConfigAndTrust is the unit-level guard for
// the two halves of the stamp. Each half is what makes one class of change
// visible without a restart; dropping either leaves a cached resolution serving
// an answer the user has already changed.
func TestProjectLanguageStamp_CoversBothConfigAndTrust(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	root := t.TempDir()
	empty := projectLanguageStamp(root)

	writePoolProjectConfig(t, root, "[lsp.go]\nenabled = true\n")
	withConfig := projectLanguageStamp(root)
	if withConfig == empty {
		t.Error("creating .plumb/config.toml did not change the stamp")
	}

	if err := os.MkdirAll(config.DataDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config.DataDir(), "trust.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if withTrust := projectLanguageStamp(root); withTrust == withConfig {
		t.Error("writing the trust store did not change the stamp — a `plumb trust` grant would not be picked up")
	}
}
