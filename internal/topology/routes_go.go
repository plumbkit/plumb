package topology

import (
	"context"
	"database/sql"
	"fmt"
	"path"
	"strings"
)

const (
	importNetHTTP = "net/http"
	importGorilla = "github.com/gorilla/mux"
	importChi     = "github.com/go-chi/chi"
	importGin     = "github.com/gin-gonic/gin"
	importEcho    = "github.com/labstack/echo"
	importCobra   = "github.com/spf13/cobra"
)

var (
	// upperVerbs are gin's and echo's registration methods.
	upperVerbs = map[string]bool{
		"GET": true, "POST": true, "PUT": true, "DELETE": true,
		"PATCH": true, "HEAD": true, "OPTIONS": true, "Any": true,
	}
	// chiVerbs are chi's.
	chiVerbs = map[string]bool{
		"Get": true, "Post": true, "Put": true, "Delete": true,
		"Patch": true, "Head": true, "Options": true, "Connect": true, "Trace": true,
	}
)

// goRouteSitesQuery prefilters Go sites to the shapes a registration can take.
// The callee list is literal SQL, not built from upperVerbs/chiVerbs, so the
// query is a constant; TestGoRouteSitesQueryCoversEveryVerb keeps the two in
// step. Framework attribution happens per site afterwards.
const goRouteSitesQuery = `
	SELECT cs.file_id, f.path, cs.site_kind, cs.callee, IFNULL(cs.qualifier, ''), cs.start_line,
	       cs.first_string_arg IS NOT NULL, IFNULL(cs.first_string_arg, ''), cs.arg_idents,
	       cs.arg_count, cs.arg_spread, IFNULL(cs.enclosing_id, 0), IFNULL(n.name, ''), IFNULL(n.kind, '')
	  FROM topology_call_sites cs
	  JOIN topology_files f ON f.id = cs.file_id
	  LEFT JOIN topology_nodes n ON n.id = cs.enclosing_id
	 WHERE cs.language = 'go'
	   AND ((cs.site_kind = 'call' AND cs.callee IN (
	            'Handle', 'HandleFunc', 'AddCommand',
	            'GET', 'POST', 'PUT', 'DELETE', 'PATCH', 'HEAD', 'OPTIONS', 'Any',
	            'Get', 'Post', 'Put', 'Delete', 'Patch', 'Head', 'Options', 'Connect', 'Trace'))
	     OR (cs.site_kind = 'field' AND cs.callee IN ('Use', 'Run', 'RunE') AND cs.qualifier IS NOT NULL))
	 ORDER BY f.path, cs.start_byte, cs.id`

// goSite is one candidate registration row.
type goSite struct {
	fileID        int64
	path          string
	kind          CallSiteKind
	callee        string
	qualifier     string
	line          int
	hasString     bool
	str           string
	idents        []string
	argCount      int
	spread        bool
	enclosingID   int64
	enclosingName string
	enclosingKind NodeKind
}

func goRouteSites(ctx context.Context, tx *sql.Tx) ([]goSite, error) {
	rows, err := tx.QueryContext(ctx, goRouteSitesQuery)
	if err != nil {
		return nil, fmt.Errorf("topology: routes: go sites: %w", err)
	}
	defer rows.Close()
	var out []goSite
	for rows.Next() {
		var s goSite
		var kind, idents, encKind string
		var spread int
		if err := rows.Scan(&s.fileID, &s.path, &kind, &s.callee, &s.qualifier, &s.line,
			&s.hasString, &s.str, &idents, &s.argCount, &spread, &s.enclosingID, &s.enclosingName, &encKind); err != nil {
			return nil, fmt.Errorf("topology: routes: go site scan: %w", err)
		}
		s.kind, s.enclosingKind, s.spread = CallSiteKind(kind), NodeKind(encKind), spread != 0
		if isGoTestPath(s.path) {
			// A registration in a test is a fixture, not an entry point.
			continue
		}
		if idents != "" {
			s.idents = strings.Split(idents, ",")
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// goRouteResolver ties Go handler and command names to declarations.
type goRouteResolver struct {
	ctx     context.Context
	tx      *sql.Tx
	imports map[int64]map[string]string
	tables  resolverTables
}

func recoverGoRoutes(ctx context.Context, tx *sql.Tx, wants func(...RouteFramework) bool, rep *RouteReport) error {
	sites, err := goRouteSites(ctx, tx)
	if err != nil || len(sites) == 0 {
		return err
	}
	r := &goRouteResolver{ctx: ctx, tx: tx}
	if r.imports, err = importsByFile(ctx, tx, "go"); err != nil {
		return err
	}
	if r.tables, err = loadResolverTables(ctx, tx, "go"); err != nil {
		return err
	}
	var cobraFields, addCommands []goSite
	for _, s := range sites {
		switch {
		case s.kind == CallSiteField:
			cobraFields = append(cobraFields, s)
		case s.callee == "AddCommand":
			addCommands = append(addCommands, s)
		default:
			fw, ok := r.classifyHTTP(s)
			if !ok || !wants(fw) {
				continue
			}
			b, err := r.binding(s, fw)
			if err != nil {
				return err
			}
			rep.Bindings = append(rep.Bindings, b)
		}
	}
	if !wants(FrameworkCobra) {
		return nil
	}
	return r.cobraTree(cobraFields, addCommands, rep)
}

// importsAny reports whether the file imports pkg or a package beneath it
// (`github.com/go-chi/chi/v5` for chi).
func (r *goRouteResolver) importsAny(fileID int64, pkg string) bool {
	for _, p := range r.imports[fileID] {
		if p == pkg || strings.HasPrefix(p, pkg+"/") {
			return true
		}
	}
	return false
}

// attributable reports whether a site in this file can belong to pkg: the file
// imports it, and the qualifier is not an import of some OTHER package —
// `http.Get(url)` in a chi file is a client call, not a chi route. A qualifier
// that is not an import is a receiver variable and passes.
func (r *goRouteResolver) attributable(s goSite, pkg string) bool {
	if !r.importsAny(s.fileID, pkg) {
		return false
	}
	p, isImport := r.imports[s.fileID][s.qualifier]
	return !isImport || p == pkg || strings.HasPrefix(p, pkg+"/")
}

type frameworkImport struct {
	fw  RouteFramework
	pkg string
}

var (
	handleFrameworks = []frameworkImport{
		{FrameworkChi, importChi}, {FrameworkGorilla, importGorilla}, {FrameworkNetHTTP, importNetHTTP},
	}
	upperVerbFrameworks = []frameworkImport{{FrameworkGin, importGin}, {FrameworkEcho, importEcho}}
	chiVerbFrameworks   = []frameworkImport{{FrameworkChi, importChi}}
)

// classifyHTTP attributes a call site to the first candidate framework the
// file imports, or reports that it is not a registration.
func (r *goRouteResolver) classifyHTTP(s goSite) (RouteFramework, bool) {
	var candidates []frameworkImport
	switch {
	case s.callee == "Handle" || s.callee == "HandleFunc":
		// Exactly (pattern, handler). gin's Handle(method, path, h...) has three
		// and would otherwise report its method string as the route.
		if s.argCount == 2 {
			candidates = handleFrameworks
		}
	case upperVerbs[s.callee] || chiVerbs[s.callee]:
		// Verb names are common method names; only a literal "/..." route on a
		// receiver makes the call a registration rather than, say, a client request.
		if s.hasString && strings.HasPrefix(s.str, "/") && s.qualifier != "" {
			candidates = chiVerbFrameworks
			if upperVerbs[s.callee] {
				candidates = upperVerbFrameworks
			}
		}
	}
	for _, c := range candidates {
		if r.attributable(s, c.pkg) {
			return c.fw, true
		}
	}
	return "", false
}

// positionalArgs rebuilds a call's argument list when every argument is either
// the one string literal or a recorded name. The literal is taken to be the
// first argument, which every recognised registration shape satisfies. ok is
// false when any argument was an expression or fell past the cap: positions are
// then unknown and no handler is named.
func positionalArgs(s goSite) (args []string, ok bool) {
	lit := 0
	if s.hasString {
		lit = 1
	}
	if s.argCount != len(s.idents)+lit {
		return nil, false
	}
	if s.hasString {
		return append([]string{""}, s.idents...), true
	}
	return s.idents, true
}

func (r *goRouteResolver) binding(s goSite, fw RouteFramework) (RouteBinding, error) {
	b := RouteBinding{Framework: fw, Path: s.path, Line: s.line}
	args, ok := positionalArgs(s)
	switch {
	case s.hasString:
		b.Route = s.str
	case ok && len(args) > 0:
		b.Route, b.Dynamic = args[0], true
	default:
		b.Dynamic = true
	}
	switch {
	case upperVerbs[s.callee] && s.callee != "Any":
		b.Method = s.callee
	case chiVerbs[s.callee]:
		b.Method = strings.ToUpper(s.callee)
	case !b.Dynamic:
		// Go 1.22 ServeMux patterns carry the method: "GET /items/{id}".
		if m, rest, found := strings.Cut(b.Route, " "); found && m != "" && m == strings.ToUpper(m) {
			b.Method, b.Route = m, strings.TrimSpace(rest)
		}
	}
	var text string
	if ok && len(args) > 1 {
		text = args[1]
		if fw == FrameworkGin {
			// gin chains middleware before the handler: the handler is last.
			text = args[len(args)-1]
		}
	}
	h, err := r.handler(s.fileID, s.path, text)
	b.Handler = h
	return b, err
}

// handler ties a Go name (`h`, `pkg.H`, `s.h`) to a declaration.
func (r *goRouteResolver) handler(fileID int64, filePath, text string) (RouteHandler, error) {
	if text == "" {
		return RouteHandler{Confidence: HandlerUnresolved}, nil
	}
	i := strings.LastIndex(text, ".")
	switch {
	case i < 0:
		return r.samePackageHandler(path.Dir(filePath), text)
	case r.isImport(fileID, text[:i]):
		return r.qualifiedHandler(fileID, text, text[:i], text[i+1:])
	default:
		return r.methodValueHandler(path.Dir(filePath), text, text[i+1:])
	}
}

func (r *goRouteResolver) isImport(fileID int64, name string) bool {
	_, ok := r.imports[fileID][name]
	return ok
}

// samePackageHandler: a bare name is the package's top-level function of that
// name — unless a local variable shadows it, which is the residual risk.
func (r *goRouteResolver) samePackageHandler(dir, text string) (RouteHandler, error) {
	h := RouteHandler{Text: text, Confidence: HandlerUnresolved}
	ids := r.tables.targets[dir][text]
	if len(ids) > 1 {
		h.Candidates, h.Confidence = len(ids), HandlerAmbiguous
		return h, nil
	}
	if len(ids) == 1 {
		n, err := nodeByID(r.ctx, r.tx, ids[0].id)
		if err != nil || n == nil {
			return h, err
		}
		h.Node, h.Confidence = n, HandlerSamePackage
	}
	return h, nil
}

// qualifiedHandler resolves `pkg.H` through the file's own import, with the
// same rule the call resolver uses.
func (r *goRouteResolver) qualifiedHandler(fileID int64, text, qual, name string) (RouteHandler, error) {
	h := RouteHandler{Text: text, Confidence: HandlerUnresolved}
	target, bucket := resolveOne(r.imports[fileID], qual, name, r.tables.pkgDirs, r.tables.targets)
	switch bucket {
	case bucketResolved:
		n, err := nodeByID(r.ctx, r.tx, target.id)
		if err != nil || n == nil {
			return h, err
		}
		h.Node, h.Confidence = n, HandlerResolved
	case bucketExternal:
		h.Confidence = HandlerExternal
	}
	return h, nil
}

// methodValueHandler matches `s.handleX` to the package's methods named
// handleX by name alone: the receiver's type is never checked, so one match is
// name-match and several are ambiguous.
func (r *goRouteResolver) methodValueHandler(dir, text, name string) (RouteHandler, error) {
	h := RouteHandler{Text: text, Confidence: HandlerUnresolved}
	methods, err := goMethodsNamed(r.ctx, r.tx, dir, name)
	if err != nil {
		return h, err
	}
	switch {
	case len(methods) == 1:
		h.Node, h.Confidence = &methods[0], HandlerNameMatch
	case len(methods) > 1:
		h.Candidates, h.Confidence = len(methods), HandlerAmbiguous
	}
	return h, nil
}
