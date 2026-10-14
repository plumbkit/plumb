package topology

import (
	"context"
	"database/sql"
	"fmt"
	"path"
	"slices"
	"strings"
)

// callsites_query.go — a read-only view of the raw call sites for a consumer that
// reasons about the calls the resolver leaves out.
//
// The call resolver follows package-qualified calls only (indexer_calls.go): a bare
// call to a function of the caller's own package, and a call on a receiver, never
// become an edge. Their sites are still recorded verbatim, with the declaration
// that holds them, so the only thing missing is a way to read them. This is that
// way. It resolves nothing and writes nothing: no edge, node or resolver outcome
// changes, and no other reader of the table is affected.

// CallSiteRef is one recorded call expression as the index holds it, before any
// resolution: what was called, the text to its left, where, and the declaration it
// sits in.
type CallSiteRef struct {
	Path    string // root-relative, slash-separated file
	Package string // the package clause the file declares; "" when none was recorded
	Callee  string
	// Qualifier is the text to the left of the final dot, "" for a bare call. A call
	// whose receiver is not a plain name chain (`f().M()`) also records "".
	Qualifier   string
	StartByte   int
	StartLine   int
	EnclosingID int64 // the declaration node holding the site; 0 when there is none
}

// callSiteNames bounds the callee names one query carries, under any SQL variable
// limit.
const callSiteNames = 64

// CallSitesInDir returns the recorded call expressions (not composite-literal field
// values, not decorators) of the language's files that sit directly in dir ("." for
// the workspace root), ordered by file then position. callees narrows them to calls
// of those names, which is what keeps the read small: a large directory records
// tens of thousands of sites and a caller wants a handful of names (nil means all).
// It returns at most limit of them; truncated reports that more exist, so a consumer
// never mistakes a cut list for the whole package.
func (s *Store) CallSitesInDir(ctx context.Context, dir, language string, callees []string, limit int) (sites []CallSiteRef, truncated bool, err error) {
	return callSitesInDir(ctx, s.db, dir, language, callees, limit)
}

// DeclsNamedInDir returns the function and method declarations called name in the
// language's files that sit directly in dir, ordered by path then line.
func (s *Store) DeclsNamedInDir(ctx context.Context, dir, language, name string) ([]Node, error) {
	return declsNamedInDir(ctx, s.db, dir, language, name)
}

// dirPathRange is the half-open range of paths under dir ("/" is 0x2F, "0" is the
// next byte); a path in a subdirectory is inside it too, so callers still compare
// path.Dir.
func dirPathRange(dir string) (lo, hi string, bounded bool) {
	if dir == "." || dir == "" {
		return "", "", false
	}
	return dir + "/", dir + "0", true
}

func callSitesInDir(ctx context.Context, db *sql.DB, dir, language string, callees []string, limit int) ([]CallSiteRef, bool, error) {
	var out []CallSiteRef
	for names := range nameChunks(callees) {
		got, err := callSitesNamed(ctx, db, dir, language, names)
		if err != nil {
			return nil, false, err
		}
		out = append(out, got...)
	}
	if len(out) == 0 {
		return nil, false, nil
	}
	if err := fillPackages(ctx, db, out); err != nil {
		return nil, false, err
	}
	slices.SortFunc(out, func(a, b CallSiteRef) int {
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		return a.StartByte - b.StartByte
	})
	if limit > 0 && len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

// nameChunks splits callees into queries' worth of names; no names is one chunk of
// none, which selects every callee.
func nameChunks(callees []string) func(yield func([]string) bool) {
	return func(yield func([]string) bool) {
		if len(callees) == 0 {
			yield(nil)
			return
		}
		for rest := callees; len(rest) > 0; {
			n := min(callSiteNames, len(rest))
			if !yield(rest[:n]) {
				return
			}
			rest = rest[n:]
		}
	}
}

// callSitesNamed reads the call sites of one chunk of callees in the files directly
// in dir. The files are chosen by a path range in the same statement, so the read
// is one pass over the directory's sites however many files it holds.
func callSitesNamed(ctx context.Context, db *sql.DB, dir, language string, callees []string) ([]CallSiteRef, error) {
	q := `SELECT f.path, cs.callee, IFNULL(cs.qualifier, ''), cs.start_byte, cs.start_line, IFNULL(cs.enclosing_id, 0)
	        FROM topology_call_sites cs
	        JOIN topology_files f ON f.id = cs.file_id
	       WHERE cs.site_kind = ? AND f.language = ?`
	args := []any{string(CallSiteCall), language}
	if lo, hi, ok := dirPathRange(dir); ok {
		q += ` AND f.path >= ? AND f.path < ?`
		args = append(args, lo, hi)
	}
	if len(callees) > 0 {
		// G202 is a false positive: the only thing concatenated is a "?,?,…" placeholder
		// list, and every value is a bound parameter.
		q += ` AND cs.callee IN (` + marks(len(callees)) + `)` //nolint:gosec // G202: placeholders are bound-parameter markers, not user data.
		for _, c := range callees {
			args = append(args, c)
		}
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("topology: call sites in dir: %w", err)
	}
	defer rows.Close()
	var out []CallSiteRef
	for rows.Next() {
		var s CallSiteRef
		if err := rows.Scan(&s.Path, &s.Callee, &s.Qualifier, &s.StartByte, &s.StartLine, &s.EnclosingID); err != nil {
			return nil, fmt.Errorf("topology: call sites in dir: %w", err)
		}
		if path.Dir(s.Path) == dir {
			out = append(out, s)
		}
	}
	return out, rows.Err()
}

// fillPackages sets each site's package clause from the package nodes of the files
// the sites are in. Reading the package nodes by their kind is one short pass; joining
// them to every file of a directory instead walks every node of every file.
func fillPackages(ctx context.Context, db *sql.DB, sites []CallSiteRef) error {
	want := make(map[string]string, len(sites))
	for _, s := range sites {
		want[s.Path] = ""
	}
	rows, err := db.QueryContext(ctx, `
		SELECT f.path, n.name
		  FROM topology_nodes n
		  JOIN topology_files f ON f.id = n.file_id
		 WHERE n.kind = ?`, string(KindPackage))
	if err != nil {
		return fmt.Errorf("topology: package clauses: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p, name string
		if err := rows.Scan(&p, &name); err != nil {
			return fmt.Errorf("topology: package clauses: %w", err)
		}
		if _, ok := want[p]; ok {
			want[p] = name
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range sites {
		sites[i].Package = want[sites[i].Path]
	}
	return nil
}

// OpenCall is a recorded call of a name that no edge accounts for exactly: the
// resolver tied it to nothing, tied it to a declaration with no caller to hang an
// edge on, or left it to an intra-file pass that cannot tell two declarations of
// the name apart. These are the calls of the name a reader still has to look at.
type OpenCall struct {
	Path      string
	Line      int
	Qualifier string // "" for a bare call
	// Kind says why the call is open: "receiver" (a method call on a value whose
	// type no declaration states), "unmatched" (a package-qualified name that
	// declares no such function), "bare" (a bare name with no function of that
	// name in its package), "local" (a bare name the calling function also
	// declares, which may or may not shadow it where the call is), "same-file"
	// (its file declares the name more than once), "top-level" (resolved, but
	// outside any declaration), "external" (a package-qualified call whose import
	// names no indexed directory: usually another module, but a package the index
	// failed to map would land here too), "stale" (resolved by an earlier pass,
	// but the edge it produced is gone) or "unresolved" (no pass has read it).
	Kind string
	// DotImport is set when the call's file dot-imports a package, so a bare name
	// may be another package's exported function.
	DotImport bool
}

// OpenCallSites returns the recorded calls named name, in the language's files,
// that no edge accounts for exactly, ordered by path then position, at most limit
// of them; total counts them all. A resolved, repeated or same-file call is not
// open while the edge it stands for exists (a same-file call needs its file to
// declare the name once). An external call is returned for the caller to judge
// by its qualifier.
func (s *Store) OpenCallSites(ctx context.Context, name, language string, limit int) (open []OpenCall, total int, err error) {
	dots, err := s.dotImportFiles(ctx)
	if err != nil {
		return nil, 0, err
	}
	// A call counts as accounted for only while the edge it produced still stands:
	// a scoped pass re-reads only the changed files' sites, so a caller elsewhere
	// keeps "resolved" after its target is deleted or renamed, with no edge left. A
	// same-file call of the enclosing declaration's own name is recursion, which no
	// pass draws an edge for and no census counts as a caller.
	rows, err := s.db.QueryContext(ctx, `
		SELECT f.path, cs.file_id, cs.start_line, IFNULL(cs.qualifier, ''), cs.outcome,
		       cs.outcome IN (?, ?, ?) AND (cs.enclosing_id IS NULL OR NOT EXISTS (
		           SELECT 1 FROM topology_edges e JOIN topology_nodes t ON t.id = e.to_id
		            WHERE e.from_id = cs.enclosing_id AND e.kind = ? AND t.name = cs.callee))
		       AND NOT (cs.outcome = ? AND EXISTS (
		           SELECT 1 FROM topology_nodes en WHERE en.id = cs.enclosing_id AND en.name = cs.callee)),
		       cs.outcome = ? AND (SELECT COUNT(*) FROM topology_nodes d
		                            WHERE d.file_id = cs.file_id AND d.name = cs.callee AND d.kind IN (?, ?, ?)) > 1
		  FROM topology_call_sites cs
		  JOIN topology_files f ON f.id = cs.file_id
		 WHERE cs.language = ? AND cs.callee = ? AND cs.site_kind = ?
		   AND (cs.outcome IN (?, ?, ?, ?, ?, -1)
		        OR (cs.outcome = ? AND (SELECT COUNT(*) FROM topology_nodes d
		                                 WHERE d.file_id = cs.file_id AND d.name = cs.callee AND d.kind IN (?, ?, ?)) > 1)
		        OR (cs.outcome IN (?, ?, ?) AND (cs.enclosing_id IS NULL OR NOT EXISTS (
		           SELECT 1 FROM topology_edges e JOIN topology_nodes t ON t.id = e.to_id
		            WHERE e.from_id = cs.enclosing_id AND e.kind = ? AND t.name = cs.callee))
		       AND NOT (cs.outcome = ? AND EXISTS (
		           SELECT 1 FROM topology_nodes en WHERE en.id = cs.enclosing_id AND en.name = cs.callee))))
		 ORDER BY f.path, cs.start_byte`,
		int(bucketResolved), int(bucketRepeat), int(bucketSameFile), string(EdgeCalls), int(bucketSameFile),
		int(bucketSameFile), string(KindFunction), string(KindMethod), string(KindTest),
		language, name, string(CallSiteCall),
		int(bucketReceiver), int(bucketUnmatched), int(bucketBareUnmatched), int(bucketNoCaller), int(bucketExternal),
		int(bucketSameFile), string(KindFunction), string(KindMethod), string(KindTest),
		int(bucketResolved), int(bucketRepeat), int(bucketSameFile), string(EdgeCalls), int(bucketSameFile))
	if err != nil {
		return nil, 0, fmt.Errorf("topology: open call sites: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var c OpenCall
		var outcome int
		var dangling, ambiguous bool
		var fileID int64
		if err := rows.Scan(&c.Path, &fileID, &c.Line, &c.Qualifier, &outcome, &dangling, &ambiguous); err != nil {
			return nil, 0, fmt.Errorf("topology: open call sites: %w", err)
		}
		c.DotImport = dots[fileID]
		c.Kind = openKind(resolveBucket(outcome))
		if dangling && !ambiguous {
			c.Kind = "stale"
		}
		if c.Qualifier == QualifierLocal {
			// The extractor marks a name local for the whole function, not the block
			// that declares it, so the call may still be the package's function.
			c.Kind, c.Qualifier = "local", ""
		}
		total++
		if limit <= 0 || len(open) < limit {
			open = append(open, c)
		}
	}
	return open, total, rows.Err()
}

func openKind(b resolveBucket) string {
	switch b {
	case bucketReceiver:
		return "receiver"
	case bucketUnmatched:
		return "unmatched"
	case bucketBareUnmatched:
		return "bare"
	case bucketSameFile:
		return "same-file"
	case bucketExternal:
		return "external"
	case bucketNoCaller:
		return "top-level"
	}
	return "unresolved"
}

// marks is n comma-separated placeholders.
func marks(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

func declsNamedInDir(ctx context.Context, db *sql.DB, dir, language, name string) ([]Node, error) {
	q := `SELECT ` + nodeColumns + `
	        FROM topology_nodes n
	        JOIN topology_files f ON f.id = n.file_id
	       WHERE n.name = ? AND n.kind IN (?, ?) AND f.language = ?`
	args := []any{name, string(KindFunction), string(KindMethod), language}
	if lo, hi, ok := dirPathRange(dir); ok {
		q += ` AND f.path >= ? AND f.path < ?`
		args = append(args, lo, hi)
	}
	rows, err := db.QueryContext(ctx, q+` ORDER BY f.path, n.start_line`, args...)
	if err != nil {
		return nil, fmt.Errorf("topology: declarations in dir: %w", err)
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, fmt.Errorf("topology: declarations in dir: %w", err)
		}
		if path.Dir(n.Path) == dir {
			out = append(out, n)
		}
	}
	return out, rows.Err()
}

// UnreadFiles counts the indexed files with the extension whose last extraction
// failed (a parse error, a file over the size cap): their calls are not recorded,
// so no census can speak for them.
func (s *Store) UnreadFiles(ctx context.Context, ext string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM topology_files WHERE error_msg != '' AND path LIKE ?`, "%"+ext).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("topology: unread files: %w", err)
	}
	return n, nil
}

// dotImportFiles is the set of files that dot-import a package, read once per
// census. Asked per call site instead, the planner reaches for the import-kind
// index and scans every import node of the workspace for each site: on this
// repository a common method name's census took seconds (PLAN-542).
func (s *Store) dotImportFiles(ctx context.Context) (map[int64]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT file_id FROM topology_nodes WHERE name = '.' AND kind = ?`, string(KindImport))
	if err != nil {
		return nil, fmt.Errorf("topology: dot imports: %w", err)
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("topology: dot imports: %w", err)
		}
		out[id] = true
	}
	return out, rows.Err()
}
