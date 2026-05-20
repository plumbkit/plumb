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

func bfs(ctx context.Context, db *sql.DB, centre Node, opts ExploreOpts) (*Neighbourhood, error) {
	nb := &Neighbourhood{Centre: centre}
	visited := map[int64]bool{centre.ID: true}
	queue := []int64{centre.ID}
	byteEst := estimateBytes(centre)

	for depth := 0; depth < opts.Depth && len(queue) > 0; depth++ {
		next, edges, err := expandFrontier(ctx, db, queue, opts.EdgeKinds)
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
				return nb, nil
			}
			nb.Nodes = append(nb.Nodes, n)
			queue = append(queue, n.ID)
		}
		nb.Edges = append(nb.Edges, edges...)
	}
	return nb, nil
}

func expandFrontier(ctx context.Context, db *sql.DB, ids []int64, edgeKinds []string) ([]Node, []Edge, error) {
	_ = ctx
	if len(ids) == 0 {
		return nil, nil, nil
	}
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	where := fmt.Sprintf(`(from_id IN (%s) OR to_id IN (%s))`, placeholders, placeholders)
	allArgs := append(args, args...)

	if len(edgeKinds) > 0 {
		kindPH := strings.Repeat("?,", len(edgeKinds))
		kindPH = kindPH[:len(kindPH)-1]
		where += fmt.Sprintf(` AND kind IN (%s)`, kindPH)
		for _, k := range edgeKinds {
			allArgs = append(allArgs, k)
		}
	}

	//nolint:gosec // G202: where clause built from integer IDs and constant string literals; no user data interpolated
	rows, err := db.Query(`SELECT id, from_id, to_id, kind, confidence, source FROM topology_edges WHERE `+where, allArgs...)
	if err != nil {
		return nil, nil, fmt.Errorf("topology: edge query: %w", err)
	}
	return collectNeighbours(db, rows, ids)
}

func collectNeighbours(db *sql.DB, rows *sql.Rows, frontier []int64) ([]Node, []Edge, error) {
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
		if !frontierSet[e.ToID] {
			neighbourIDs[e.ToID] = true
		}
		if !frontierSet[e.FromID] {
			neighbourIDs[e.FromID] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var nodes []Node
	for id := range neighbourIDs {
		if n, err := nodeByID(db, id); err == nil {
			nodes = append(nodes, n)
		}
	}
	return nodes, edges, nil
}

func estimateBytes(n Node) int {
	return len(n.Name) + len(n.Qualified) + len(n.Signature) + len(n.Path) + 50
}
