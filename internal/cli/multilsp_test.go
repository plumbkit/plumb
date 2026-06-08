package cli

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/golimpio/plumb/internal/cache"
	"github.com/golimpio/plumb/internal/config"
	"github.com/golimpio/plumb/internal/lsp"
	"github.com/golimpio/plumb/internal/lsp/protocol"
)

// newTestPoolMulti builds a pool with the named languages enabled, so per-file
// routing within a single root (e.g. Go + HTML) can be exercised without
// spawning real language servers.
func newTestPoolMulti(langs ...string) *workspacePool {
	cfgs := map[string]config.LSPConfig{
		"go":         {RootMarkers: []string{"go.mod"}, Enabled: true},
		"html":       {RootMarkers: []string{"index.html"}, Enabled: true},
		"typescript": {RootMarkers: []string{"tsconfig.json"}, Enabled: true},
	}
	p := &workspacePool{
		entries: make(map[poolKey]*poolEntry),
		baseCtx: context.Background(),
	}
	for _, l := range langs {
		p.langs = append(p.langs, langConfig{name: l, cfg: cfgs[l]})
	}
	return p
}

// installEntryLang mounts a stub client as the (root, language) entry, as if it
// had been acquired normally, and returns its proxy.
func installEntryLang(pool *workspacePool, root, language string, client lsp.Client) *clientProxy {
	cp := &clientProxy{}
	cp.set(client)
	pool.entries[poolKey{root, language}] = &poolEntry{root: root, language: language, proxy: cp}
	return cp
}

func TestNormaliseLangName(t *testing.T) {
	cases := map[string]string{
		"tsx":        "typescript",
		"jsx":        "typescript",
		"javascript": "typescript",
		"typescript": "typescript",
		"go":         "go",
		"html":       "html",
		"python":     "python",
	}
	for in, want := range cases {
		if got := normaliseLangName(in); got != want {
			t.Errorf("normaliseLangName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFileLanguage(t *testing.T) {
	pool := newTestPoolMulti("go", "html")
	cases := []struct{ path, want string }{
		{"/x/main.go", "go"},
		{"/x/index.html", "html"},
		{"/x/page.htm", "html"},
		{"/x/readme.md", ""}, // markdown not enabled
		{"/x/app.tsx", ""},   // typescript not enabled
		{"/x/Makefile", ""},  // no owning language
	}
	for _, c := range cases {
		if got := pool.fileLanguage(c.path); got != c.want {
			t.Errorf("fileLanguage(%q) = %q, want %q", c.path, got, c.want)
		}
	}

	// With typescript enabled, every dialect folds onto the typescript adapter.
	tspool := newTestPoolMulti("go", "typescript")
	for _, p := range []string{"/x/a.ts", "/x/a.tsx", "/x/a.jsx", "/x/a.js"} {
		if got := tspool.fileLanguage(p); got != "typescript" {
			t.Errorf("fileLanguage(%q) = %q, want typescript", p, got)
		}
	}
}

// TestRoutingProxy_RoutesByExtensionWithinRoot is the core multi-LSP guard: a
// single root with Go primary and HTML secondary routes each file to the
// language server that owns its extension.
func TestRoutingProxy_RoutesByExtensionWithinRoot(t *testing.T) {
	root := freshTempDir(t)
	mustWrite(t, filepath.Join(root, "go.mod"), "module x\n")

	pool := newTestPoolMulti("go", "html")
	goClient := &stubClient{id: "go"}
	htmlClient := &stubClient{id: "html"}
	goProxy := installEntryLang(pool, root, "go", goClient)
	installEntryLang(pool, root, "html", htmlClient)

	rp := newRoutingProxy(pool)
	rp.setPrimary(root, "go", goProxy)

	_, _ = rp.Definition(context.Background(), protocol.DefinitionParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: "file://" + filepath.Join(root, "main.go")},
	})
	_, _ = rp.Definition(context.Background(), protocol.DefinitionParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: "file://" + filepath.Join(root, "index.html")},
	})

	if len(goClient.definitions) != 1 {
		t.Errorf("go client: want 1 Definition (the .go file), got %d", len(goClient.definitions))
	}
	if len(htmlClient.definitions) != 1 {
		t.Errorf("html client: want 1 Definition (the .html file), got %d", len(htmlClient.definitions))
	}
}

// TestRoutingProxy_ActivateHookFiresForSecondary verifies the session-display
// hook fires the first time a secondary language server serves a request, and
// never for the primary language.
func TestRoutingProxy_ActivateHookFiresForSecondary(t *testing.T) {
	root := freshTempDir(t)
	mustWrite(t, filepath.Join(root, "go.mod"), "module x\n")

	pool := newTestPoolMulti("go", "html")
	goProxy := installEntryLang(pool, root, "go", &stubClient{id: "go"})
	installEntryLang(pool, root, "html", &stubClient{id: "html"})

	rp := newRoutingProxy(pool)
	rp.setPrimary(root, "go", goProxy)

	var activated []string
	rp.setActivateHook(func(lang string) { activated = append(activated, lang) })

	// A .go request must NOT fire the hook (it is the primary language).
	_, _ = rp.Definition(context.Background(), protocol.DefinitionParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: "file://" + filepath.Join(root, "main.go")},
	})
	// A .html request fires it once.
	_, _ = rp.Definition(context.Background(), protocol.DefinitionParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: "file://" + filepath.Join(root, "index.html")},
	})

	if len(activated) != 1 || activated[0] != "html" {
		t.Fatalf("activate hook: got %v, want [html]", activated)
	}
}

// TestRoutingProxy_ActivateHookFiresForSubRootSecondary is the regression test
// for the missed-adapter bug: a secondary language whose own root marker lives
// in a subdirectory (e.g. site/index.html for HTML) makes Detect carve out a
// sub-root, so the activated root differs from the session's primary root. The
// hook must still fire because the file is inside the pinned workspace.
func TestRoutingProxy_ActivateHookFiresForSubRootSecondary(t *testing.T) {
	root := freshTempDir(t)
	mustWrite(t, filepath.Join(root, "go.mod"), "module x\n")
	site := filepath.Join(root, "site")
	mustMkdir(t, site)
	mustWrite(t, filepath.Join(site, "index.html"), "<html></html>\n")

	pool := newTestPoolMulti("go", "html")
	goProxy := installEntryLang(pool, root, "go", &stubClient{id: "go"})
	// HTML resolves to the sub-root (site/ has the index.html marker).
	installEntryLang(pool, site, "html", &stubClient{id: "html"})

	rp := newRoutingProxy(pool)
	rp.setPrimary(root, "go", goProxy)
	var activated []string
	rp.setActivateHook(func(lang string) { activated = append(activated, lang) })

	_, _ = rp.Definition(context.Background(), protocol.DefinitionParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: "file://" + filepath.Join(site, "index.html")},
	})

	if len(activated) != 1 || activated[0] != "html" {
		t.Fatalf("activate hook for a sub-root secondary: got %v, want [html]", activated)
	}
}

// TestRoutingInvProxy_MergesAcrossLanguages verifies AllDiagnostics aggregates
// every language server under the primary root (Go + HTML), not just the
// primary, while still filtering out-of-root URIs.
func TestRoutingInvProxy_MergesAcrossLanguages(t *testing.T) {
	root := freshTempDir(t)
	mustWrite(t, filepath.Join(root, "go.mod"), "module x\n")

	pool := newTestPoolMulti("go", "html")
	goCache := cache.New(time.Hour)
	defer goCache.Close()
	goInv := cache.NewInvalidator(goCache)
	htmlCache := cache.New(time.Hour)
	defer htmlCache.Close()
	htmlInv := cache.NewInvalidator(htmlCache)
	pool.entries[poolKey{root, "go"}] = &poolEntry{root: root, language: "go", inv: goInv}
	pool.entries[poolKey{root, "html"}] = &poolEntry{root: root, language: "html", inv: htmlInv}

	goURI := "file://" + filepath.Join(root, "main.go")
	htmlURI := "file://" + filepath.Join(root, "index.html")
	outOfRoot := "file:///some/dependency/x.go"
	pushDiag(t, goInv, goURI, []protocol.Diagnostic{{Severity: protocol.SevError, Message: "go err"}})
	pushDiag(t, htmlInv, htmlURI, []protocol.Diagnostic{{Severity: protocol.SevError, Message: "html err"}})
	pushDiag(t, htmlInv, outOfRoot, []protocol.Diagnostic{{Severity: protocol.SevError, Message: "leak"}})

	ri := newRoutingInvProxy(pool)
	ri.setPrimary(root, "go", goInv)

	all := ri.AllDiagnostics()
	if _, ok := all[goURI]; !ok {
		t.Errorf("AllDiagnostics missing Go diagnostics")
	}
	if _, ok := all[htmlURI]; !ok {
		t.Errorf("AllDiagnostics missing HTML diagnostics from the secondary server")
	}
	if _, ok := all[outOfRoot]; ok {
		t.Errorf("AllDiagnostics leaked out-of-root URI")
	}
	if len(all) != 2 {
		t.Errorf("AllDiagnostics returned %d URIs; want 2", len(all))
	}
}

// TestRoutingInvProxy_MergesSubRootSecondary covers the workspace-wide
// diagnostics aggregate reaching into a secondary's sub-root: an HTML server
// rooted at site/ (its index.html marker) must still fold into the Go root's
// AllDiagnostics, since site/ is inside the pinned workspace.
func TestRoutingInvProxy_MergesSubRootSecondary(t *testing.T) {
	root := freshTempDir(t)
	mustWrite(t, filepath.Join(root, "go.mod"), "module x\n")
	site := filepath.Join(root, "site")
	mustMkdir(t, site)

	pool := newTestPoolMulti("go", "html")
	goCache := cache.New(time.Hour)
	defer goCache.Close()
	goInv := cache.NewInvalidator(goCache)
	htmlCache := cache.New(time.Hour)
	defer htmlCache.Close()
	htmlInv := cache.NewInvalidator(htmlCache)
	pool.entries[poolKey{root, "go"}] = &poolEntry{root: root, language: "go", inv: goInv}
	// HTML server carved out at the sub-root site/.
	pool.entries[poolKey{site, "html"}] = &poolEntry{root: site, language: "html", inv: htmlInv}

	goURI := "file://" + filepath.Join(root, "main.go")
	htmlURI := "file://" + filepath.Join(site, "index.html")
	pushDiag(t, goInv, goURI, []protocol.Diagnostic{{Severity: protocol.SevError, Message: "go err"}})
	pushDiag(t, htmlInv, htmlURI, []protocol.Diagnostic{{Severity: protocol.SevError, Message: "html err"}})

	ri := newRoutingInvProxy(pool)
	ri.setPrimary(root, "go", goInv)

	all := ri.AllDiagnostics()
	if _, ok := all[goURI]; !ok {
		t.Errorf("AllDiagnostics missing Go diagnostics")
	}
	if _, ok := all[htmlURI]; !ok {
		t.Errorf("AllDiagnostics missing sub-root HTML diagnostics (site/)")
	}
	if len(all) != 2 {
		t.Errorf("AllDiagnostics returned %d URIs; want 2", len(all))
	}
}
