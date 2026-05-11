package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/golimpio/plumb/internal/cache"
	"github.com/golimpio/plumb/internal/config"
	"github.com/golimpio/plumb/internal/lsp"
	"github.com/golimpio/plumb/internal/lsp/adapters/gopls"
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
// markers; acquire() starts the right adapter for that language.
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

// Detect walks up from start looking for the first configured language's
// root markers. A `.plumb/` directory takes priority — its containing
// directory wins regardless of which language marker is present there or
// above.
func (p *workspacePool) Detect(start string) (root, language string, err error) {
	d := start
	for {
		// Highest priority: explicit .plumb marker.
		if _, err := os.Stat(filepath.Join(d, ".plumb")); err == nil {
			if lang := p.detectLanguageAt(d); lang != "" {
				return d, lang, nil
			}
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

// acquire returns (or starts) the shared workspace state for root. The
// language argument selects the adapter; if it doesn't match a configured
// language, an error is returned.
func (p *workspacePool) acquire(ctx context.Context, root string) (*poolEntry, error) {
	return p.acquireLang(ctx, root, "")
}

// acquireLang is like acquire but lets the caller specify language directly,
// skipping detection. Pass "" to detect from root markers.
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

	rootURI := "file://" + root
	c := cache.New(p.cacheTTL)
	inv := cache.NewInvalidator(c)
	proxy := &clientProxy{}

	e := &poolEntry{root: root, language: language, proxy: proxy, inv: inv, cache: c}

	sup := lsp.NewSupervisor(lspCfg.Command, lspCfg.Args, nil, lsp.SupervisorOptions{
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
	case "python":
		return pyright.New(conn), nil
	default:
		return nil, fmt.Errorf("no adapter registered for language %q", language)
	}
}

// initParamsFor builds the Initialize params for a language.
func initParamsFor(language, rootURI string) protocol.InitializeParams {
	switch language {
	case "python":
		return pyright.DefaultInitParams(rootURI)
	default:
		return gopls.DefaultInitParams(rootURI)
	}
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
