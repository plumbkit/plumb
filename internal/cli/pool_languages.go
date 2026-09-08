package cli

// pool_languages.go — the EFFECTIVE language set, resolved per project.
//
// The daemon's global config names a base set of enabled+installed language
// servers. A project's .plumb/config.toml may widen it ([lsp.python] enabled =
// true on a daemon whose global config leaves Python off) or narrow it. Every
// question the pool asks about languages — workspace detection, the content
// sniff, child-root discovery, per-file routing, an explicit session_start
// language override, and server acquisition — must be answered against the SAME
// set, or the answers contradict each other: detection would refuse a language
// routing then tries to start, or routing would start one detection never saw.
//
// The set is therefore resolved once per operation, from the governing project,
// and threaded through that operation as an immutable slice. Resolution is
// cached per policy root and revalidated by a cheap stat, so a bounded scan over
// thousands of files never reparses TOML or re-walks ancestors per file.

import (
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/paths"
)

// fileStamp is a file's cheap identity — enough to notice a change, a creation,
// or a deletion without reading the file. A missing file stamps as the zero
// value, so create and delete both flip it.
type fileStamp struct {
	modTime int64
	size    int64
	exists  bool
}

func statStamp(path string) fileStamp {
	info, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{modTime: info.ModTime().UnixNano(), size: info.Size(), exists: true}
}

// languageConfigStamp is what a cached resolution is valid FOR. Both halves are
// needed. The project config is the obvious one. The trust store is the
// non-obvious one: LoadProject forces a project's exec-deciding [lsp.<lang>]
// fields (command, args, env, root markers) back to the global config unless the
// user has trusted that exact content, so `plumb trust` changes the resolved
// LSPConfig with the project file untouched. Stamping only the project file
// would serve the untrusted projection until something else happened to
// invalidate it.
type languageConfigStamp struct {
	project fileStamp
	trust   fileStamp
}

// cachedWorkspaceLanguages is one project's resolved active (enabled +
// installed) language set, and the file identities it was resolved from.
type cachedWorkspaceLanguages struct {
	stamp languageConfigStamp
	langs []langConfig
}

// languageConfigState protects the small cache of project-resolved languages. It
// is separate from workspacePool.mu on purpose: startOrReuse holds that mutex
// while resolving config, and enableLanguage holds it while writing baseConfig,
// so a resolver that took p.mu would deadlock the first and invert against the
// second.
//
// LOCK ORDER, load-bearing in both directions.
//
// Acquisition order is p.mu -> languageConfigState.mu -> langsMu, and no path
// nests them any other way. effectiveLanguages holds this mutex and takes
// langsMu.RLock inside it (snapshotBaseConfig). enableLanguage takes p.mu, then
// langsMu, RELEASES langsMu, and only then takes this mutex. That release is not
// tidiness: moving invalidateAllLanguageConfigs inside the langsMu critical
// section would leave one goroutine holding langsMu and wanting this mutex while
// another holds this mutex and wants langsMu, which is the cycle.
//
// Separately, the SCOPE of this mutex — not the position of any other lock — is
// what closes the stale-write window, and that distinction decides what a future
// change may do. effectiveLanguages takes this lock BEFORE reading the base
// config and holds it through the cache write. invalidateAllLanguageConfigs
// needs the same lock, so it cannot land between that read and that write: it
// runs wholly before (the resolver then reads the widened base) or wholly after
// (the resolver's entry is swept). Were the read outside, a resolution computed
// from base version N could be cached under a stamp taken before N+1 was
// published, surviving the very invalidation meant to discard it — and surviving
// FOREVER, because the project file never changed, so the stamp never flips to
// repair it.
//
// Two natural-looking refactors would reopen that, so they are named here rather
// than left to be rediscovered: serving a resolution from the RLock fast path,
// and hoisting snapshotBaseConfig above the Lock to get LoadProject's filesystem
// I/O out from under a write lock. Neither is a data race, so -race stays quiet
// and the failure is a lost update, not a crash.
type languageConfigState struct {
	mu    sync.RWMutex
	cache map[string]cachedWorkspaceLanguages
}

func projectLanguageStamp(root string) languageConfigStamp {
	return languageConfigStamp{
		project: statStamp(filepath.Join(root, ".plumb", "config.toml")),
		trust:   statStamp(filepath.Join(config.DataDir(), "trust.json")),
	}
}

// activeLanguages is the effective set of a resolved config: the user's intent
// (LSPConfig.Enabled) gated on the server actually being installed, in the
// pool's deterministic order. An enabled-but-uninstalled language is excluded so
// its root markers never pollute workspace detection.
func activeLanguages(cfg config.Config) []langConfig {
	langs := make([]langConfig, 0, len(cfg.LSP))
	for name, lspCfg := range cfg.LSP {
		if lspActive(lspCfg) {
			langs = append(langs, langConfig{name: name, cfg: lspCfg})
		}
	}
	sortLangs(langs)
	return langs
}

// languagePolicyRoot returns the project whose .plumb/config.toml governs LSP
// enablement for start: the nearest explicit boundary at or above it. Returns ""
// when there is none, in which case the global set governs.
//
// "Explicit" means a .plumb marker or a .git directory — the two things that
// unambiguously declare "a project starts here". A LANGUAGE root marker
// (go.mod, tsconfig.json) deliberately does not, because that is the whole
// monorepo case this fix exists for: app/tsconfig.json is a language subroot of
// the project above it, and must inherit that project's enablement rather than
// starting a policy scope of its own. A nested .plumb or .git does start one, so
// a vendored dependency or a submodule cannot inherit its host's enables.
//
// The home directory bounds the walk exactly as Detect's does, and for the same
// reason: a stray ~/.git must not make every project beneath $HOME resolve its
// policy from the home directory. Only a deliberate marker (one carrying a
// context.md) counts at $HOME itself.
func languagePolicyRoot(start string) string {
	homeInfo := homeDirInfos()
	for dir := filepath.Clean(start); ; dir = filepath.Dir(dir) {
		// sameDirAs stats dir, and BOTH questions on this rung need its answer:
		// "is this a boundary" (a .plumb AT $HOME counts only when deliberate) and
		// "must the walk stop here". Asking once per rung rather than once per
		// question removes one stat per ancestor level from a walk that runs on
		// every routing request. See languageBoundaryKnownHome.
		atHome := sameDirAs(dir, homeInfo)
		if languageBoundaryKnownHome(dir, atHome) {
			return paths.Canonical(dir)
		}
		if atHome || filepath.Dir(dir) == dir {
			return ""
		}
	}
}

// languageBoundaryAtHome reports whether dir itself declares a project boundary
// — a .plumb marker or a .git directory. Used by the child-root scan to decide
// whether a subdirectory inherits its parent's language policy or starts its
// own.
//
// homeInfo is passed in rather than derived, deliberately: homeDirInfos is
// uncached, and every caller asks this question in a loop over directories.
func languageBoundaryAtHome(dir string, homeInfo []os.FileInfo) bool {
	return languageBoundaryKnownHome(dir, sameDirAs(dir, homeInfo))
}

// languageBoundaryKnownHome is languageBoundaryAtHome for a caller that has
// ALREADY established whether dir is the home directory. Split out because
// sameDirAs stats dir, and languagePolicyRoot needs that same answer for its own
// termination test on every rung — so deriving it twice per rung doubled the
// per-level stat cost of the walk for nothing.
func languageBoundaryKnownHome(dir string, atHome bool) bool {
	if _, err := os.Stat(filepath.Join(dir, ".plumb")); err == nil && (!atHome || deliberatePlumbMarker(dir)) {
		return true
	}
	if atHome {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// effectiveLanguages resolves the active languages governing root. The returned
// slice is immutable and shared — callers that scan many files resolve ONCE and
// thread it through the whole operation rather than calling this per file.
//
// A pool built without a full config snapshot (the narrow detection-only
// fixtures) has no base to merge a project config onto; its language slice is
// already the resolved source of truth, and feeding a zero Config into
// LoadProject would fail validation. LogLevel is the sentinel for that, matching
// what cfgForWorkspace used before this became a shared resolver.
func (p *workspacePool) effectiveLanguages(root string) []langConfig {
	p.langResolves.Add(1)
	// LogLevel is written once at construction and never mutated, so it needs no
	// lock; the maps and slices behind it do (see langsSnapshot). Safe only while
	// nothing assigns p.baseConfig WHOLE — .LSP is replaced field-wise by
	// enableLanguage for exactly this reason.
	if p.baseConfig.LogLevel == "" {
		return p.langsSnapshot()
	}
	policyRoot := languagePolicyRoot(root)
	if policyRoot == "" {
		return p.langsSnapshot()
	}
	stamp := projectLanguageStamp(policyRoot)

	p.languageConfig.mu.RLock()
	cached, ok := p.languageConfig.cache[policyRoot]
	p.languageConfig.mu.RUnlock()
	if ok && cached.stamp == stamp {
		return cached.langs
	}

	p.languageConfig.mu.Lock()
	defer p.languageConfig.mu.Unlock()
	if cached, ok = p.languageConfig.cache[policyRoot]; ok && cached.stamp == stamp {
		return cached.langs
	}
	langs, err := p.resolveProjectLanguages(policyRoot)
	if err != nil {
		// Same degradation as everywhere else a project config will not parse: the
		// global config applies whole.
		//
		// CACHED, under the failing file's own stamp, and that is the whole point.
		// This resolver runs several times per request — Detect, per-file routing,
		// and the acquisition gate all ask — where the old cfgForWorkspace ran only
		// when a server was actually being started. Leaving the failure uncached
		// reparsed the broken TOML and logged this warning on every one of them,
		// forever, turning a typo in a .plumb/config.toml into a permanent log flood
		// and a per-request parse. The stamp already flips when the file is
		// repaired, so caching costs nothing in recovery time.
		slog.Warn("pool: project config invalid; using global LSP config", "root", policyRoot, "err", err)
		langs = p.langsSnapshot()
	}
	resolved := cachedWorkspaceLanguages{stamp: stamp, langs: langs}
	if p.languageConfig.cache == nil {
		p.languageConfig.cache = make(map[string]cachedWorkspaceLanguages)
	}
	p.languageConfig.cache[policyRoot] = resolved
	return resolved.langs
}

// resolveProjectLanguages merges the project config at policyRoot onto the base
// and returns its active set. Split out so the caller's error branch is one
// decision — what to cache when this fails — rather than three statements.
func (p *workspacePool) resolveProjectLanguages(policyRoot string) ([]langConfig, error) {
	project, err := config.LoadProject(p.snapshotBaseConfig(), policyRoot)
	if err != nil {
		return nil, err
	}
	return activeLanguages(project), nil
}

// snapshotBaseConfig reads the pool's base config under the langs lock.
// enableLanguage publishes the widened LSP map and the widened langs slice in
// one langsMu critical section, so taking the read lock here is what stops a
// resolver from merging a project config onto a half-updated base. It never
// takes p.mu — see languageConfigState.
func (p *workspacePool) snapshotBaseConfig() config.Config {
	p.langsMu.RLock()
	defer p.langsMu.RUnlock()
	return p.baseConfig
}

// invalidatePoolLanguages is applyProjectConfig's nil-safe entry to
// invalidateLanguageConfig. A method rather than an inline guard because
// applyProjectConfig sits at the gocyclo limit, and "this session has no pool
// wired" is a degraded-start fact, not a decision belonging in a reload sequence.
func (s *connSession) invalidatePoolLanguages(workspace string) {
	if s.pool == nil {
		return
	}
	s.pool.invalidateLanguageConfig(workspace)
}

// invalidateLanguageConfig drops the cached resolution for one project and bumps
// the language generation, so a live session whose primary never resolved
// re-detects against the new set (see connSession.refreshPrimaryIfStale). Called
// from the project-config reload lane; the stamp alone would already give a
// correct answer to the next question asked, but nothing would prompt a session
// to ask one.
//
// The generation is a GLOBAL wakeup, not a targeted one, and the asymmetry is
// worth naming: the cache drop is per project, but langsGen is pool-wide and
// refreshPrimaryIfStale claims the generation before it checks whether this
// session needs anything. So a config edit in one project costs every session in
// every project one re-Detect. That is cheap (a filesystem walk, once) and
// simpler than a per-project generation map, but a reader should not infer a
// targeted wakeup from a per-project invalidation.
func (p *workspacePool) invalidateLanguageConfig(root string) {
	// Keyed on the POLICY root, not on the caller's root, because those are not
	// always the same directory: detect() stops at a strong language marker, so a
	// session can be pinned at repo/app (go.mod) while the config governing its
	// languages is repo/.plumb/config.toml. Deleting under the session's own root
	// would remove a key nothing ever wrote and leave the real entry standing.
	// Falls back to the caller's spelling when no project governs it, which is a
	// no-op delete either way.
	policyRoot := languagePolicyRoot(root)
	if policyRoot == "" {
		policyRoot = paths.Canonical(root)
	}
	p.languageConfig.mu.Lock()
	delete(p.languageConfig.cache, policyRoot)
	p.languageConfig.mu.Unlock()
	p.langsGen.Add(1)
}

// invalidateAllLanguageConfigs drops every cached resolution. Used when the
// BASE config changes (live enable-lsp), which every project's resolution is
// merged onto. The caller bumps the generation.
func (p *workspacePool) invalidateAllLanguageConfigs() {
	p.languageConfig.mu.Lock()
	clear(p.languageConfig.cache)
	p.languageConfig.mu.Unlock()
}

// cfgAmong looks language up in an effective-language slice. Linear rather than
// a binary search: the slice holds one entry per configured adapter (a dozen at
// most), and it is assembled by hand in several tests, so depending on
// sortLangs' "go first, then alphabetical" order here would make a fixture's
// ordering a correctness question instead of a cosmetic one.
func cfgAmong(langs []langConfig, language string) (config.LSPConfig, bool) {
	for _, l := range langs {
		if l.name == language {
			return l.cfg, true
		}
	}
	return config.LSPConfig{}, false
}
