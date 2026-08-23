package topology

import (
	"context"
	"database/sql"
	"fmt"
	"path"
	"unicode"
)

// callResolverSource marks the edges this pass owns. Like the import resolver's,
// they are DERIVED: the pass deletes and rebuilds every edge carrying this
// source, so no extractor may ever emit it.
const callResolverSource = "call-resolver"

// callEdgeConfidence sits below an extractor's 1.0 for the same reason the
// import resolver's does: the link is inferred from a qualifier matched against
// a file's import set and a path suffix, not read from a resolved reference.
const callEdgeConfidence = 0.9

// Meta keys under which a pass publishes what it actually did. They are written
// on every pass so a reader never sees a stale count from an earlier index, and
// they are the source for the call-graph line topology_status prints — the place
// a user reads how little of their call graph this covers.
const (
	metaCallResolved        = "callgraph.resolved"
	metaCallResolvedNonTest = "callgraph.resolved_nontest"
	metaCallUnresolvedRecv  = "callgraph.unresolved_receiver"
	metaCallExternal        = "callgraph.external_package"
	metaCallUnmatched       = "callgraph.unmatched_target"
	metaCallRepeatOfEdge    = "callgraph.repeat_of_edge"
	metaCallNoCallerNode    = "callgraph.no_caller_node"
	metaCallSites           = "callgraph.call_sites"
	metaCallQualifiedSites  = "callgraph.qualified_sites"
)

// callResolution is one pass's tally. Every qualified call site lands in exactly
// one bucket, so the buckets sum to the qualified-site count — which is what
// makes "how much of the call graph does this reach" answerable rather than
// inferable from an edge count alone.
//
// The invariant is held by construction rather than by care: collectResolvedEdges
// calls countBucket exactly once per qualified site, on every path including the
// resolved one, and there is no `continue` between the two. The two buckets that
// are not resolution outcomes at all — a site whose edge was already emitted, and
// a site with no caller node to hang an edge on — exist BECAUSE they were the two
// ways a site used to leave the accounting: the first was skipped silently, the
// second was folded into unmatchedTarget, whose printed wording ("names no
// top-level function") is a different and untrue claim about it.
type callResolution struct {
	resolved        int
	resolvedNonTest int
	unresolvedRecv  int
	externalPackage int
	unmatchedTarget int
	// repeatOfEdge counts qualified sites that resolved to a caller→target pair
	// an earlier site already produced an edge for. The edge is emitted once, so
	// these sites contribute no edge and must not be counted as if they had.
	repeatOfEdge int
	// noCallerNode counts qualified sites that resolved to a real target but have
	// no enclosing declaration to be the edge's tail (or whose tail would be the
	// target itself). It is a fact about the CALLER, not about the target, which
	// is why it cannot share unmatchedTarget's sentence.
	noCallerNode int
	// callSites is EVERY recorded call site in the language, qualified or not.
	// It is the denominator the reach percentage is published against: measuring
	// reach against qualified sites alone flatters the number by excluding the
	// bare-identifier calls this resolver does not attempt either.
	callSites int
	// qualifiedSites is the subset carrying a qualifier, and equals the sum of
	// the six buckets above.
	qualifiedSites int
}

// resolveCalls turns recorded call sites into cross-file `calls` edges, for
// admitted languages only.
//
// What it resolves and what it deliberately does not:
//
//   - A PACKAGE-QUALIFIED call (`stats.Open(…)`) is resolved by looking the
//     qualifier up in THAT file's own import set, mapping the import path to an
//     indexed directory, and finding an exported top-level function of that name
//     there. Per-file import sets are what keep this from degenerating into
//     name-only matching: this workspace has 2,588 callables sharing a name with
//     another callable, so a global by-name resolver would be wrong at scale.
//   - A METHOD CALL on a receiver (`idx.linkImports()`) is NOT resolved, and is
//     counted instead. The Go extractor parses with SkipObjectResolution, so
//     there is no type information anywhere in the index that could turn a
//     receiver VARIABLE into the type whose method was called. Textual receiver
//     matching would manufacture wrong edges across those 2,588 colliding names.
//     A method call is the single most common Go call there is, so this is not a
//     rounding error — it is most of the call graph, and it is left absent and
//     labelled rather than present and wrong.
//
// Like linkImports, the edges are rebuilt wholesale on every pass rather than
// patched per file: a cross-file edge is keyed by two node rowids and both
// endpoints CASCADE, so re-indexing either side silently deletes it. Rebuilding
// is what makes that harmless today; making these edges survive an incremental
// re-index without a full pass is a separate piece of work, and until it lands
// nothing may depend on them persisting between passes.
func (idx *Indexer) resolveCalls(ctx context.Context) error {
	tx, err := idx.db.Begin()
	if err != nil {
		return fmt.Errorf("topology: resolve calls: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`DELETE FROM topology_edges WHERE source = ?`, callResolverSource); err != nil {
		return fmt.Errorf("topology: resolve calls: clear: %w", err)
	}

	var total callResolution
	for _, lang := range supportedCallGraphLanguages() {
		admitted, err := hasPackageNodeTx(ctx, tx, lang)
		if err != nil {
			return err
		}
		if !admitted {
			// The second half of the gate is missing: the index holds no package
			// node for this language, so there is nothing to resolve THROUGH. Not
			// an assertion that the workspace has no calls.
			continue
		}
		r, err := resolveLanguageCalls(ctx, tx, lang)
		if err != nil {
			return err
		}
		total.add(r)
	}
	if err := writeCallMeta(tx, total); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *callResolution) add(o callResolution) {
	r.resolved += o.resolved
	r.resolvedNonTest += o.resolvedNonTest
	r.unresolvedRecv += o.unresolvedRecv
	r.externalPackage += o.externalPackage
	r.unmatchedTarget += o.unmatchedTarget
	r.repeatOfEdge += o.repeatOfEdge
	r.noCallerNode += o.noCallerNode
	r.callSites += o.callSites
	r.qualifiedSites += o.qualifiedSites
}

// resolveLanguageCalls resolves one admitted language's call sites. Every lookup
// table it builds is filtered to that language, which is what stops a traversal
// admitted for one language from reaching into another's nodes: a C# namespace
// node is not a candidate target for a Go call, and a polyglot workspace's Go
// answer must contain no C# node at all rather than a plausible-looking one.
func resolveLanguageCalls(ctx context.Context, tx *sql.Tx, lang string) (callResolution, error) {
	var r callResolution
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM topology_call_sites WHERE language = ? AND site_kind = ?`,
		lang, string(CallSiteCall)).Scan(&r.callSites); err != nil {
		return r, fmt.Errorf("topology: resolve calls: count sites: %w", err)
	}
	tables, err := loadResolverTables(ctx, tx, lang)
	if err != nil {
		return r, err
	}
	edges, err := collectResolvedEdges(ctx, tx, lang, tables, &r)
	if err != nil {
		return r, err
	}
	if err := insertResolvedEdges(tx, edges); err != nil {
		return r, err
	}
	return r, nil
}

// resolverTables are the three language-filtered lookups a resolution pass needs.
// Filtering every one of them to the admitted language is what stops a
// traversal admitted for one language from reaching into another's nodes: a C#
// namespace node is not a candidate target for a Go call, and a polyglot
// workspace's Go answer must contain no non-Go node at all rather than a
// plausible-looking one.
type resolverTables struct {
	pkgDirs map[string][]int64
	imports map[int64]map[string]string
	targets map[string]map[string][]int64
}

func loadResolverTables(ctx context.Context, tx *sql.Tx, lang string) (resolverTables, error) {
	var t resolverTables
	var err error
	if t.pkgDirs, err = packageDirsForLanguage(ctx, tx, lang); err != nil {
		return t, err
	}
	if t.imports, err = importsByFile(ctx, tx, lang); err != nil {
		return t, err
	}
	t.targets, err = targetsByDir(ctx, tx, lang)
	return t, err
}

// resolvedEdge is one cross-file call edge to be written.
type resolvedEdge struct {
	from, to int64
}

func collectResolvedEdges(ctx context.Context, tx *sql.Tx, lang string, t resolverTables, r *callResolution) ([]resolvedEdge, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT cs.file_id, IFNULL(cs.enclosing_id, 0), cs.callee, cs.qualifier, f.path
           FROM topology_call_sites cs
           JOIN topology_files f ON f.id = cs.file_id
          WHERE cs.language = ? AND cs.site_kind = ? AND cs.qualifier IS NOT NULL`,
		lang, string(CallSiteCall))
	if err != nil {
		return nil, fmt.Errorf("topology: resolve calls: scan sites: %w", err)
	}
	defer rows.Close()
	seen := map[[2]int64]bool{}
	var edges []resolvedEdge
	for rows.Next() {
		var fileID, enclosing int64
		var callee, qualifier, callerPath string
		if err := rows.Scan(&fileID, &enclosing, &callee, &qualifier, &callerPath); err != nil {
			return nil, fmt.Errorf("topology: resolve calls: scan site: %w", err)
		}
		r.qualifiedSites++
		to, bucket := resolveOne(t.imports[fileID], qualifier, callee, t.pkgDirs, t.targets)
		key := [2]int64{enclosing, to}
		if bucket == bucketResolved {
			switch {
			// A site with no caller node to hang an edge on, or one whose target
			// is its own declaration, has no edge to write. Writing one anyway
			// would put from_id = 0 into topology_edges, which violates the FK
			// under `PRAGMA foreign_keys = ON` and aborts the whole resolve
			// transaction — one such site would cost the workspace its entire
			// cross-file call graph, not just its own edge.
			case enclosing == 0 || enclosing == to:
				bucket = bucketNoCaller
			// The same caller calling the same target twice is one edge, not two:
			// a duplicated row double-counts that neighbour in every consumer that
			// weighs edges.
			case seen[key]:
				bucket = bucketRepeat
			}
		}
		// Exactly one bucket per qualified site, on every path — that is what
		// makes the buckets sum to qualifiedSites rather than nearly sum to it.
		r.countBucket(bucket)
		if bucket != bucketResolved {
			continue
		}
		seen[key] = true
		edges = append(edges, resolvedEdge{from: enclosing, to: to})
		if !isGoTestPath(callerPath) {
			r.resolvedNonTest++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("topology: resolve calls: rows: %w", err)
	}
	return edges, nil
}

func (r *callResolution) countBucket(b resolveBucket) {
	switch b {
	case bucketResolved:
		r.resolved++
	case bucketReceiver:
		r.unresolvedRecv++
	case bucketExternal:
		r.externalPackage++
	case bucketUnmatched:
		r.unmatchedTarget++
	case bucketRepeat:
		r.repeatOfEdge++
	case bucketNoCaller:
		r.noCallerNode++
	}
}

func insertResolvedEdges(tx *sql.Tx, edges []resolvedEdge) error {
	if len(edges) == 0 {
		return nil
	}
	stmt, err := tx.Prepare(
		`INSERT INTO topology_edges(from_id, to_id, kind, confidence, source)
         VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("topology: resolve calls: prepare: %w", err)
	}
	defer stmt.Close()
	for _, e := range edges {
		if _, err := stmt.Exec(e.from, e.to, string(EdgeCalls), callEdgeConfidence, callResolverSource); err != nil {
			return fmt.Errorf("topology: resolve calls: insert: %w", err)
		}
	}
	return nil
}

type resolveBucket int

const (
	bucketResolved resolveBucket = iota
	// bucketReceiver: the qualifier is not an import of the calling file, so it
	// is a receiver variable or a package-level value — a method call.
	bucketReceiver
	// bucketExternal: the qualifier IS an import, but the import path names no
	// indexed directory (standard library, third-party module).
	bucketExternal
	// bucketUnmatched: the import resolves to an indexed directory that declares
	// no exported top-level function of that name — a type conversion
	// (`time.Duration(n)`), a package-level variable, or a method on one.
	bucketUnmatched
	// bucketRepeat: the target resolved, but this caller→target edge was already
	// emitted by an earlier site. Not a resolution failure; a second site for one
	// edge.
	bucketRepeat
	// bucketNoCaller: the target resolved, but the site has no enclosing
	// declaration to be the edge's tail (or the tail would be the target itself).
	// A fact about the caller, not about the target.
	bucketNoCaller
)

// resolveOne maps one qualified call site to a target node id.
//
// The exported-name requirement is a precision guard, not decoration: a local
// variable can shadow an import name (`func f(topology *T) { topology.walk() }`
// in a file that also imports .../topology), and without it that call would
// resolve to the package's unexported walk. An unexported callee behind a
// qualifier is never a package-qualified call in valid Go, so treating it as a
// receiver call is the correct reading, not a fallback.
func resolveOne(fileImports map[string]string, qualifier, callee string,
	pkgDirs map[string][]int64, targets map[string]map[string][]int64,
) (int64, resolveBucket) {
	importPath, ok := fileImports[qualifier]
	if !ok || !isExportedName(callee) {
		return 0, bucketReceiver
	}
	dir, ok := matchImportDir(importPath, pkgDirs)
	if !ok {
		return 0, bucketExternal
	}
	ids := targets[dir][callee]
	if len(ids) == 0 {
		return 0, bucketUnmatched
	}
	// A directory can hold two packages (`foo` and its external `foo_test`), so a
	// name can appear twice in one directory. targetsByDir orders by rowid, which
	// makes the choice stable across passes rather than map-iteration random.
	return ids[0], bucketResolved
}

func isExportedName(name string) bool {
	for _, r := range name {
		return unicode.IsUpper(r)
	}
	return false
}

// isGoTestPath reports whether a caller lives in a Go test file. Test callers
// ARE resolved and ARE counted in the edge total — a test calling the function
// it exercises is the single most useful cross-file call edge there is, and
// dropping it would gut the consumer this exists for. The split is published
// instead, so a consumer that must not count test callers (import-cycle
// reasoning, for one) can filter on the caller's path rather than discovering
// after the fact that the numbers included them.
func isGoTestPath(p string) bool {
	return len(p) > len("_test.go") && p[len(p)-len("_test.go"):] == "_test.go"
}

func hasPackageNodeTx(ctx context.Context, tx *sql.Tx, lang string) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM topology_nodes WHERE kind = ? AND language = ? LIMIT 1`,
		string(KindPackage), lang).Scan(&one)
	switch {
	case err == sql.ErrNoRows:
		return false, nil
	case err != nil:
		return false, fmt.Errorf("topology: resolve calls: admission: %w", err)
	}
	return true, nil
}

// packageDirsForLanguage groups package-node ids by directory, restricted to one
// language. matchImportDir needs the same directory keys linkImports uses, but a
// resolver admitted for one language must never match a directory whose only
// package node belongs to another.
func packageDirsForLanguage(ctx context.Context, tx *sql.Tx, lang string) (map[string][]int64, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT n.id, f.path FROM topology_nodes n
           JOIN topology_files f ON f.id = n.file_id
          WHERE n.kind = ? AND n.language = ?`, string(KindPackage), lang)
	if err != nil {
		return nil, fmt.Errorf("topology: resolve calls: packages: %w", err)
	}
	defer rows.Close()
	out := map[string][]int64{}
	for rows.Next() {
		var id int64
		var p string
		if err := rows.Scan(&id, &p); err != nil {
			return nil, fmt.Errorf("topology: resolve calls: package scan: %w", err)
		}
		out[path.Dir(p)] = append(out[path.Dir(p)], id)
	}
	return out, rows.Err()
}

// importsByFile maps each file to its own import set: local name → import path.
// The local name is the extractor's import node Name, which already accounts for
// an explicit alias.
//
// Known limitation, measured and left alone: for an UNALIASED import the Go
// extractor derives the local name from the import path's last element, and Go
// does not require a package's name to match its directory (`internal/utils`
// declaring `package util`). A call qualified by such a package misses
// fileImports and is bucketed — and labelled — as a method call on a receiver,
// which it is not. It is a missing-edge and mis-label class, never a false edge.
// plumb's own tree has zero occurrences (two directories mismatch, `cmd/plumb`
// and an external test package, and neither is reachable as a qualified call),
// so this is documented rather than chased. Explicit aliases resolve correctly.
func importsByFile(ctx context.Context, tx *sql.Tx, lang string) (map[int64]map[string]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT n.file_id, n.name, n.qualified FROM topology_nodes n
          WHERE n.kind = ? AND n.language = ? AND n.qualified <> '' AND n.name <> ''`,
		string(KindImport), lang)
	if err != nil {
		return nil, fmt.Errorf("topology: resolve calls: imports: %w", err)
	}
	defer rows.Close()
	out := map[int64]map[string]string{}
	for rows.Next() {
		var fileID int64
		var name, qualified string
		if err := rows.Scan(&fileID, &name, &qualified); err != nil {
			return nil, fmt.Errorf("topology: resolve calls: import scan: %w", err)
		}
		if out[fileID] == nil {
			out[fileID] = map[string]string{}
		}
		out[fileID][name] = qualified
	}
	return out, rows.Err()
}

// targetsByDir indexes callable targets by directory and name. Only top-level
// functions are candidates: a package-qualified call cannot name a method, and
// including methods would let `pkg.Do()` match an unrelated `(*T).Do` that
// happens to live in the target package.
func targetsByDir(ctx context.Context, tx *sql.Tx, lang string) (map[string]map[string][]int64, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT n.id, n.name, f.path FROM topology_nodes n
           JOIN topology_files f ON f.id = n.file_id
          WHERE n.kind = ? AND n.language = ?
          ORDER BY n.id`, string(KindFunction), lang)
	if err != nil {
		return nil, fmt.Errorf("topology: resolve calls: targets: %w", err)
	}
	defer rows.Close()
	out := map[string]map[string][]int64{}
	for rows.Next() {
		var id int64
		var name, p string
		if err := rows.Scan(&id, &name, &p); err != nil {
			return nil, fmt.Errorf("topology: resolve calls: target scan: %w", err)
		}
		dir := path.Dir(p)
		if out[dir] == nil {
			out[dir] = map[string][]int64{}
		}
		out[dir][name] = append(out[dir][name], id)
	}
	return out, rows.Err()
}

func writeCallMeta(tx *sql.Tx, r callResolution) error {
	pairs := [][2]any{
		{metaCallResolved, r.resolved},
		{metaCallResolvedNonTest, r.resolvedNonTest},
		{metaCallUnresolvedRecv, r.unresolvedRecv},
		{metaCallExternal, r.externalPackage},
		{metaCallUnmatched, r.unmatchedTarget},
		{metaCallRepeatOfEdge, r.repeatOfEdge},
		{metaCallNoCallerNode, r.noCallerNode},
		{metaCallSites, r.callSites},
		{metaCallQualifiedSites, r.qualifiedSites},
	}
	for _, p := range pairs {
		if _, err := tx.Exec(
			`INSERT INTO topology_meta(key, value) VALUES (?, ?)
             ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
			p[0], fmt.Sprint(p[1])); err != nil {
			return fmt.Errorf("topology: resolve calls: meta: %w", err)
		}
	}
	return nil
}
