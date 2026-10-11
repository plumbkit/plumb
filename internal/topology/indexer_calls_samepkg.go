package topology

import (
	"context"
	"database/sql"
	"fmt"
	"path"
	"strings"
)

// This file holds the two same-package tiers of the Go call resolver (#632).
// The qualified tier in indexer_calls.go resolves `pkg.F()` through the caller's
// import set; it never attempted the two shapes that make up most of a Go call
// graph:
//
//   - A BARE call, `F()`. In valid Go an unqualified call names a declaration in
//     the caller's own package, a builtin, or a local value (parameter, closure
//     variable). The resolver links it only to a top-level FUNCTION declared in
//     the caller's directory under the caller's own package clause, so an
//     external test package (`package foo_test`, same directory) never reaches
//     package foo's unexported functions, and a builtin, a conversion or a local
//     closure — none of which is a top-level function — lands in a census bucket
//     instead of an edge. The residual risk is a local value SHADOWING a
//     package function of the same name; it is why these edges carry a lower
//     confidence than the qualified tier's.
//   - A RECEIVER-SELF call, `r.M()` inside `func (r *T) …`. The extractor stores
//     the method's receiver variable in its signature and the receiver type in
//     its qualified name, so the call's receiver type is known without type
//     checking when, and only when, the qualifier IS that receiver variable. Any
//     other receiver — a local, a parameter, a field chain `r.db.Exec()`, a
//     promoted method of an embedded type — stays unresolved and counted, as
//     before.
//
// Both tiers only ever link within one directory and one package clause, which
// is what lets the incremental lifecycle stay scoped: a scoped pass widens its
// scope to the changed files' sibling Go files (samePackageScope) instead of
// forcing a full rebuild whenever a function is added or removed.

// samePackageCallConfidence sits below the qualified tier's 0.9: a qualified
// call is anchored on the caller's import set, while a bare or receiver-self
// call is anchored on a name that a local value could shadow.
const samePackageCallConfidence = 0.85

// pkgTarget is a same-package call target: a callTarget plus the file it lives
// in, so a call into the caller's own file can be left to the extractor's
// intra-file edge rather than duplicated.
type pkgTarget struct {
	callTarget
	fileID int64
}

// enclosingInfo is what a function or method's own declaration says about the
// variables a call inside it can be qualified by: its receiver, and its
// parameters with a named type.
type enclosingInfo struct {
	recvName string            // the receiver variable, e.g. "idx"; "" for a function
	recvBase string            // the receiver type without pointer or type parameters, e.g. "Indexer"
	params   map[string]string // parameter name → named type, "Store" or "topology.Store"
}

// samePackageTables are the lookups the receiver-aware tiers need. Keys of funcs
// and methods are dirPkgKey(dir, package clause).
type samePackageTables struct {
	filePkg   map[int64]string                  // file id → package clause
	dirPkg    map[string]string                 // directory → its non-test package clause
	funcs     map[string]map[string][]pkgTarget // top-level functions by name
	methods   map[string]map[string][]pkgTarget // methods by "Base.Name"
	enclosing map[int64]enclosingInfo           // function/method node id → its variables
}

func dirPkgKey(dir, pkg string) string { return dir + "\x00" + pkg }

func loadSamePackageTables(ctx context.Context, tx *sql.Tx, lang string) (samePackageTables, error) {
	t := samePackageTables{
		filePkg:   map[int64]string{},
		dirPkg:    map[string]string{},
		funcs:     map[string]map[string][]pkgTarget{},
		methods:   map[string]map[string][]pkgTarget{},
		enclosing: map[int64]enclosingInfo{},
	}
	if lang != "go" {
		return t, nil
	}
	if err := loadFilePackages(ctx, tx, lang, t.filePkg, t.dirPkg); err != nil {
		return t, err
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT n.id, n.kind, n.name, n.qualified, n.signature, n.file_id, f.path
		   FROM topology_nodes n
		   JOIN topology_files f ON f.id = n.file_id
		  WHERE n.language = ? AND n.kind IN (?, ?, ?)
		  ORDER BY n.id`, lang, string(KindFunction), string(KindMethod), string(KindTest))
	if err != nil {
		return t, fmt.Errorf("topology: resolve calls: same-package targets: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, fileID int64
		var kind, name, qualified, signature, p string
		if err := rows.Scan(&id, &kind, &name, &qualified, &signature, &fileID, &p); err != nil {
			return t, fmt.Errorf("topology: resolve calls: same-package target scan: %w", err)
		}
		pkg, ok := t.filePkg[fileID]
		if !ok {
			continue
		}
		key := dirPkgKey(path.Dir(p), pkg)
		target := pkgTarget{callTarget: callTarget{id: id, identity: nodeIdentity(p, qualified, name)}, fileID: fileID}
		base, isMethod := receiverBase(qualified)
		info := enclosingInfo{params: namedParams(signature)}
		if isMethod {
			info.recvName, info.recvBase = receiverVar(signature), base
		}
		if info.recvName != "" || len(info.params) > 0 {
			t.enclosing[id] = info
		}
		switch {
		case !isMethod && NodeKind(kind) == KindFunction:
			addPkgTarget(t.funcs, key, name, target)
		case isMethod:
			// A Test-kind method (a suite's TestX) is an entry point, not a
			// callee anyone writes `s.TestX()` for; it can enclose a call but is
			// not offered as a target.
			if NodeKind(kind) == KindMethod {
				addPkgTarget(t.methods, key, base+"."+name, target)
			}
		}
	}
	return t, rows.Err()
}

func addPkgTarget(m map[string]map[string][]pkgTarget, key, name string, target pkgTarget) {
	if m[key] == nil {
		m[key] = map[string][]pkgTarget{}
	}
	m[key][name] = append(m[key][name], target)
}

// loadFilePackages maps each file to its package clause, from the package node
// the extractor emits once per file, and each directory to its non-test clause
// (the package another directory's import of it names).
func loadFilePackages(ctx context.Context, tx *sql.Tx, lang string, out map[int64]string, dirs map[string]string) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT n.file_id, n.name, f.path FROM topology_nodes n
		   JOIN topology_files f ON f.id = n.file_id
		  WHERE n.kind = ? AND n.language = ?`,
		string(KindPackage), lang)
	if err != nil {
		return fmt.Errorf("topology: resolve calls: package clauses: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var fileID int64
		var name, p string
		if err := rows.Scan(&fileID, &name, &p); err != nil {
			return fmt.Errorf("topology: resolve calls: package clause scan: %w", err)
		}
		out[fileID] = name
		if !strings.HasSuffix(name, "_test") {
			dirs[path.Dir(p)] = name
		}
	}
	return rows.Err()
}

// nodeIdentity is the stable target identity restoreIncomingDerived matches on:
// file path, NUL, qualified name (or the bare name when there is none).
func nodeIdentity(p, qualified, name string) string {
	if qualified == "" {
		return p + "\x00" + name
	}
	return p + "\x00" + qualified
}

// receiverBase extracts the receiver type from a Go method's qualified name,
// "(*Indexer).resolveCalls" → "Indexer". The extractor already drops type
// parameters, so a generic receiver reads as its base name.
func receiverBase(qualified string) (string, bool) {
	if !strings.HasPrefix(qualified, "(") {
		return "", false
	}
	end := strings.Index(qualified, ").")
	if end < 0 {
		return "", false
	}
	base := strings.TrimPrefix(qualified[1:end], "*")
	if base == "" || base == "_" {
		return "", false
	}
	return base, true
}

// receiverVar extracts the receiver variable from a Go method's stored
// signature, "func (idx *Indexer) resolveCalls(…)" → "idx". An unnamed or blank
// receiver ("func (*Indexer) …", "func (_ T) …") has no variable a call could be
// qualified by, and yields "".
func receiverVar(signature string) string {
	rest, ok := strings.CutPrefix(signature, "func (")
	if !ok {
		return ""
	}
	end := strings.IndexByte(rest, ')')
	if end < 0 {
		return ""
	}
	fields := strings.Fields(rest[:end])
	if len(fields) != 2 || fields[0] == "_" {
		return ""
	}
	return fields[0]
}

// samePackageScope widens a scoped pass from the changed files to every indexed
// file in their directories. The same-package tiers link only within one
// directory, so a function added to, removed from or renamed in one file can
// change a sibling's edges without the sibling changing. Paths of deleted files
// still contribute their directory.
func samePackageScope(ctx context.Context, tx *sql.Tx, changed []string) ([]int64, error) {
	dirs := map[string]bool{}
	for _, p := range changed {
		dirs[path.Dir(p)] = true
	}
	var out []int64
	for dir := range dirs {
		var rows *sql.Rows
		var err error
		if dir == "." {
			rows, err = tx.QueryContext(ctx, `SELECT id, path FROM topology_files WHERE instr(path, '/') = 0`)
		} else {
			prefix := dir + "/"
			rows, err = tx.QueryContext(ctx,
				`SELECT id, path FROM topology_files WHERE substr(path, 1, ?) = ?`, len(prefix), prefix)
		}
		if err != nil {
			return nil, fmt.Errorf("topology: resolve calls: sibling scope: %w", err)
		}
		for rows.Next() {
			var id int64
			var p string
			if err := rows.Scan(&id, &p); err != nil {
				rows.Close()
				return nil, fmt.Errorf("topology: resolve calls: sibling scope scan: %w", err)
			}
			if path.Dir(p) == dir {
				out = append(out, id)
			}
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// samePackageOutcome decides the bucket of a bare or receiver-self site from its
// same-package candidates, and returns one edge per cross-file candidate not
// already linked from this caller. A site with no candidate keeps the bucket it
// always had: a bare one names no function of the package, a receiver one stays
// an unresolved method call.
func samePackageOutcome(fileID, enclosing int64, cands []pkgTarget, bare bool, seen map[[2]int64]bool) (resolveBucket, []resolvedEdge) {
	if len(cands) == 0 {
		if bare {
			return bucketBareUnmatched, nil
		}
		return bucketReceiver, nil
	}
	cross := make([]pkgTarget, 0, len(cands))
	for _, c := range cands {
		if c.fileID != fileID {
			cross = append(cross, c)
		}
	}
	if len(cross) == 0 {
		return bucketSameFile, nil
	}
	if enclosing == 0 {
		return bucketNoCaller, nil
	}
	var out []resolvedEdge
	repeat := false
	for _, c := range cross {
		if seen[[2]int64{enclosing, c.id}] {
			repeat = true
			continue
		}
		out = append(out, resolvedEdge{from: enclosing, to: c.id, toIdentity: c.identity, confidence: samePackageCallConfidence})
	}
	if len(out) > 0 {
		return bucketResolved, out
	}
	if repeat {
		return bucketRepeat, nil
	}
	return bucketNoCaller, nil
}
