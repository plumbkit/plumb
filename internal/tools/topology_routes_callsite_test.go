package tools_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/tools"
	"github.com/plumbkit/plumb/internal/topology"
	"github.com/plumbkit/plumb/internal/topology/extractors/golang"
	"github.com/plumbkit/plumb/internal/topology/extractors/treesitter"
)

// routesFixture indexes files through the production Go and Python extractors
// and returns the topology_routes tool over that index.
func routesFixture(t *testing.T, files map[string]string) *tools.TopologyRoutes {
	t.Helper()
	ws := t.TempDir()
	for rel, src := range files {
		p := filepath.Join(ws, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	store, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
		[]topology.Extractor{golang.New(), treesitter.NewPython()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	deadline := time.Now().Add(30 * time.Second)
	for rel := range files {
		if !strings.HasSuffix(rel, ".go") && !strings.HasSuffix(rel, ".py") {
			continue
		}
		for {
			if nodes, _ := store.SymbolsInFile(context.Background(), filepath.Join(ws, rel)); len(nodes) > 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s to be indexed", rel)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	return tools.NewTopologyRoutes(func() *topology.Store { return store })
}

func runRoutes(t *testing.T, tool *tools.TopologyRoutes, args map[string]any) string {
	t.Helper()
	raw, _ := json.Marshal(args)
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("topology_routes: %v", err)
	}
	return out
}

func mustContain(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
}

func mustNotContain(t *testing.T, out string, nots ...string) {
	t.Helper()
	for _, n := range nots {
		if strings.Contains(out, n) {
			t.Errorf("output must not contain %q:\n%s", n, out)
		}
	}
}

var goHTTPFixture = map[string]string{
	"go.mod": "module example.com/app\n\ngo 1.22\n",
	"web/server.go": `package web

import (
	"net/http"

	"example.com/app/api"
)

type Server struct{}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {}

func handleHealth(w http.ResponseWriter, r *http.Request) {}

func Routes(mux *http.ServeMux, s *Server) {
	mux.HandleFunc("/healthz", handleHealth)
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("/api/users", api.ListUsers)
	mux.HandleFunc("/inline", func(w http.ResponseWriter, r *http.Request) {})
	http.HandleFunc("/missing", http.NotFound)
}
`,
	"api/users.go": `package api

import "net/http"

func ListUsers(w http.ResponseWriter, r *http.Request) {}
`,
	// Registration-shaped calls in a file that imports no HTTP framework.
	// A registration in a test is a fixture, not an entry point.
	"web/server_test.go": `package web

import "net/http"

func fixtureHandler(w http.ResponseWriter, r *http.Request) {}

func setup(mux *http.ServeMux) { mux.HandleFunc("/test-only", fixtureHandler) }
`,
	"cache/cache.go": `package cache

type C struct{}

func (c C) Get(k string) string        { return k }
func (c C) HandleFunc(p string, f func()) {}

func use(c C, h func()) {
	c.Get("/not-a-route")
	c.HandleFunc("/fake", h)
}
`,
}

// TestRoutesCallSite_GoNetHTTP: route string -> handler for each handler
// shape, with the confidence each one has earned, and no claim for a
// registration-shaped call in a file that does not import net/http.
func TestRoutesCallSite_GoNetHTTP(t *testing.T) {
	tool := routesFixture(t, goHTTPFixture)
	out := runRoutes(t, tool, map[string]any{"framework": "net/http"})
	mustContain(t, out,
		"* /healthz [net/http] -> handleHealth (same-package, web/server.go:",
		"GET /status [net/http] -> s.handleStatus (name-match, web/server.go:",
		"* /api/users [net/http] -> api.ListUsers (resolved, api/users.go:5)",
		"* /inline [net/http] -> <inline or expression> (unresolved)",
		"* /missing [net/http] -> http.NotFound (external)",
		"5 HTTP route(s)",
	)
	mustNotContain(t, out, "/fake", "/not-a-route", "/test-only", "name-match candidates")
}

// TestRoutesCallSite_PathPrefixFiltersRouteStrings: path_prefix is now a real
// route filter, not a symbol-name substring.
func TestRoutesCallSite_PathPrefixFiltersRouteStrings(t *testing.T) {
	tool := routesFixture(t, goHTTPFixture)
	out := runRoutes(t, tool, map[string]any{"framework": "net/http", "path_prefix": "/api"})
	mustContain(t, out, "/api/users", "1 HTTP route(s)")
	mustNotContain(t, out, "/healthz", "/status")
}

func cobraFixture() map[string]string {
	vars, args := make([]string, 0, 10), make([]string, 0, 10)
	// Ten leaf commands registered in ONE AddCommand call: past the old
	// argument cap of 8, so the tail is linked only if the cap was raised.
	for i := range 10 {
		vars = append(vars, fmt.Sprintf("var leaf%dCmd = &cobra.Command{Use: \"leaf%d\", RunE: runLeaf}", i, i))
		args = append(args, fmt.Sprintf("leaf%dCmd", i))
	}
	return map[string]string{
		"go.mod": "module example.com/app\n\ngo 1.22\n",
		"cli/root.go": `package cli

import "github.com/spf13/cobra"

var rootCmd = &cobra.Command{Use: "app"}

var serveCmd = &cobra.Command{
	Use:  "serve [addr]",
	RunE: runServe,
}

var configCmd = &cobra.Command{Use: "config"}

var configShowCmd = &cobra.Command{Use: "show", Run: showConfig}

` + strings.Join(vars, "\n") + `

func runServe(cmd *cobra.Command, args []string) error { return nil }
func showConfig(cmd *cobra.Command, args []string)     {}
func runLeaf(cmd *cobra.Command, args []string) error  { return nil }

func versionCmd() *cobra.Command {
	return &cobra.Command{Use: "version", Run: func(*cobra.Command, []string) {}}
}

func pairCmds() []*cobra.Command {
	return []*cobra.Command{
		&cobra.Command{Use: "pair-a", RunE: runLeaf},
		&cobra.Command{Use: "pair-b", RunE: runServe},
	}
}

func init() {
	rootCmd.AddCommand(serveCmd, configCmd, versionCmd())
	rootCmd.AddCommand(` + strings.Join(args, ", ") + `)
	configCmd.AddCommand(configShowCmd)
}
`,
	}
}

// TestRoutesCallSite_CobraTree: the command tree is recovered from Use fields
// and AddCommand links, with Run/RunE tied to its function; a factory call is
// disclosed as unlinked rather than silently dropped.
func TestRoutesCallSite_CobraTree(t *testing.T) {
	tool := routesFixture(t, cobraFixture())
	out := runRoutes(t, tool, map[string]any{"framework": "cobra"})
	mustContain(t, out,
		"17 Cobra command(s)",
		"\n  pair-a -> runLeaf (same-package",
		"\n  pair-b -> runServe (same-package",
		"\n  app  [rootCmd cli/root.go:",
		"\n    serve -> runServe (same-package, cli/root.go:",
		"\n    config  [configCmd cli/root.go:",
		"\n      show -> showConfig (same-package, cli/root.go:",
		"\n    leaf9 -> runLeaf (same-package",
		"\n  version -> <inline or expression> (unresolved)  [versionCmd",
		"1 AddCommand argument(s) not linked",
		"group commands (no Run) 2",
	)
	mustNotContain(t, out, "name-match candidates")

	prefixed := runRoutes(t, tool, map[string]any{"framework": "cobra", "path_prefix": "app config"})
	mustContain(t, prefixed, "  app config  [configCmd", "  app config show -> showConfig")
	mustNotContain(t, prefixed, "serve")
}

var pythonFixture = map[string]string{
	"app/views.py": `from flask import Flask

app = Flask(__name__)

@app.route("/")
def index():
    return "hi"

@app.post("/login")
def login():
    pass

def create_app():
    @app.get("/nested")
    def nested():
        pass
    return app
`,
	"svc/test_api.py": `from fastapi import APIRouter

router = APIRouter()

@router.get("/pytest-only")
def fixture():
    pass
`,
	"svc/api.py": `from fastapi import APIRouter

router = APIRouter()

@router.get("/items/{item_id}")
async def read_item(item_id: int):
    return {}

@router.websocket("/ws")
async def stream():
    pass
`,
	// A decorator that looks like a route, in a file that imports neither.
	"other/tool.py": `class Router:
    def get(self, p):
        return lambda f: f

router = Router()

@router.get("/nope")
def nope():
    pass
`,
}

// TestRoutesCallSite_PythonDecorators is the scripted-language case: the
// handler is the decorated def itself, attributed only where the file imports
// Flask or FastAPI.
func TestRoutesCallSite_PythonDecorators(t *testing.T) {
	tool := routesFixture(t, pythonFixture)
	out := runRoutes(t, tool, map[string]any{})
	mustContain(t, out,
		"* / [flask] -> index (decorated, app/views.py:6)",
		"POST /login [flask] -> login (decorated, app/views.py:",
		"GET /nested [flask] -> nested (decorated, app/views.py:",
		"GET /items/{item_id} [fastapi] -> read_item (decorated, svc/api.py:",
		"WEBSOCKET /ws [fastapi] -> stream (decorated, svc/api.py:",
		"5 HTTP route(s)",
	)
	mustNotContain(t, out, "/nope", "/pytest-only")
}

// TestRoutesCallSite_FallsBackToNameMatchOnlyWithoutSites: a framework with no
// recovered registration falls back to the old candidates, labelled as such.
func TestRoutesCallSite_FallsBackToNameMatchOnlyWithoutSites(t *testing.T) {
	tool := routesFixture(t, map[string]string{
		"go.mod": "module example.com/app\n\ngo 1.22\n",
		"h/h.go": `package h

func HandleFuncWrapper() {}
`,
	})
	out := runRoutes(t, tool, map[string]any{"framework": "net/http"})
	mustContain(t, out, "name-match candidates: 1", "HandleFuncWrapper", "(name-match)")
	mustNotContain(t, out, "recovered from registration sites")
}

// TestRoutesCallSite_GoRouterFrameworks covers gin, echo and chi: each one's
// handler position, and the attribution guards — a client call through another
// package's import, and gin's three-argument Handle, are not routes.
func TestRoutesCallSite_GoRouterFrameworks(t *testing.T) {
	tool := routesFixture(t, map[string]string{
		"go.mod": "module example.com/app\n\ngo 1.22\n",
		"g/gin.go": `package g

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func auth(c *gin.Context)      {}
func listItems(c *gin.Context) {}

func Register(r *gin.Engine) {
	r.GET("/items", auth, listItems)
	r.Handle("GET", "/raw", listItems)
	_, _ = http.Get("/client-call")
}
`,
		"e/echo.go": `package e

import "github.com/labstack/echo/v4"

func show(c echo.Context) error { return nil }
func mw(next echo.HandlerFunc) echo.HandlerFunc { return next }

func makeHandler() echo.HandlerFunc { return show }

func Register(e *echo.Echo) {
	e.POST("/things", show, mw)
	// An expression before a name: positions are unknowable, so mw must not be
	// read as the handler.
	e.PUT("/made", makeHandler(), mw)
}
`,
		"c/chi.go": `package c

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

func getUser(w http.ResponseWriter, r *http.Request) {}

func Register(r chi.Router) {
	r.Get("/users/{id}", getUser)
	_, _ = http.Get("/also-a-client-call")
}
`,
	})
	out := runRoutes(t, tool, map[string]any{})
	mustContain(t, out,
		"GET /items [gin] -> listItems (same-package, g/gin.go:",
		"POST /things [echo] -> show (same-package, e/echo.go:",
		"GET /users/{id} [chi] -> getUser (same-package, c/chi.go:",
		"PUT /made [echo] -> <inline or expression> (unresolved)",
		"4 HTTP route(s)",
	)
	mustNotContain(t, out, "/client-call", "/also-a-client-call", "/raw", "-> auth", "-> mw")
}
