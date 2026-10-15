package topology

// indexer_calls_modules.go — the module resolver: cross-file call edges for
// Python, TypeScript and JavaScript, from what an import names.
//
// It follows a call across a file boundary only when the calling file's own
// import binding names the target: `from .a import f` then `f()`, `import a` then
// `a.f()`, `import * as m from "./a"` then `m.f()`, and a method call on
// `self`/`cls`/`this` to a method of the enclosing class. Everything else (calls
// through untyped variables, dynamic dispatch, re-exports, package aliases from a
// bundler or tsconfig) is left alone: no edge, never a guessed one. That is why it
// makes no completeness claim and why the Go-only census and caller-completeness
// answers stay Go-only; what it adds is derived edges a traversal may follow.
//
// It runs in the same transaction as the Go resolver (resolveCallsContext), after
// it, over the same rebuild scope, and writes edges with the same source, so the
// scoped clear and the re-index capture/restore by target identity cover it.

import (
	"context"
	"database/sql"
	"fmt"
	"path"
	"slices"
	"strings"
)

// Module-resolver confidences: an import-named target is as sure as a Go
// package-qualified one; a self/this method is a little less, since the method
// could be overridden by a subclass.
const (
	moduleCallConfidence = callEdgeConfidence
	selfCallConfidence   = 0.85
)

// moduleResolvedLanguages are the languages this resolver serves. A
// compile-time list, like supportedCallGraphLanguages, for the same reason.
func moduleResolvedLanguages() []string { return []string{"python", "typescript", "javascript"} }

func moduleLanguageResolved(lang string) bool {
	return slices.Contains(moduleResolvedLanguages(), lang)
}

// jsFamily reports whether lang is TypeScript or JavaScript, which import each
// other freely and resolve specifiers the same way.
func jsFamily(lang string) bool { return lang == "typescript" || lang == "javascript" }

// moduleDecl is one declaration a call may land on.
type moduleDecl struct {
	id                    int64
	kind, name, qualified string
	path                  string
}

func (d moduleDecl) identity() string { return nodeIdentity(d.path, d.qualified, d.name) }

// moduleBinding is one name an import binds in a file.
type moduleBinding struct {
	spec     string // the module as written
	imported string // the name imported from it; "" for the whole module
}

// moduleTables is the whole-index state one resolution pass reads.
type moduleTables struct {
	files    map[string]int64                  // path → file id, module languages only
	langOf   map[int64]string                  // file id → language
	top      map[int64]map[string][]moduleDecl // file id → top-level name → declarations
	members  map[int64]map[string]moduleDecl   // class id → member name → method
	parentOf map[int64]int64                   // method id → class id
	binds    map[int64]map[string]moduleBinding
}

func moduleLanguagePlaceholders() (string, []any) {
	langs := moduleResolvedLanguages()
	args := make([]any, 0, len(langs))
	for _, l := range langs {
		args = append(args, l)
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(langs)), ","), args
}

// resolveModuleCalls resolves the module-language call sites in scope (every one
// when not scoped) and inserts their edges.
func resolveModuleCalls(ctx context.Context, tx *sql.Tx, scoped bool) error {
	t, err := loadModuleTables(ctx, tx)
	if err != nil {
		return err
	}
	if len(t.files) == 0 {
		return nil
	}
	edges, err := collectModuleEdges(ctx, tx, &t, scoped)
	if err != nil {
		return err
	}
	return insertResolvedEdges(tx, edges)
}

func loadModuleTables(ctx context.Context, tx *sql.Tx) (moduleTables, error) {
	t := moduleTables{
		files: map[string]int64{}, langOf: map[int64]string{}, top: map[int64]map[string][]moduleDecl{},
		members: map[int64]map[string]moduleDecl{}, parentOf: map[int64]int64{}, binds: map[int64]map[string]moduleBinding{},
	}
	ph, args := moduleLanguagePlaceholders()
	if err := t.loadFiles(ctx, tx, ph, args); err != nil {
		return t, err
	}
	if len(t.files) == 0 {
		return t, nil
	}
	if err := t.loadParents(ctx, tx, ph, args); err != nil {
		return t, err
	}
	if err := t.loadDecls(ctx, tx, ph, args); err != nil {
		return t, err
	}
	return t, t.loadBindings(ctx, tx, ph, args)
}

//nolint:gosec // G202: the IN list is placeholders only; every value is bound
func (t *moduleTables) loadFiles(ctx context.Context, tx *sql.Tx, ph string, args []any) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, path, language FROM topology_files WHERE language IN (`+ph+`)`, args...)
	if err != nil {
		return fmt.Errorf("topology: module calls: files: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var p, lang string
		if err := rows.Scan(&id, &p, &lang); err != nil {
			return fmt.Errorf("topology: module calls: files: %w", err)
		}
		t.files[p], t.langOf[id] = id, lang
	}
	return rows.Err()
}

// loadParents maps each method to the class that contains it.
//
//nolint:gosec // G202: the IN list is placeholders only; every value is bound
func (t *moduleTables) loadParents(ctx context.Context, tx *sql.Tx, ph string, args []any) error {
	q := `SELECT e.from_id, e.to_id FROM topology_edges e
		   JOIN topology_nodes p ON p.id = e.from_id
		   JOIN topology_nodes c ON c.id = e.to_id
		  WHERE e.kind = ? AND p.kind = ? AND c.kind = ? AND c.language IN (` + ph + `)`
	rows, err := tx.QueryContext(ctx, q, append([]any{string(EdgeContains), string(KindClass), string(KindMethod)}, args...)...)
	if err != nil {
		return fmt.Errorf("topology: module calls: members: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var class, method int64
		if err := rows.Scan(&class, &method); err != nil {
			return fmt.Errorf("topology: module calls: members: %w", err)
		}
		t.parentOf[method] = class
	}
	return rows.Err()
}

// loadDecls indexes the callable declarations: methods under their class, the
// rest under their file's top level.
//
//nolint:gosec // G202: the IN list is placeholders only; every value is bound
func (t *moduleTables) loadDecls(ctx context.Context, tx *sql.Tx, ph string, args []any) error {
	q := `SELECT n.id, n.file_id, n.kind, n.name, n.qualified, f.path
		   FROM topology_nodes n JOIN topology_files f ON f.id = n.file_id
		  WHERE n.language IN (` + ph + `) AND n.kind IN (?, ?, ?, ?)`
	rows, err := tx.QueryContext(ctx, q, append(args, string(KindFunction), string(KindMethod), string(KindClass), string(KindTest))...)
	if err != nil {
		return fmt.Errorf("topology: module calls: declarations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var d moduleDecl
		var file int64
		if err := rows.Scan(&d.id, &file, &d.kind, &d.name, &d.qualified, &d.path); err != nil {
			return fmt.Errorf("topology: module calls: declarations: %w", err)
		}
		if class, ok := t.parentOf[d.id]; ok {
			if t.members[class] == nil {
				t.members[class] = map[string]moduleDecl{}
			}
			t.members[class][d.name] = d
			continue
		}
		if t.top[file] == nil {
			t.top[file] = map[string][]moduleDecl{}
		}
		t.top[file][d.name] = append(t.top[file][d.name], d)
	}
	return rows.Err()
}

//nolint:gosec // G202: the IN list is placeholders only; every value is bound
func (t *moduleTables) loadBindings(ctx context.Context, tx *sql.Tx, ph string, args []any) error {
	q := `SELECT file_id, callee, IFNULL(qualifier, ''), IFNULL(first_string_arg, '')
		   FROM topology_call_sites WHERE site_kind = ? AND language IN (` + ph + `)`
	rows, err := tx.QueryContext(ctx, q, append([]any{string(CallSiteBinding)}, args...)...)
	if err != nil {
		return fmt.Errorf("topology: module calls: bindings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var file int64
		var local string
		var b moduleBinding
		if err := rows.Scan(&file, &local, &b.spec, &b.imported); err != nil {
			return fmt.Errorf("topology: module calls: bindings: %w", err)
		}
		if t.binds[file] == nil {
			t.binds[file] = map[string]moduleBinding{}
		}
		t.binds[file][local] = b
	}
	return rows.Err()
}

// moduleSite is one call site to resolve.
type moduleSite struct {
	file, enclosing   int64
	callee, qualifier string
	path              string
}

//nolint:gosec // G202: the IN list is placeholders only; every value is bound
func collectModuleEdges(ctx context.Context, tx *sql.Tx, t *moduleTables, scoped bool) ([]resolvedEdge, error) {
	ph, args := moduleLanguagePlaceholders()
	q := `SELECT cs.file_id, IFNULL(cs.enclosing_id, 0), cs.callee, IFNULL(cs.qualifier, ''), f.path
	        FROM topology_call_sites cs JOIN topology_files f ON f.id = cs.file_id
	       WHERE cs.site_kind = ? AND cs.language IN (` + ph + `)`
	if scoped {
		q += ` AND cs.file_id IN (SELECT file_id FROM ` + scopeTable + `)`
	}
	rows, err := tx.QueryContext(ctx, q, append([]any{string(CallSiteCall)}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("topology: module calls: sites: %w", err)
	}
	defer rows.Close()
	var out []resolvedEdge
	seen := map[[2]int64]bool{}
	for rows.Next() {
		var s moduleSite
		if err := rows.Scan(&s.file, &s.enclosing, &s.callee, &s.qualifier, &s.path); err != nil {
			return nil, fmt.Errorf("topology: module calls: sites: %w", err)
		}
		target, conf, ok := t.resolve(s)
		if !ok || s.enclosing == 0 || target.id == s.enclosing || seen[[2]int64{s.enclosing, target.id}] {
			continue
		}
		seen[[2]int64{s.enclosing, target.id}] = true
		out = append(out, resolvedEdge{from: s.enclosing, to: target.id, toIdentity: target.identity(), confidence: conf})
	}
	return out, rows.Err()
}

// resolve finds the one declaration a site calls, or reports that it cannot.
func (t *moduleTables) resolve(s moduleSite) (moduleDecl, float64, bool) {
	lang := t.langOf[s.file]
	switch {
	case s.qualifier == "":
		b, ok := t.binds[s.file][s.callee]
		// A whole-module or namespace binding is not callable as a declaration;
		// a default import names no declaration (the module's default export
		// has no name the index records), so it finds nothing either.
		if !ok || b.imported == "" || b.imported == "*" {
			return moduleDecl{}, 0, false
		}
		d, ok := t.inModule(lang, s.path, b.spec, b.imported)
		return d, moduleCallConfidence, ok
	case isSelfQualifier(lang, s.qualifier):
		d, ok := t.members[t.parentOf[s.enclosing]][s.callee]
		return d, selfCallConfidence, ok && t.parentOf[s.enclosing] != 0
	}
	b, ok := t.binds[s.file][s.qualifier]
	if !ok {
		return moduleDecl{}, 0, false
	}
	if b.imported == "" || b.imported == "*" {
		d, ok := t.inModule(lang, s.path, b.spec, s.callee)
		return d, moduleCallConfidence, ok
	}
	// `from pkg import mod` then `mod.f()`: the imported name is a submodule.
	if lang == "python" {
		if d, ok := t.inModule(lang, s.path, joinPythonModule(b.spec, b.imported), s.callee); ok {
			return d, moduleCallConfidence, true
		}
	}
	// `from m import Cls` then `Cls.method()`: a member of the imported class.
	class, ok := t.inModule(lang, s.path, b.spec, b.imported)
	if !ok || class.kind != string(KindClass) {
		return moduleDecl{}, 0, false
	}
	d, ok := t.members[class.id][s.callee]
	return d, moduleCallConfidence, ok
}

func isSelfQualifier(lang, q string) bool {
	if lang == "python" {
		return q == "self" || q == "cls"
	}
	return q == "this"
}

// inModule is the one top-level declaration called name in the module spec
// resolves to, from the file at from. Several modules, or several declarations,
// is no answer.
func (t *moduleTables) inModule(lang, from, spec, name string) (moduleDecl, bool) {
	file, ok := t.moduleFile(lang, from, spec)
	if !ok {
		return moduleDecl{}, false
	}
	ds := t.top[file][name]
	if len(ds) != 1 {
		return moduleDecl{}, false
	}
	return ds[0], true
}

// moduleFile is the indexed file a module specifier names, when exactly one does.
func (t *moduleTables) moduleFile(lang, from, spec string) (int64, bool) {
	var cands []string
	suffix := false
	switch {
	case lang == "python":
		cands, suffix = pythonModuleCandidates(from, spec)
	case jsFamily(lang):
		cands = jsModuleCandidates(from, spec)
	}
	for _, c := range cands {
		if id, ok := t.files[c]; ok {
			return id, true
		}
	}
	if !suffix {
		return 0, false
	}
	// An absolute Python import names a module from a source root (src/, a
	// package directory) the index does not know: accept the one file whose path
	// ends with the module's path, and no answer when several do.
	var found int64
	for p, id := range t.files {
		for _, c := range cands {
			if strings.HasSuffix(p, "/"+c) {
				if found != 0 && found != id {
					return 0, false
				}
				found = id
			}
		}
	}
	return found, found != 0
}

// pythonModuleCandidates lists the files a Python module specifier may be, in
// preference order, and whether a path-suffix match is allowed (absolute
// imports only).
func pythonModuleCandidates(from, spec string) ([]string, bool) {
	dots := len(spec) - len(strings.TrimLeft(spec, "."))
	rest := strings.ReplaceAll(spec[dots:], ".", "/")
	if dots == 0 {
		return []string{rest + ".py", rest + "/__init__.py"}, true
	}
	base := path.Dir(from)
	for range dots - 1 {
		base = path.Dir(base)
	}
	if rest == "" {
		return []string{path.Join(base, "__init__.py")}, false
	}
	p := path.Join(base, rest)
	return []string{p + ".py", p + "/__init__.py"}, false
}

// joinPythonModule names a submodule of spec: "." + "a" is ".a", "pkg" + "a"
// is "pkg.a".
func joinPythonModule(spec, name string) string {
	if strings.HasSuffix(spec, ".") {
		return spec + name
	}
	return spec + "." + name
}

// jsExtensions are the files a relative specifier without an extension may be,
// in the order bundlers and tsc try them.
var jsExtensions = []string{".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs"}

// jsModuleCandidates lists the files a relative JS/TS specifier may be. A bare
// specifier names a package (node_modules, a bundler alias) and is not followed.
func jsModuleCandidates(from, spec string) []string {
	if !strings.HasPrefix(spec, "./") && !strings.HasPrefix(spec, "../") {
		return nil
	}
	p := path.Join(path.Dir(from), spec)
	var out []string
	if ext := path.Ext(p); slices.Contains(jsExtensions, ext) {
		out = append(out, p)
		// TypeScript's ESM convention imports "./a.js" for the source file a.ts.
		stem := strings.TrimSuffix(p, ext)
		for _, e := range []string{".ts", ".tsx", ".mts", ".cts"} {
			out = append(out, stem+e)
		}
		return out
	}
	for _, e := range jsExtensions {
		out = append(out, p+e)
	}
	for _, e := range jsExtensions {
		out = append(out, p+"/index"+e)
	}
	return out
}
