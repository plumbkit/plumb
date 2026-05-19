package cli

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/golimpio/plumb/internal/cache"
	"github.com/golimpio/plumb/internal/config"
	"github.com/golimpio/plumb/internal/lsp"
	"github.com/golimpio/plumb/internal/lsp/adapters/gopls"
	"github.com/golimpio/plumb/internal/lsp/adapters/jdtls"
	"github.com/golimpio/plumb/internal/lsp/adapters/pyright"
	"github.com/golimpio/plumb/internal/lsp/jsonrpc"
	"github.com/golimpio/plumb/internal/lsp/protocol"
)

// workspacePool keeps one language-server process alive per workspace root.
// Multiple MCP sessions targeting the same root share a single LS process,
// its cache, and its diagnostic stream.
//
// The pool supports multiple languages (Go via gopls, Python via pyright).
// Detect() resolves a path → (root, language) tuple from configured root
// markers; acquireLang() starts the right adapter for that language.
//
// Concurrency: all methods are safe for concurrent use.
type workspacePool struct {
	mu       sync.Mutex
	entries  map[string]*poolEntry // key: root path; one LS per root
	langs    []langConfig          // enabled languages, deterministic order
	cacheTTL time.Duration
}

type langConfig struct {
	name string
	cfg  config.LSPConfig
}

type poolEntry struct {
	root     string
	language string
	proxy    *clientProxy
	inv      *cache.Invalidator
	cache    *cache.Cache
	sup      *lsp.Supervisor
}

func newWorkspacePool(cfg config.Config) *workspacePool {
	var langs []langConfig
	for name, lspCfg := range cfg.LSP {
		if lspCfg.Enabled {
			langs = append(langs, langConfig{name: name, cfg: lspCfg})
		}
	}
	// Deterministic order: "go" first for backward compatibility, then alphabetical.
	sort.Slice(langs, func(i, j int) bool {
		if langs[i].name == "go" {
			return true
		}
		if langs[j].name == "go" {
			return false
		}
		return langs[i].name < langs[j].name
	})
	return &workspacePool{
		entries:  make(map[string]*poolEntry),
		langs:    langs,
		cacheTTL: cfg.Cache.TTL.Duration,
	}
}

// LanguageNone is the sentinel language returned by Detect for workspaces
// that are explicitly marked (via .plumb/) but have no enabled LSP language.
// Filesystem tools, stats attribution, and project config all still work for
// these workspaces; LSP tools fail with "LSP server not yet ready".
const LanguageNone = "none"

// Detect walks up from start looking for a workspace root, with three
// fallbacks tried in order at each directory:
//
//  1. A `.plumb/` marker. If an LSP language is also detectable from this
//     directory or any ancestor, return (root, language). Otherwise return
//     (root, "none") — the user marked this directory as a workspace, so we
//     respect that even without LSP support.
//  2. A configured language's root marker (`go.mod`, `pyproject.toml`, ...).
//     Returns (root, language).
//
// If neither is found, walk up to the parent. If we walk past the filesystem
// root, return an error.
func (p *workspacePool) Detect(start string) (root, language string, err error) {
	d := start
	for {
		// Highest priority: explicit .plumb marker. Honour it even when no
		// LSP language matches — the user has declared this directory a
		// plumb workspace, and stats / project config should follow that
		// declaration regardless of whether gopls or pyright can attach.
		if _, err := os.Stat(filepath.Join(d, ".plumb")); err == nil {
			if lang := p.detectLanguageAt(d); lang != "" {
				return d, lang, nil
			}
			return d, LanguageNone, nil
		}
		// Otherwise: first language whose root marker exists.
		for _, l := range p.langs {
			for _, marker := range l.cfg.RootMarkers {
				if _, err := os.Stat(filepath.Join(d, marker)); err == nil {
					return d, l.name, nil
				}
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", "", fmt.Errorf("no project root found in or above %s", start)
		}
		d = parent
	}
}

// SynthesiseRoot returns a synthetic workspace root for seedDir, used as a
// last resort when Detect has already failed. It walks up from seedDir
// looking for a .git directory (the conventional project-root signal for
// unrecognised languages). If found, that directory is returned. If the
// filesystem root is reached without finding .git, seedDir itself is
// returned as the safest approximation.
//
// SynthesiseRoot must only be called on the Detect error path in
// OnBeforeTool — never inside route() or LSP-routing paths.
func (p *workspacePool) SynthesiseRoot(seedDir string) string {
	d := seedDir
	for {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return seedDir // reached filesystem root — use the seed itself
		}
		d = parent
	}
}

// detectLanguageAt returns the language for dir based on which root marker
// is present at dir or any ancestor. Used after a .plumb/ marker is found
// to determine which adapter to start.
func (p *workspacePool) detectLanguageAt(dir string) string {
	d := dir
	for {
		for _, l := range p.langs {
			for _, marker := range l.cfg.RootMarkers {
				if _, err := os.Stat(filepath.Join(d, marker)); err == nil {
					return l.name
				}
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
	}
}

// resolveCLIWorkspace resolves start to the same workspace root the daemon
// would use, without acquiring or starting a language server. If no project
// marker exists, it returns start unchanged so explicit non-project inspection
// paths keep their current behaviour.
func resolveCLIWorkspace(start string, cfg config.Config) (string, error) {
	if start == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("getwd: %w", err)
		}
		start = cwd
	}
	abs, err := filepath.Abs(start)
	if err != nil {
		return "", fmt.Errorf("resolving workspace path %s: %w", start, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("stat workspace path %s: %w", abs, err)
	}
	if !info.IsDir() {
		abs = filepath.Dir(abs)
	}
	root, _, err := newWorkspacePool(cfg).Detect(abs)
	if err != nil {
		return abs, nil
	}
	return root, nil
}

// acquireLang returns (or starts) the shared workspace state for root.
// Pass "" for language to detect from root markers; otherwise the named
// adapter is used directly.
func (p *workspacePool) acquireLang(ctx context.Context, root, language string) (*poolEntry, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if e, ok := p.entries[root]; ok {
		slog.Info("pool: reusing LS", "root", root, "language", e.language)
		return e, nil
	}

	if language == "" {
		language = p.detectLanguageAt(root)
		if language == "" {
			return nil, fmt.Errorf("no enabled language matches %s", root)
		}
	}

	lspCfg, ok := p.cfgFor(language)
	if !ok {
		return nil, fmt.Errorf("language %q not configured or not enabled", language)
	}

	rootURI := protocol.FileURI(root)
	c := cache.New(p.cacheTTL)
	inv := cache.NewInvalidator(c)
	proxy := &clientProxy{}

	e := &poolEntry{root: root, language: language, proxy: proxy, inv: inv, cache: c}

	sup := lsp.NewSupervisor(lspCfg.Command, argsFor(language, root, lspCfg), envFor(lspCfg), lsp.SupervisorOptions{
		OnStart: func(startCtx context.Context, conn *jsonrpc.Conn) error {
			ad, err := newAdapter(language, conn)
			if err != nil {
				return err
			}
			// Subscribe BEFORE initialized so the first publishDiagnostics
			// burst (sent within ms of initialized) is not lost.
			ad.Subscribe(inv.Handle)
			if _, err := ad.Initialize(startCtx, initParamsFor(language, rootURI)); err != nil {
				return fmt.Errorf("initialize: %w", err)
			}
			if err := ad.Initialized(startCtx); err != nil {
				return fmt.Errorf("initialized: %w", err)
			}
			proxy.set(ad)
			slog.Info("pool: LS ready", "root", root, "language", language)
			return nil
		},
	})

	if err := sup.Start(ctx); err != nil {
		c.Close()
		return nil, fmt.Errorf("starting %s for %s: %w", language, root, err)
	}
	e.sup = sup

	p.entries[root] = e
	slog.Info("pool: new workspace", "root", root, "language", language)
	return e, nil
}

func (p *workspacePool) cfgFor(language string) (config.LSPConfig, bool) {
	for _, l := range p.langs {
		if l.name == language {
			return l.cfg, true
		}
	}
	return config.LSPConfig{}, false
}

// newAdapter constructs the right adapter for a language.
func newAdapter(language string, conn *jsonrpc.Conn) (lsp.LSPClient, error) {
	switch language {
	case "go":
		return gopls.New(conn), nil
	case "java":
		return jdtls.New(conn), nil
	case "python":
		return pyright.New(conn), nil
	default:
		return nil, fmt.Errorf("no adapter registered for language %q", language)
	}
}

// initParamsFor builds the Initialize params for a language.
func initParamsFor(language, rootURI string) protocol.InitializeParams {
	switch language {
	case "java":
		return jdtls.DefaultInitParams(rootURI)
	case "python":
		return pyright.DefaultInitParams(rootURI)
	default:
		return gopls.DefaultInitParams(rootURI)
	}
}

// argsFor returns the supervisor args for the given language and workspace root.
// For most languages this is lspCfg.Args verbatim. Java is special: jdtls
// requires a -data <dir> argument pointing to an Eclipse workspace storage
// directory. Using a per-root directory prevents classpath conflicts when
// multiple Java projects are open simultaneously.
func argsFor(language, root string, lspCfg config.LSPConfig) []string {
	if language != "java" {
		return lspCfg.Args
	}
	dataDir := jdtlsDataDir(root)
	_ = os.MkdirAll(dataDir, 0o700)
	out := make([]string, len(lspCfg.Args), len(lspCfg.Args)+2)
	copy(out, lspCfg.Args)
	return append(out, "-data", dataDir)
}

// jdtlsDataDir returns a per-workspace Eclipse workspace data directory for
// jdtls. The directory name is derived from a hash of the workspace root so
// each project gets isolated Eclipse state.
func jdtlsDataDir(root string) string {
	sum := sha256.Sum256([]byte(root))
	return filepath.Join(config.CacheDir(), "jdtls-data", fmt.Sprintf("%x", sum[:8]))
}

// envFor builds the environment slice for the LSP supervisor process.
// When lspCfg.Env is empty, nil is returned so the child process inherits
// the daemon's environment unchanged. When overrides are present, the daemon's
// environment is copied and the per-key overrides are applied on top — the
// child process still sees PATH, HOME, JAVA_HOME, etc., with only the
// configured keys changed. This is important for SDKMAN and other version
// managers that rely on PATH being set correctly in the daemon's environment.
func envFor(lspCfg config.LSPConfig) []string {
	if len(lspCfg.Env) == 0 {
		return nil
	}
	env := os.Environ()
	for k, v := range lspCfg.Env {
		env = setEnvVar(env, k, v)
	}
	return env
}

// setEnvVar replaces the value of key in env if it is already present,
// otherwise appends "key=value". The input slice is modified in place.
func setEnvVar(env []string, key, val string) []string {
	prefix := key + "="
	for i, e := range env {
		if strings.HasPrefix(e, prefix) {
			env[i] = prefix + val
			return env
		}
	}
	return append(env, prefix+val)
}

// lookup returns the entry for root if it has already been acquired, or nil
// if no entry exists. Unlike acquire, lookup never starts a new LS.
func (p *workspacePool) lookup(root string) *poolEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.entries[root]
}

// close shuts down all LS processes. Safe to call from multiple goroutines
// but intended to be called once at daemon shutdown.
func (p *workspacePool) close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	bg := context.Background()
	for _, e := range p.entries {
		if c := e.proxy.get(); c != nil {
			_ = c.Shutdown(bg)
			_ = c.Exit(bg)
		}
		if e.sup != nil {
			e.sup.Stop()
		}
		if e.cache != nil {
			e.cache.Close()
		}
	}
}
