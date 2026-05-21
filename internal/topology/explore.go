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
	opts = clampOpts(opts)
	centre, err := resolveNode(db, name)
	if err != nil {
		return nil, err
	}
	return bfs(ctx, db, centre, opts)
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
func bfs(ctx context.Context, db *sql.DB, centre Node, opts ExploreOpts) (*Neighbourhood, error) {
	nb := &Neighbourhood{Centre: centre}
	visited := map[int64]bool{centre.ID: true}
	inOutput := map[int64]bool{centre.ID: true}
	seenEdges := map[int64]bool{}
	queue := []int64{centre.ID}
	byteEst := estimateBytes(centre)

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
			byteEst += estimateBytes(n)
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

func estimateBytes(n Node) int {
	return len(n.Name) + len(n.Qualified) + len(n.Signature) + len(n.Path) + 50
}
