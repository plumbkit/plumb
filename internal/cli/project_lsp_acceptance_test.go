package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
)

// Reproduce the reported shape through the real pool constructor and routing:
// Python at a manifest-less root, a TypeScript child, and stray HTML. Stub
// transports isolate policy and dispatch from installed language-server binaries.
func TestProjectLSPAcceptanceMixedRepository(t *testing.T) {
	root := paths.Canonical(t.TempDir())
	app := filepath.Join(root, "app")
	mustMkdir(t, app)
	writePoolProjectConfig(t, root, "[lsp.python]\nenabled = true\n[lsp.typescript]\nenabled = true\n")
	mustWrite(t, filepath.Join(app, "tsconfig.json"), "{}")
	mustWrite(t, filepath.Join(app, "main.ts"), "export const value = 1")
	for i := range 70 {
		mustWrite(t, filepath.Join(root, fmt.Sprintf("module%d.py", i)), "value = 1")
	}
	mustWrite(t, filepath.Join(root, "report.html"), "<html></html>")
	mustWrite(t, filepath.Join(root, "other.html"), "<html></html>")

	cfg := config.Defaults()
	cfg.LSP = map[string]config.LSPConfig{
		"python":     {Command: os.Args[0], RootMarkers: []string{"pyproject.toml"}},
		"typescript": {Command: os.Args[0], RootMarkers: []string{"tsconfig.json"}},
		"html":       {Command: os.Args[0], Enabled: true},
	}
	pool := newWorkspacePool(t.Context(), cfg)
	if got := pool.extLangAt(root); got != "python" {
		t.Errorf("content sniff = %q, want python despite globally disabled Python", got)
	}
	discovered := pool.discoverChildLanguages(root, 2)
	if !slices.Contains(discovered, discoveredRoot{root: app, language: "typescript"}) {
		t.Errorf("child discovery = %v, want TypeScript at %s", discovered, app)
	}
	python := &stubClient{id: "python"}
	typescript := &stubClient{id: "typescript"}
	html := &stubClient{id: "html"}
	installEntryLang(pool, root, "python", python)
	tsProxy := installEntryLang(pool, app, "typescript", typescript)
	installEntryLang(pool, root, "html", html)
	proxy := newRoutingProxy(pool)
	proxy.setPrimary(app, "typescript", tsProxy)
	proxy.setDiscovered(root, discovered)
	var activated []string
	proxy.setActivateHook(func(language string) { activated = append(activated, language) })

	for _, path := range []string{filepath.Join(root, "module0.py"), filepath.Join(app, "main.ts")} {
		if _, err := proxy.Definition(t.Context(), protocol.DefinitionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: paths.PathToURI(path)},
		}); err != nil {
			t.Errorf("Definition(%s): %v", path, err)
		}
	}
	if len(python.definitions) != 1 || len(typescript.definitions) != 1 || len(html.definitions) != 0 {
		t.Errorf("dispatch counts python=%d typescript=%d html=%d, want 1/1/0",
			len(python.definitions), len(typescript.definitions), len(html.definitions))
	}
	if !slices.Contains(activated, "python") {
		t.Errorf("secondary adapter notifications = %v, want python for the multi-language display", activated)
	}

	sibling := paths.Canonical(t.TempDir())
	writePoolProjectConfig(t, sibling, "")
	mustWrite(t, filepath.Join(sibling, "pyproject.toml"), "")
	if _, language, err := pool.Detect(sibling); err != nil || language != LanguageNone {
		t.Errorf("sibling detection = %q, %v; project enable leaked outside its root", language, err)
	}
}
