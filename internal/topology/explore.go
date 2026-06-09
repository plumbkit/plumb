package topology

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

const (
	defaultDepth    = 2
	defaultMaxNodes = 50
	defaultMaxBytes = 30000
	hardCapDepth    = 4
	hardCapNodes    = 200
	hardCapBytes    = 100000
)

// Explore performs a bounded BFS from the named symbol and returns its neighbourhood.
func Explore(ctx context.Context, db *sql.DB, name string, opts ExploreOpts) (*Neighbourhood, error) {
	centre, err := resolveNode(db, name)
	if err != nil {
		return nil, err
	}
	return ExploreFrom(ctx, db, centre, opts)
}

// ExploreFrom performs the bounded BFS from an already-resolved centre node.
// Callers that disambiguate an ambiguous symbol name themselves (via
// ResolveNodes) use this so the traversal is guaranteed to start from the
// intended node rather than an arbitrary first match.
func ExploreFrom(ctx context.Context, db *sql.DB, centre Node, opts ExploreOpts) (*Neighbourhood, error) {
	return bfs(ctx, db, centre, clampOpts(opts))
}

func clampOpts(opts ExploreOpts) ExploreOpts {
	if opts.Depth <= 0 {
		opts.Depth = defaultDepth
	}
	if opts.Depth > hardCapDepth {
		opts.Depth = hardCapDepth
	}
	if opts.MaxNodes <= 0 {
		opts.MaxNodes = defaultMaxNodes
	}
	if opts.MaxNodes > hardCapNodes {
		opts.MaxNodes = hardCapNodes
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = defaultMaxBytes
	}
	if opts.MaxBytes > hardCapBytes {
		opts.MaxBytes = hardCapBytes
	}
	return opts
}

// NodeHint narrows an ambiguous symbol-name resolution to a specific node.
// Both fields are optional; an empty hint matches every candidate.
type NodeHint struct {
	PathSubstr string // case-insensitive substring of the node's file path
	Kind       string // exact NodeKind match (e.g. "function", "method")
}

func (h NodeHint) empty() bool { return h.PathSubstr == "" && h.Kind == "" }

func (h NodeHint) matches(n Node) bool {
	if h.Kind != "" && string(n.Kind) != h.Kind {
		return false
	}
	if h.PathSubstr != "" && !strings.Contains(strings.ToLower(n.Path), strings.ToLower(h.PathSubstr)) {
		return false
	}
	return true
}

// ResolveNodes returns every indexed node whose name or qualified name equals
// name, ordered deterministically by path then start line. When hint is
// non-empty and matches at least one candidate the result is restricted to the
// matching nodes; a hint that matches nothing is ignored, so a stale hint never
// turns a real symbol into a miss. The first element is the best pick for a
// traversal start; any remaining elements are genuine same-name alternatives a
// caller can surface for disambiguation.
func ResolveNodes(ctx context.Context, db *sql.DB, name string, hint NodeHint) ([]Node, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT n.id, n.file_id, n.kind, n.name, n.qualified, n.signature,
                n.start_line, n.end_line, n.docstring, n.language, f.path
         FROM topology_nodes n
         JOIN topology_files f ON f.id = n.file_id
         WHERE n.name = ? OR n.qualified = ?
         ORDER BY f.path, n.start_line`, name, name)
	if err != nil {
		return nil, fmt.Errorf("topology: resolve nodes: %w", err)
	}
	defer rows.Close()
	var all []Node
	for rows.Next() {
		var n Node
		if scanErr := rows.Scan(&n.ID, &n.FileID, &n.Kind, &n.Name, &n.Qualified, &n.Signature,
			&n.StartLine, &n.EndLine, &n.Docstring, &n.Language, &n.Path); scanErr != nil {
			continue
		}
		all = append(all, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("topology: resolve nodes: %w", err)
	}
	if hint.empty() {
		return all, nil
	}
	matched := make([]Node, 0, len(all))
	for _, n := range all {
		if hint.matches(n) {
			matched = append(matched, n)
		}
	}
	if len(matched) > 0 {
		return matched, nil
	}
	return all, nil
}

func resolveNode(db *sql.DB, name string) (Node, error) {
	var n Node
	row := db.QueryRow(
		`SELECT n.id, n.file_id, n.kind, n.name, n.qualified, n.signature,
                n.start_line, n.end_line, n.docstring, n.language, f.path
         FROM topology_nodes n
         JOIN topology_files f ON f.id = n.file_id
         WHERE n.name = ? OR n.qualified = ?
         LIMIT 1`, name, name)
	if err := row.Scan(&n.ID, &n.FileID, &n.Kind, &n.Name, &n.Qualified, &n.Signature,
		&n.StartLine, &n.EndLine, &n.Docstring, &n.Language, &n.Path); err == sql.ErrNoRows {
		return n, fmt.Errorf("topology: symbol %q not found in index", name)
	} else if err != nil {
		return n, fmt.Errorf("topology: resolve node: %w", err)
	}
	return n, nil
}

// bfs performs a bounded BFS from centre.
// MaxNodes is the cap on nb.Nodes (neighbours only); the centre node itself is
// not counted against the budget, so the total output is centre + up to MaxNodes
// neighbours.
//
// Budgeting is on AST boundaries: each whole symbol is costed by estimateBytes
// for the current source mode, and a neighbour is added only if it fits within
// MaxBytes in full — a symbol is never split, so the truncated result is always
// a set of whole, coherent symbols.
func bfs(ctx context.Context, db *sql.DB, centre Node, opts ExploreOpts) (*Neighbourhood, error) {
	nb := &Neighbourhood{Centre: centre}
	visited := map[int64]bool{centre.ID: true}
	inOutput := map[int64]bool{centre.ID: true}
	seenEdges := map[int64]bool{}
	queue := []int64{centre.ID}
	byteEst := estimateBytes(centre, opts.IncludeSource)

	for depth := 0; depth < opts.Depth && len(queue) > 0; depth++ {
		next, edges, err := expandFrontier(ctx, db, queue, opts.EdgeKinds, opts.Direction)
		if err != nil {
			return nil, err
		}
		queue = nil
		for _, n := range next {
			if visited[n.ID] {
				continue
			}
			visited[n.ID] = true
			byteEst += estimateBytes(n, opts.IncludeSource)
			if len(nb.Nodes)+1 > opts.MaxNodes || byteEst > opts.MaxBytes {
				nb.Truncated = true
				break
			}
			nb.Nodes = append(nb.Nodes, n)
			inOutput[n.ID] = true
			queue = append(queue, n.ID)
		}
		// Only include edges whose both endpoints are in the output set (fixes dangling
		// edges on truncation). Deduplication via seenEdges (fixes multi-depth duplicates).
		nb.Edges = append(nb.Edges, filterEdges(edges, inOutput, seenEdges)...)
		if nb.Truncated {
			return nb, nil
		}
	}
	return nb, nil
}

// filterEdges returns edges whose both endpoints are in inOutput and that have
// not already been emitted (edge ID deduplication across BFS depths).
func filterEdges(edges []Edge, inOutput map[int64]bool, seen map[int64]bool) []Edge {
	var result []Edge
	for _, e := range edges {
		if seen[e.ID] {
			continue
		}
		if inOutput[e.FromID] && inOutput[e.ToID] {
			seen[e.ID] = true
			result = append(result, e)
		}
	}
	return result
}

func expandFrontier(ctx context.Context, db *sql.DB, ids []int64, edgeKinds []string, dir Direction) ([]Node, []Edge, error) {
	if len(ids) == 0 {
		return nil, nil, nil
	}
	ph := strings.Repeat("?,", len(ids))
	ph = ph[:len(ph)-1]
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}

	var where string
	var allArgs []any
	switch dir {
	case DirectionOutward:
		where = fmt.Sprintf(`from_id IN (%s)`, ph)
		allArgs = args
	case DirectionInward:
		where = fmt.Sprintf(`to_id IN (%s)`, ph)
		allArgs = args
	default:
		where = fmt.Sprintf(`(from_id IN (%s) OR to_id IN (%s))`, ph, ph)
		allArgs = append(args, args...)
	}

	if len(edgeKinds) > 0 {
		kindPH := strings.Repeat("?,", len(edgeKinds))
		kindPH = kindPH[:len(kindPH)-1]
		where += fmt.Sprintf(` AND kind IN (%s)`, kindPH)
		for _, k := range edgeKinds {
			allArgs = append(allArgs, k)
		}
	}

	//nolint:gosec // G202: where clause built from integer IDs and constant string literals; no user data interpolated
	rows, err := db.QueryContext(ctx, `SELECT id, from_id, to_id, kind, confidence, source FROM topology_edges WHERE `+where, allArgs...)
	if err != nil {
		return nil, nil, fmt.Errorf("topology: edge query: %w", err)
	}
	return collectNeighbours(ctx, db, rows, ids, dir)
}

func collectNeighbours(ctx context.Context, db *sql.DB, rows *sql.Rows, frontier []int64, dir Direction) ([]Node, []Edge, error) {
	frontierSet := make(map[int64]bool, len(frontier))
	for _, id := range frontier {
		frontierSet[id] = true
	}
	defer rows.Close()
	var edges []Edge
	neighbourIDs := map[int64]bool{}
	for rows.Next() {
		var e Edge
		if err := rows.Scan(&e.ID, &e.FromID, &e.ToID, &e.Kind, &e.Confidence, &e.Source); err != nil {
			continue
		}
		edges = append(edges, e)
		switch dir {
		case DirectionOutward:
			neighbourIDs[e.ToID] = true
		case DirectionInward:
			neighbourIDs[e.FromID] = true
		default:
			if !frontierSet[e.ToID] {
				neighbourIDs[e.ToID] = true
			}
			if !frontierSet[e.FromID] {
				neighbourIDs[e.FromID] = true
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	nodes, err := nodesByIDs(ctx, db, neighbourIDs)
	return nodes, edges, err
}

// nodesByIDs fetches multiple nodes in a single query, avoiding N+1 roundtrips.
func nodesByIDs(ctx context.Context, db *sql.DB, ids map[int64]bool) ([]Node, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	idList := make([]int64, 0, len(ids))
	for id := range ids {
		idList = append(idList, id)
	}
	ph := strings.Repeat("?,", len(idList))
	ph = ph[:len(ph)-1]
	args := make([]any, len(idList))
	for i, id := range idList {
		args[i] = id
	}
	//nolint:gosec // G202: placeholders built from integer IDs only; no user data interpolated
	rows, err := db.QueryContext(ctx,
		`SELECT n.id, n.file_id, n.kind, n.name, n.qualified, n.signature,
                n.start_line, n.end_line, n.docstring, n.language, f.path
         FROM topology_nodes n
         JOIN topology_files f ON f.id = n.file_id
         WHERE n.id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("topology: batch node fetch: %w", err)
	}
	defer rows.Close()
	var nodes []Node
	for rows.Next() {
		var n Node
		if err := rows.Scan(&n.ID, &n.FileID, &n.Kind, &n.Name, &n.Qualified, &n.Signature,
			&n.StartLine, &n.EndLine, &n.Docstring, &n.Language, &n.Path); err != nil {
			continue
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

// estimateBytes approximates a node's contribution to the rendered output under
// the given source mode, so MaxBytes bounds what is actually returned. The
// estimate is per whole symbol: the signature is counted only when it will be
// shown (every mode except "none"), and the docstring only for the richer
// "snippets"/"full" modes. Because a node is always added whole, costing it this
// way keeps truncation on symbol boundaries.
func estimateBytes(n Node, includeSource string) int {
	b := len(n.Name) + len(n.Qualified) + len(n.Path) + 50
	if includeSource != "none" {
		b += len(n.Signature)
	}
	if includeSource == "snippets" || includeSource == "full" {
		b += len(n.Docstring)
	}
	return b
}
