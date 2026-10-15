package topology

// indexer_calls_modules.go — the module resolver: cross-file call edges for
// Python, TypeScript and JavaScript, from what an import names.
//
// It follows a call across a file boundary only when the calling file's own
// import binding names the target: `from .a import f` then `f()`, `import a` then
// `a.f()`, `import * as m from "./a"` then `m.f()`, and a method call on
// `self`/`cls`/`this` to a method of the enclosing class. Everything else (calls
// through untyped variables, dynamic dispatch, re-exports, package aliases from a
// bundler or tsconfig, a name bound twice to different things) is left alone: no
// edge, never a guessed one. That is why it makes no completeness claim and why
// the Go-only census and caller-completeness answers stay Go-only; what it adds is
// derived edges a traversal may follow.
//
// It runs in the same transaction as the Go resolver (resolveCallsContext), after
// it, and writes edges with the same source, so the scoped clear and the re-index
// capture/restore by target identity cover it. A scoped pass resolves the rebuild
// scope plus every file that imports a file in it, so a declaration added to a
// callee is found by its importers without waiting for a full rebuild. The
// extractors qualify members by their container (Class.method), which keeps each
// target's identity unique, so a restore never re-attaches an edge to a
// same-named declaration.

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

// moduleScopeTable holds the files a scoped module pass resolves.
const moduleScopeTable = "temp.module_scope"

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

// moduleBinding is one name an import binds in a file. conflict marks a name
// bound more than once to different things (a try/except import, a second
// import of the name): which one a call means is not known, so it means none.
type moduleBinding struct {
	spec     string // the module as written
	imported string // the name imported from it; "" for the whole module
	conflict bool
}

// moduleTables is the whole-index state one resolution pass reads.
type moduleTables struct {
	files    map[string]int64                   // path → file id, module languages only
	pathOf   map[int64]string                   // file id → path
	langOf   map[int64]string                   // file id → language
	byBase   map[string][]string                // base name → paths, for suffix matches
	top      map[int64]map[string][]moduleDecl  // file id → top-level name → declarations
	members  map[int64]map[string][]moduleDecl  // class or type id → member name → members
	parentOf map[int64]int64                    // member id → its class or type
	binds    map[int64]map[string]moduleBinding // file id → local name → binding
	modMemo  map[moduleKey]moduleHit            // module lookups, this pass
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
	if scoped {
		if err := t.widenScope(ctx, tx); err != nil {
			return err
		}
	}
	edges, err := collectModuleEdges(ctx, tx, &t, scoped)
	if err != nil {
		return err
	}
	return insertResolvedEdges(tx, edges)
}

// widenScope fills moduleScopeTable with the rebuild scope's module-language
// files and every file that imports one of them, and clears the importers' own
// derived edges (the scope's were cleared by resolveCallsContext), so all of them
// are resolved afresh against the changed files' new declarations.
func (t *moduleTables) widenScope(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS module_scope(file_id INTEGER PRIMARY KEY)`); err != nil {
		return fmt.Errorf("topology: module calls: scope: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+moduleScopeTable); err != nil {
		return fmt.Errorf("topology: module calls: scope: %w", err)
	}
	changed, err := scopeFileIDs(ctx, tx)
	if err != nil {
		return err
	}
	var importers []int64
	for file, names := range t.binds {
		if !changed[file] && t.importsAny(file, names, changed) {
			importers = append(importers, file)
		}
	}
	for id := range changed {
		if _, ok := t.langOf[id]; ok {
			importers = append(importers, id)
		}
	}
	for _, id := range importers {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO `+moduleScopeTable+`(file_id) VALUES (?)`, id); err != nil {
			return fmt.Errorf("topology: module calls: scope: %w", err)
		}
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM topology_edges WHERE source = ? AND from_id IN (
		SELECT n.id FROM topology_nodes n JOIN `+moduleScopeTable+` s ON s.file_id = n.file_id)`, callResolverSource)
	if err != nil {
		return fmt.Errorf("topology: module calls: scoped clear: %w", err)
	}
	return nil
}

// importsAny reports whether one of a file's bindings resolves to a changed file.
func (t *moduleTables) importsAny(file int64, names map[string]moduleBinding, changed map[int64]bool) bool {
	lang, from := t.langOf[file], t.pathOf[file]
	for _, b := range names {
		if id, ok := t.moduleFile(lang, from, b.spec); ok && changed[id] {
			return true
		}
		if lang == "python" && b.imported != "" {
			if id, ok := t.moduleFile(lang, from, joinPythonModule(b.spec, b.imported)); ok && changed[id] {
				return true
			}
		}
	}
	return false
}

func scopeFileIDs(ctx context.Context, tx *sql.Tx) (map[int64]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT file_id FROM `+scopeTable)
	if err != nil {
		return nil, fmt.Errorf("topology: module calls: scope: %w", err)
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("topology: module calls: scope: %w", err)
		}
		out[id] = true
	}
	return out, rows.Err()
}

func loadModuleTables(ctx context.Context, tx *sql.Tx) (moduleTables, error) {
	t := moduleTables{
		files: map[string]int64{}, pathOf: map[int64]string{}, langOf: map[int64]string{}, byBase: map[string][]string{},
		top: map[int64]map[string][]moduleDecl{}, members: map[int64]map[string][]moduleDecl{},
		parentOf: map[int64]int64{}, binds: map[int64]map[string]moduleBinding{}, modMemo: map[moduleKey]moduleHit{},
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
		t.files[p], t.pathOf[id], t.langOf[id] = id, p, lang
		t.byBase[path.Base(p)] = append(t.byBase[path.Base(p)], p)
	}
	return rows.Err()
}

// loadParents maps each member (a method, a test method, a nested function) to
// the class or type that contains it.
//
//nolint:gosec // G202: the IN list is placeholders only; every value is bound
func (t *moduleTables) loadParents(ctx context.Context, tx *sql.Tx, ph string, args []any) error {
	q := `SELECT e.from_id, e.to_id FROM topology_edges e
		   JOIN topology_nodes p ON p.id = e.from_id
		   JOIN topology_nodes c ON c.id = e.to_id
		  WHERE e.kind = ? AND p.kind IN (?, ?) AND c.kind IN (?, ?, ?) AND c.language IN (` + ph + `)`
	rows, err := tx.QueryContext(ctx, q, append([]any{
		string(EdgeContains), string(KindClass), string(KindType),
		string(KindMethod), string(KindTest), string(KindFunction),
	}, args...)...)
	if err != nil {
		return fmt.Errorf("topology: module calls: members: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var owner, member int64
		if err := rows.Scan(&owner, &member); err != nil {
			return fmt.Errorf("topology: module calls: members: %w", err)
		}
		t.parentOf[member] = owner
	}
	return rows.Err()
}

// loadDecls indexes the callable declarations: members under their class or
// type, top-level ones under their file. A nested function (qualified by the
// function around it) is neither: no import or self call can name it.
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
		switch owner, ok := t.parentOf[d.id]; {
		case ok:
			if t.members[owner] == nil {
				t.members[owner] = map[string][]moduleDecl{}
			}
			t.members[owner][d.name] = append(t.members[owner][d.name], d)
		case d.qualified != "" && d.qualified != d.name:
			// nested: not reachable through an import
		default:
			if t.top[file] == nil {
				t.top[file] = map[string][]moduleDecl{}
			}
			t.top[file][d.name] = append(t.top[file][d.name], d)
		}
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
		if prev, ok := t.binds[file][local]; ok && (prev.spec != b.spec || prev.imported != b.imported) {
			b.conflict = true
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
		q += ` AND cs.file_id IN (SELECT file_id FROM ` + moduleScopeTable + `)`
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
		// a default import names no declaration (the module's default export has
		// no name the index records), so it finds nothing either.
		if !ok || b.conflict || b.imported == "" || b.imported == "*" {
			return moduleDecl{}, 0, false
		}
		d, ok := t.inModule(lang, s.path, b.spec, b.imported)
		return d, moduleCallConfidence, ok
	case isSelfQualifier(lang, s.qualifier):
		owner, ok := t.parentOf[s.enclosing]
		if !ok {
			return moduleDecl{}, 0, false
		}
		d, ok := one(t.members[owner][s.callee])
		return d, selfCallConfidence, ok
	}
	b, ok := t.binds[s.file][s.qualifier]
	if !ok || b.conflict {
		return moduleDecl{}, 0, false
	}
	return t.resolveMember(lang, s, b)
}

// resolveMember resolves a call qualified by an import binding: a function of the
// bound module, a function of a submodule a from-import named, or a member of an
// imported class.
func (t *moduleTables) resolveMember(lang string, s moduleSite, b moduleBinding) (moduleDecl, float64, bool) {
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
	d, ok := one(t.members[class.id][s.callee])
	return d, moduleCallConfidence, ok
}

// one is the single declaration of ds, or none when there are several or none.
func one(ds []moduleDecl) (moduleDecl, bool) {
	if len(ds) != 1 {
		return moduleDecl{}, false
	}
	return ds[0], true
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
	return one(t.top[file][name])
}
