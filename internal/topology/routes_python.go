package topology

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

var (
	flaskDecorators   = map[string]bool{"route": true, "get": true, "post": true, "put": true, "delete": true, "patch": true}
	fastAPIDecorators = map[string]bool{
		"get": true, "post": true, "put": true, "delete": true, "patch": true,
		"options": true, "head": true, "trace": true, "api_route": true, "websocket": true,
	}
)

// pySite is one decorator site with a qualifier (`@app.route`, not `@cache`).
type pySite struct {
	fileID, enclosing int64
	path, callee, str string
	line              int
	hasString         bool
}

func recoverPythonRoutes(ctx context.Context, tx *sql.Tx, wants func(...RouteFramework) bool, rep *RouteReport) error {
	modules, err := pythonImportsByFile(ctx, tx)
	if err != nil {
		return err
	}
	sites, err := pythonDecoratorSites(ctx, tx)
	if err != nil {
		return err
	}
	for _, s := range sites {
		fw, ok := pythonFramework(modules[s.fileID], s.callee)
		if !ok || !wants(fw) {
			continue
		}
		b, err := pythonBinding(ctx, tx, s, fw)
		if err != nil {
			return err
		}
		rep.Bindings = append(rep.Bindings, b)
	}
	return nil
}

func pythonDecoratorSites(ctx context.Context, tx *sql.Tx) ([]pySite, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT cs.file_id, f.path, cs.callee, cs.start_line,
		       cs.first_string_arg IS NOT NULL, IFNULL(cs.first_string_arg, ''), IFNULL(cs.enclosing_id, 0)
		  FROM topology_call_sites cs
		  JOIN topology_files f ON f.id = cs.file_id
		 WHERE cs.language = 'python' AND cs.site_kind = ? AND cs.qualifier IS NOT NULL
		 ORDER BY f.path, cs.start_byte, cs.id`, string(CallSiteDecorator))
	if err != nil {
		return nil, fmt.Errorf("topology: routes: python sites: %w", err)
	}
	defer rows.Close()
	var sites []pySite
	for rows.Next() {
		var s pySite
		if err := rows.Scan(&s.fileID, &s.path, &s.callee, &s.line, &s.hasString, &s.str, &s.enclosing); err != nil {
			return nil, fmt.Errorf("topology: routes: python site scan: %w", err)
		}
		sites = append(sites, s)
	}
	return sites, rows.Err()
}

// pythonFramework attributes a decorator to the framework its file imports.
// FastAPI is checked first: a file importing both is a FastAPI app that uses
// some Flask utility far more often than the reverse.
func pythonFramework(modules []string, callee string) (RouteFramework, bool) {
	switch {
	case importsModule(modules, "fastapi") && fastAPIDecorators[callee]:
		return FrameworkFastAPI, true
	case importsModule(modules, "flask") && flaskDecorators[callee]:
		return FrameworkFlask, true
	}
	return "", false
}

func pythonBinding(ctx context.Context, tx *sql.Tx, s pySite, fw RouteFramework) (RouteBinding, error) {
	b := RouteBinding{
		Framework: fw, Route: s.str, Dynamic: !s.hasString, Path: s.path, Line: s.line,
		Handler: RouteHandler{Confidence: HandlerUnresolved},
	}
	switch s.callee {
	case "route", "api_route":
		// The method list is a keyword argument the site does not record.
	case "websocket":
		b.Method = "WEBSOCKET"
	default:
		b.Method = strings.ToUpper(s.callee)
	}
	if s.enclosing == 0 {
		return b, nil
	}
	n, err := nodeByID(ctx, tx, s.enclosing)
	if err != nil || n == nil {
		return b, err
	}
	b.Handler = RouteHandler{Text: n.Name, Node: n, Confidence: HandlerDecorated}
	return b, nil
}

// pythonImportsByFile maps each Python file to the module names it imports.
// Python import nodes carry the module in Name and an empty Qualified, which is
// why the Go resolver's importsByFile cannot be reused here.
func pythonImportsByFile(ctx context.Context, tx *sql.Tx) (map[int64][]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT file_id, name FROM topology_nodes WHERE kind = ? AND language = 'python'`, string(KindImport))
	if err != nil {
		return nil, fmt.Errorf("topology: routes: python imports: %w", err)
	}
	defer rows.Close()
	out := map[int64][]string{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("topology: routes: python import scan: %w", err)
		}
		out[id] = append(out[id], name)
	}
	return out, rows.Err()
}

func importsModule(modules []string, root string) bool {
	for _, m := range modules {
		if m == root || strings.HasPrefix(m, root+".") {
			return true
		}
	}
	return false
}
