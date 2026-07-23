package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// enableTestPool builds a pool whose baseConfig knows Go and HTML but whose
// effective set (langs) starts with only Go active — HTML is configured but
// disabled, the exact precondition `enable-lsp html` must flip. HTML's command
// is "go" so lspInstalled() passes deterministically (go is guaranteed on PATH
// in the test suite) without depending on a real HTML server. baseConfig.LogLevel
// is left "" so cfgForWorkspace takes the narrow-pool path (no LoadProject),
// keeping the routing tests hermetic.
func enableTestPool() *workspacePool {
	goCfg := config.LSPConfig{Command: "go", RootMarkers: []string{"go.mod"}, Enabled: true}
	htmlCfg := config.LSPConfig{Command: "go", RootMarkers: []string{"index.html"}, Enabled: false}
	return &workspacePool{
		entries:  make(map[poolKey]*poolEntry),
		baseCtx:  context.Background(),
		cacheTTL: time.Minute,
		langs:    []langConfig{{name: "go", cfg: goCfg}},
		baseConfig: config.Config{
			LSP: map[string]config.LSPConfig{"go": goCfg, "html": htmlCfg},
		},
	}
}

// TestEnableLanguage_AttachesAndServes is hard requirement (1): after
// enable-lsp, the language is in the effective set and a file of that language
// routes to its server. It proves BOTH the language-set update AND the live
// routing end to end — an index.html request goes to the primary (Go) before
// the enable and to the HTML server after, with no daemon restart and no eager
// spawn (the HTML entry is reused, not started).
func TestEnableLanguage_AttachesAndServes(t *testing.T) {
	root := freshTempDir(t)
	mustWrite(t, filepath.Join(root, "go.mod"), "module x\n")

	pool := enableTestPool()
	goClient := &stubClient{id: "go"}
	htmlClient := &stubClient{id: "html"}
	goProxy := installEntryLang(pool, root, "go", goClient)
	// The HTML server is mounted (as the lazy secondary attach would leave it),
	// but fileLanguage gates whether routing ever reaches it.
	installEntryLang(pool, root, "html", htmlClient)

	rp := newRoutingProxy(pool)
	rp.setPrimary(root, "go", goProxy)

	htmlURI := "file://" + filepath.Join(root, "index.html")

	// Precondition: HTML is not active, so an .html request falls back to the
	// primary Go server.
	if pool.hasActiveLanguage("html") {
		t.Fatal("html must not be active before enable")
	}
	if got := pool.fileLanguage(filepath.Join(root, "index.html")); got != "" {
		t.Fatalf("fileLanguage(index.html) = %q before enable, want \"\"", got)
	}
	_, _ = rp.Definition(context.Background(), protocol.DefinitionParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: htmlURI},
	})
	if len(htmlClient.definitions) != 0 {
		t.Fatalf("html served %d requests before enable; want 0 (should route to primary)", len(htmlClient.definitions))
	}

	// Enable HTML live.
	already, err := pool.enableLanguage("html")
	if err != nil {
		t.Fatalf("enableLanguage(html): %v", err)
	}
	if already {
		t.Fatal("enableLanguage(html) reported already-enabled; html was disabled")
	}

	// Postcondition: the language set recognises HTML and routes .html to it.
	if !pool.hasActiveLanguage("html") {
		t.Error("html should be active after enable")
	}
	if got := pool.fileLanguage(filepath.Join(root, "index.html")); got != "html" {
		t.Errorf("fileLanguage(index.html) = %q after enable, want html", got)
	}
	if !pool.baseConfig.LSP["html"].Enabled {
		t.Error("enable must flip baseConfig.LSP[html].Enabled so cfgForWorkspace starts the server")
	}
	_, _ = rp.Definition(context.Background(), protocol.DefinitionParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: htmlURI},
	})
	if len(htmlClient.definitions) != 1 {
		t.Errorf("html served %d requests after enable; want 1 (the .html file must route to it)", len(htmlClient.definitions))
	}
}

// TestEnableLanguage_PriorSessionUnaffected is hard requirement (2): enabling a
// new language must not disturb an existing session — its pinned workspace,
// refcount, and pool entry are untouched, and its primary server keeps serving.
// (Read-tracking is per-connection state that enableLanguage never reaches — it
// mutates only langs and baseConfig — so the pool-observable invariants are the
// existing entries and the primary pin.)
func TestEnableLanguage_PriorSessionUnaffected(t *testing.T) {
	root := freshTempDir(t)
	mustWrite(t, filepath.Join(root, "go.mod"), "module x\n")

	pool := enableTestPool()
	goClient := &stubClient{id: "go"}
	goProxy := installEntryLang(pool, root, "go", goClient)

	// Represent the prior session's pin: a live refcount on its primary entry.
	goEntry := pool.entries[poolKey{root, "go"}]
	goEntry.refs = 1

	rp := newRoutingProxy(pool)
	rp.setPrimary(root, "go", goProxy)

	entriesBefore := len(pool.entries)

	already, err := pool.enableLanguage("html")
	if err != nil || already {
		t.Fatalf("enableLanguage(html) = (already=%v, err=%v), want (false, nil)", already, err)
	}

	// The prior session's pool entry is the SAME object, with its pin intact.
	if pool.entries[poolKey{root, "go"}] != goEntry {
		t.Error("the prior session's go pool entry was replaced by enable-lsp")
	}
	if goEntry.refs != 1 {
		t.Errorf("prior session's pin refcount = %d after enable, want 1 (untouched)", goEntry.refs)
	}
	if len(pool.entries) != entriesBefore {
		t.Errorf("pool entry count changed from %d to %d; enable must not add or drop entries", entriesBefore, len(pool.entries))
	}

	// The prior session's primary server keeps serving its own language.
	_, _ = rp.Definition(context.Background(), protocol.DefinitionParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: "file://" + filepath.Join(root, "main.go")},
	})
	if len(goClient.definitions) != 1 {
		t.Errorf("prior session's go server served %d requests after enable; want 1", len(goClient.definitions))
	}

	// The primary pin is unchanged.
	rp.mu.RLock()
	pr, pl := rp.primaryRoot, rp.primaryLang
	rp.mu.RUnlock()
	if pr != root || pl != "go" {
		t.Errorf("primary pin moved to (%q, %q); want (%q, go)", pr, pl, root)
	}
}

// TestEnableLanguage_ConcurrentReadsRace exercises the copy-on-write contract:
// hot-path readers (fileLanguage, hasActiveLanguage, and the full Detect walk)
// run concurrently with a live enable. Under `go test -race` this fails if the
// langs swap ever tears a reader's slice — the daemon-lifecycle regression this
// design guards against. It asserts nothing beyond not racing/panicking.
func TestEnableLanguage_ConcurrentReadsRace(t *testing.T) {
	root := freshTempDir(t)
	mustWrite(t, filepath.Join(root, "go.mod"), "module x\n")

	pool := enableTestPool()

	const readers = 8
	stop := make(chan struct{})
	done := make(chan struct{})
	for i := 0; i < readers; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for {
				select {
				case <-stop:
					return
				default:
					_ = pool.fileLanguage(filepath.Join(root, "index.html"))
					_ = pool.hasActiveLanguage("html")
					_, _, _ = pool.Detect(root)
				}
			}
		}()
	}

	// Enable while the readers hammer the language set.
	if _, err := pool.enableLanguage("html"); err != nil {
		t.Fatalf("enableLanguage(html): %v", err)
	}

	close(stop)
	for i := 0; i < readers; i++ {
		<-done
	}
}

func TestEnableLanguage_AlreadyEnabled(t *testing.T) {
	pool := enableTestPool()
	before := len(pool.langsSnapshot())

	already, err := pool.enableLanguage("go")
	if err != nil {
		t.Fatalf("enableLanguage(go): %v", err)
	}
	if !already {
		t.Error("enableLanguage(go) should report already-enabled (go is active)")
	}
	if got := len(pool.langsSnapshot()); got != before {
		t.Errorf("langs length changed from %d to %d on a no-op enable", before, got)
	}
}

func TestEnableLanguage_UnknownLanguage(t *testing.T) {
	pool := enableTestPool()
	already, err := pool.enableLanguage("cobol")
	if err == nil {
		t.Fatal("enableLanguage(cobol) should fail: no [lsp.cobol] block")
	}
	if already {
		t.Error("an unknown language must not report already-enabled")
	}
	if pool.hasActiveLanguage("cobol") {
		t.Error("an unknown language must not join the effective set")
	}
}

func TestEnableLanguage_ServerNotInstalled(t *testing.T) {
	pool := enableTestPool()
	pool.baseConfig.LSP["ghost"] = config.LSPConfig{
		Command:     "plumb-no-such-binary-xyz",
		RootMarkers: []string{"ghost.toml"},
		Enabled:     false,
	}
	already, err := pool.enableLanguage("ghost")
	if err == nil {
		t.Fatal("enableLanguage(ghost) should fail: server binary not installed")
	}
	if already {
		t.Error("a not-installed language must not report already-enabled")
	}
	// The error must name the binary so the user knows what to install.
	if want := "plumb-no-such-binary-xyz"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q should name the missing binary %q", err.Error(), want)
	}
	if pool.hasActiveLanguage("ghost") {
		t.Error("a not-installed language must not join the effective set")
	}
}

func TestEnableLanguage_NoCommandConfigured(t *testing.T) {
	pool := enableTestPool()
	pool.baseConfig.LSP["nocmd"] = config.LSPConfig{Command: "", Enabled: false}
	_, err := pool.enableLanguage("nocmd")
	if err == nil {
		t.Fatal("enableLanguage(nocmd) should fail: no command configured")
	}
	if pool.hasActiveLanguage("nocmd") {
		t.Error("a command-less language must not join the effective set")
	}
}
