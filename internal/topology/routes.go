package topology

import (
	"context"
	"database/sql"
	"fmt"
	"path"
	"sort"
	"strings"
)

// Route recovery reads REGISTRATION sites — the call, composite-literal field or
// decorator that binds a route string to a handler — out of topology_call_sites,
// and ties each handler argument to a declaration where the index can.
//
// A site is attributed to a framework only when the registering file imports
// that framework's package (the plumb#5 lesson: qualify by import, never by a
// bare type or method name). `cache.Get(ctx, key)` in a file that does not import
// chi is not a chi route, however much it looks like one.
//
// Coverage is what the extractors record: Go (net/http, gorilla/mux, chi, gin,
// echo, Cobra) and Python decorators (Flask, FastAPI). Router groups and mounts
// (`r.Route("/api", ...)`, `r.Group("/v1")`) are not composed: a route registered
// inside one is reported with the string written at its own site.

// RouteFramework names a framework family whose registrations are recognised.
type RouteFramework string

const (
	FrameworkNetHTTP RouteFramework = "net/http"
	FrameworkGorilla RouteFramework = "gorilla/mux"
	FrameworkChi     RouteFramework = "chi"
	FrameworkGin     RouteFramework = "gin"
	FrameworkEcho    RouteFramework = "echo"
	FrameworkCobra   RouteFramework = "cobra"
	FrameworkFlask   RouteFramework = "flask"
	FrameworkFastAPI RouteFramework = "fastapi"
)

// AllRouteFrameworks lists every framework route recovery recognises, in display order.
var AllRouteFrameworks = []RouteFramework{
	FrameworkNetHTTP, FrameworkGorilla, FrameworkChi, FrameworkGin, FrameworkEcho,
	FrameworkCobra, FrameworkFlask, FrameworkFastAPI,
}

// HandlerConfidence labels how a registration's handler was tied to a declaration.
// None of them is a probability, and none claims type resolution.
type HandlerConfidence string

const (
	// HandlerResolved: a package-qualified name (`api.Handle`) resolved through
	// the registering file's own import to that package's top-level function.
	HandlerResolved HandlerConfidence = "resolved"
	// HandlerSamePackage: a bare name resolved to the top-level function of that
	// name in the registering file's package. A local variable of the same name
	// would shadow it; that is the residual risk.
	HandlerSamePackage HandlerConfidence = "same-package"
	// HandlerDecorated: the handler IS the decorated declaration (Python).
	HandlerDecorated HandlerConfidence = "decorated"
	// HandlerNameMatch: a method value (`s.handleX`) matched to the package's only
	// method of that name. The receiver's type is not checked.
	HandlerNameMatch HandlerConfidence = "name-match"
	// HandlerAmbiguous: a method value whose name more than one method in the
	// package carries. Candidates says how many; none is chosen.
	HandlerAmbiguous HandlerConfidence = "ambiguous"
	// HandlerExternal: a package-qualified handler whose package is outside the
	// indexed tree (`http.NotFound`).
	HandlerExternal HandlerConfidence = "external"
	// HandlerUnresolved: an inline function, an expression, or a name no indexed
	// declaration carries (a local variable, a closure).
	HandlerUnresolved HandlerConfidence = "unresolved"
)

// RouteHandler is a registration's handler: the argument as written, and the
// declaration it was tied to when one was.
type RouteHandler struct {
	// Text is the handler argument as written; "" when it is not a name (an
	// inline function literal or another expression).
	Text       string
	Node       *Node
	Candidates int // set for HandlerAmbiguous
	Confidence HandlerConfidence
}

// RouteBinding is one recovered route registration.
type RouteBinding struct {
	Framework RouteFramework
	// Method is the HTTP method when the registration names one ("" otherwise).
	Method string
	// Route is the route string as written. When Dynamic, it is the route
	// argument's expression text if that is a name, else "".
	Route   string
	Dynamic bool
	Handler RouteHandler
	Path    string // registering file
	Line    int
}

// Command is one recovered Cobra command and the subcommands registered on it.
type Command struct {
	// Use is the command's Use string; empty and Dynamic when it is not a literal.
	Use     string
	Dynamic bool
	// Decl is the declaration the command literal sits in: the package-level
	// variable it initialises, or the factory function that returns it.
	Decl     string
	DeclKind NodeKind
	// Handler is the Run/RunE function; its Confidence is "" for a group
	// command that sets neither.
	Handler  RouteHandler
	Path     string
	Line     int
	Children []*Command
	parented bool
}

// Name is the command's invocation word: the first field of Use.
func (c *Command) Name() string {
	if f := strings.Fields(c.Use); len(f) > 0 {
		return f[0]
	}
	return ""
}

// RouteReport is the result of one recovery pass.
type RouteReport struct {
	Bindings []RouteBinding
	// Commands are the Cobra commands no recovered AddCommand registers: the
	// root(s) of each recovered tree, plus any command whose registration could
	// not be linked (see UnlinkedChildArgs).
	Commands     []*Command
	CommandCount int
	// UnlinkedChildArgs counts AddCommand arguments that produced no tree edge:
	// a factory call or other expression rather than a name, a name no recovered
	// command is declared under, an argument past the recorded cap, or a parent
	// that is not itself a recovered command.
	UnlinkedChildArgs int
}

// RouteOpts narrows a recovery pass.
type RouteOpts struct {
	// Frameworks restricts recovery to these families; empty means all.
	Frameworks []RouteFramework
}

// Routes recovers route registrations and Cobra command trees from the index.
func (s *Store) Routes(ctx context.Context, opts RouteOpts) (*RouteReport, error) {
	return RecoverRoutes(ctx, s.db, opts)
}

// RecoverRoutes is Routes over an open index database. It reads inside one
// transaction so sites, imports and declarations come from the same snapshot.
func RecoverRoutes(ctx context.Context, db *sql.DB, opts RouteOpts) (*RouteReport, error) {
	want := map[RouteFramework]bool{}
	for _, f := range opts.Frameworks {
		want[f] = true
	}
	wants := func(fs ...RouteFramework) bool {
		if len(want) == 0 {
			return true
		}
		for _, f := range fs {
			if want[f] {
				return true
			}
		}
		return false
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("topology: routes: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rep := &RouteReport{}
	if wants(FrameworkNetHTTP, FrameworkGorilla, FrameworkChi, FrameworkGin, FrameworkEcho, FrameworkCobra) {
		if err := recoverGoRoutes(ctx, tx, wants, rep); err != nil {
			return nil, err
		}
	}
	if wants(FrameworkFlask, FrameworkFastAPI) {
		if err := recoverPythonRoutes(ctx, tx, wants, rep); err != nil {
			return nil, err
		}
	}
	sort.SliceStable(rep.Bindings, func(i, j int) bool {
		a, b := rep.Bindings[i], rep.Bindings[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Line < b.Line
	})
	return rep, nil
}

// --- shared -----------------------------------------------------------------

func nodeByID(ctx context.Context, tx *sql.Tx, id int64) (*Node, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+nodeColumns+`
		  FROM topology_nodes n JOIN topology_files f ON f.id = n.file_id
		 WHERE n.id = ?`, id)
	if err != nil {
		return nil, fmt.Errorf("topology: routes: node: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	n, err := scanNode(rows)
	if err != nil {
		return nil, fmt.Errorf("topology: routes: node scan: %w", err)
	}
	return &n, nil
}

// goMethodsNamed returns the Go methods called name declared in dir.
func goMethodsNamed(ctx context.Context, tx *sql.Tx, dir, name string) ([]Node, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+nodeColumns+`
		  FROM topology_nodes n JOIN topology_files f ON f.id = n.file_id
		 WHERE n.kind = ? AND n.language = 'go' AND n.name = ?
		 ORDER BY n.id`, string(KindMethod), name)
	if err != nil {
		return nil, fmt.Errorf("topology: routes: methods: %w", err)
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, fmt.Errorf("topology: routes: method scan: %w", err)
		}
		if path.Dir(n.Path) == dir {
			out = append(out, n)
		}
	}
	return out, rows.Err()
}
