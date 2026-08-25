package topology

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
)

// FunctionGraph is the admitted callable subgraph used by function-level
// reachability. Nodes are stable topology declarations; Edges follow calls
// from caller to callee. It deliberately contains only indexed Go callables.
type FunctionGraph struct {
	Nodes    map[int64]Node
	Edges    map[int64][]int64
	MainDirs map[string]bool
}

// LoadFunctionGraph loads the complete callable graph for one admitted language.
// productionOnly excludes callers in *_test.go files, matching the
// production-only reachability contract. The graph is intentionally full and
// uncapped: reachability cannot use the neighbourhood BFS depth cap without
// turning deep, reachable functions into false "not reached" results.
func LoadFunctionGraph(ctx context.Context, db *sql.DB, language string, productionOnly bool) (*FunctionGraph, error) {
	if strings.TrimSpace(language) == "" {
		return nil, errors.New("topology: function graph language is empty")
	}
	nodes, err := loadFunctionNodes(ctx, db, language, productionOnly)
	if err != nil {
		return nil, err
	}
	mainDirs, err := loadFunctionMainDirs(ctx, db, language)
	if err != nil {
		return nil, err
	}
	edges, err := loadFunctionEdges(ctx, db, language, productionOnly, nodes)
	if err != nil {
		return nil, err
	}
	return &FunctionGraph{Nodes: nodes, Edges: edges, MainDirs: mainDirs}, nil
}

func loadFunctionNodes(ctx context.Context, db *sql.DB, language string, productionOnly bool) (map[int64]Node, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT `+nodeColumns+`
		  FROM topology_nodes n
		  JOIN topology_files f ON f.id = n.file_id
		 WHERE n.language = ?
		   AND n.kind IN (?, ?)
		   AND (? = 0 OR f.path NOT LIKE '%_test.go')
		 ORDER BY f.path, n.start_line, n.id`,
		language, string(KindFunction), string(KindMethod), boolArg(productionOnly))
	if err != nil {
		return nil, fmt.Errorf("topology: load function nodes: %w", err)
	}
	nodes := map[int64]Node{}
	for rows.Next() {
		n, scanErr := scanNode(rows)
		if scanErr != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("topology: load function node: %w", scanErr)
		}
		nodes[n.ID] = n
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("topology: load function nodes: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("topology: load function nodes: close: %w", err)
	}
	return nodes, nil
}

func loadFunctionMainDirs(ctx context.Context, db *sql.DB, language string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT f.path
		  FROM topology_nodes n
		  JOIN topology_files f ON f.id = n.file_id
		 WHERE n.kind = ? AND n.name = 'main' AND n.language = ?`,
		string(KindPackage), language)
	if err != nil {
		return nil, fmt.Errorf("topology: load function main dirs: %w", err)
	}
	mainDirs := map[string]bool{}
	for rows.Next() {
		var filePath string
		if err := rows.Scan(&filePath); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("topology: load function main dir: %w", err)
		}
		mainDirs[path.Dir(filePath)] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("topology: load function main dirs: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("topology: load function main dirs: close: %w", err)
	}
	return mainDirs, nil
}

func loadFunctionEdges(ctx context.Context, db *sql.DB, language string, productionOnly bool, nodes map[int64]Node) (map[int64][]int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT e.from_id, e.to_id
		  FROM topology_edges e
		  JOIN topology_nodes a ON a.id = e.from_id
		  JOIN topology_files af ON af.id = a.file_id
		  JOIN topology_nodes b ON b.id = e.to_id
		  JOIN topology_files bf ON bf.id = b.file_id
		 WHERE e.kind = ?
		   AND a.language = ? AND b.language = ?
		   AND a.kind IN (?, ?) AND b.kind IN (?, ?)
		   AND (? = 0 OR af.path NOT LIKE '%_test.go')
		 ORDER BY e.from_id, e.to_id`,
		string(EdgeCalls), language, language,
		string(KindFunction), string(KindMethod),
		string(KindFunction), string(KindMethod),
		boolArg(productionOnly))
	if err != nil {
		return nil, fmt.Errorf("topology: load function edges: %w", err)
	}
	edges := map[int64][]int64{}
	for rows.Next() {
		var from, to int64
		if err := rows.Scan(&from, &to); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("topology: load function edge: %w", err)
		}
		if _, ok := nodes[from]; !ok {
			continue
		}
		if _, ok := nodes[to]; !ok {
			continue
		}
		edges[from] = append(edges[from], to)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("topology: load function edges: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("topology: load function edges: close: %w", err)
	}
	return edges, nil
}

func boolArg(v bool) int {
	if v {
		return 1
	}
	return 0
}

// FunctionReachabilityResult is the full outward closure from stable root node
// IDs. Predecessor records the shortest deterministic root-to-node tree.
type FunctionReachabilityResult struct {
	Roots       []int64
	Reachable   map[int64]bool
	Predecessor map[int64]int64
}

// ReachableFunctions performs a full, deterministic BFS over call edges.
func ReachableFunctions(g *FunctionGraph, roots []int64) *FunctionReachabilityResult {
	res := &FunctionReachabilityResult{
		Reachable:   map[int64]bool{},
		Predecessor: map[int64]int64{},
	}
	var queue []int64
	for _, root := range roots {
		if _, ok := g.Nodes[root]; !ok || res.Reachable[root] {
			continue
		}
		res.Reachable[root] = true
		res.Roots = append(res.Roots, root)
		queue = append(queue, root)
	}
	sort.Slice(res.Roots, func(i, j int) bool { return functionNodeLess(g.Nodes[res.Roots[i]], g.Nodes[res.Roots[j]]) })
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		neighbours := append([]int64(nil), g.Edges[cur]...)
		sort.Slice(neighbours, func(i, j int) bool {
			return functionNodeLess(g.Nodes[neighbours[i]], g.Nodes[neighbours[j]])
		})
		for _, next := range neighbours {
			if res.Reachable[next] {
				continue
			}
			res.Reachable[next] = true
			res.Predecessor[next] = cur
			queue = append(queue, next)
		}
	}
	return res
}

// FunctionPathTo reconstructs one shortest root-to-target chain.
func FunctionPathTo(g *FunctionGraph, res *FunctionReachabilityResult, target int64) ([]Node, bool) {
	if res == nil || !res.Reachable[target] {
		return nil, false
	}
	var chain []Node
	cur := target
	for {
		chain = append([]Node{g.Nodes[cur]}, chain...)
		prev, ok := res.Predecessor[cur]
		if !ok {
			break
		}
		cur = prev
	}
	return chain, true
}

type FunctionSCC struct {
	Nodes []Node
	Cycle bool
	Layer int
}

type functionTarjan struct {
	index   map[int64]int
	low     map[int64]int
	onStack map[int64]bool
	stack   []int64
	next    int
	comps   [][]int64
}

func CondenseFunctionSCCs(g *FunctionGraph, scope map[int64]bool) []FunctionSCC {
	st := &functionTarjan{index: map[int64]int{}, low: map[int64]int{}, onStack: map[int64]bool{}}
	ids := scopedFunctionIDs(g, scope)
	for _, id := range ids {
		if _, seen := st.index[id]; !seen {
			st.connect(g, scope, id)
		}
	}
	compOf := functionComponentIndex(st.comps)
	adj, indeg := functionComponentGraph(g, st.comps, compOf)
	layers := functionComponentLayers(adj, indeg)
	return functionSCCResults(g, st.comps, layers)
}

func scopedFunctionIDs(g *FunctionGraph, scope map[int64]bool) []int64 {
	ids := make([]int64, 0, len(scope))
	for id := range scope {
		if _, ok := g.Nodes[id]; ok {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return functionNodeLess(g.Nodes[ids[i]], g.Nodes[ids[j]]) })
	return ids
}

func functionComponentIndex(comps [][]int64) map[int64]int {
	compOf := map[int64]int{}
	for i, comp := range comps {
		for _, id := range comp {
			compOf[id] = i
		}
	}
	return compOf
}

func functionComponentGraph(g *FunctionGraph, comps [][]int64, compOf map[int64]int) ([]map[int]bool, []int) {
	adj := make([]map[int]bool, len(comps))
	indeg := make([]int, len(comps))
	for i := range adj {
		adj[i] = map[int]bool{}
	}
	for from, tos := range g.Edges {
		cf, ok := compOf[from]
		if !ok {
			continue
		}
		for _, to := range tos {
			ct, ok := compOf[to]
			if !ok || cf == ct || adj[cf][ct] {
				continue
			}
			adj[cf][ct] = true
			indeg[ct]++
		}
	}
	return adj, indeg
}

func functionComponentLayers(adj []map[int]bool, indeg []int) []int {
	layers := make([]int, len(adj))
	queue := make([]int, 0, len(adj))
	for i, d := range indeg {
		if d == 0 {
			queue = append(queue, i)
		}
	}
	sort.Ints(queue)
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		nexts := make([]int, 0, len(adj[c]))
		for next := range adj[c] {
			nexts = append(nexts, next)
		}
		sort.Ints(nexts)
		for _, next := range nexts {
			if layers[c]+1 > layers[next] {
				layers[next] = layers[c] + 1
			}
			indeg[next]--
			if indeg[next] == 0 {
				i := sort.SearchInts(queue, next)
				queue = append(queue, 0)
				copy(queue[i+1:], queue[i:])
				queue[i] = next
			}
		}
	}
	return layers
}

func functionSCCResults(g *FunctionGraph, comps [][]int64, layers []int) []FunctionSCC {
	out := make([]FunctionSCC, len(comps))
	for i, comp := range comps {
		nodes := make([]Node, len(comp))
		for j, id := range comp {
			nodes[j] = g.Nodes[id]
		}
		sort.Slice(nodes, func(a, b int) bool { return functionNodeLess(nodes[a], nodes[b]) })
		out[i] = FunctionSCC{Nodes: nodes, Cycle: len(nodes) > 1, Layer: layers[i]}
		if len(nodes) == 1 {
			id := nodes[0].ID
			for _, to := range g.Edges[id] {
				if to == id {
					out[i].Cycle = true
					break
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Layer != out[j].Layer {
			return out[i].Layer < out[j].Layer
		}
		return functionNodeLess(out[i].Nodes[0], out[j].Nodes[0])
	})
	return out
}

func (st *functionTarjan) connect(g *FunctionGraph, scope map[int64]bool, v int64) {
	st.index[v] = st.next
	st.low[v] = st.next
	st.next++
	st.stack = append(st.stack, v)
	st.onStack[v] = true
	neighbours := append([]int64(nil), g.Edges[v]...)
	sort.Slice(neighbours, func(i, j int) bool { return functionNodeLess(g.Nodes[neighbours[i]], g.Nodes[neighbours[j]]) })
	for _, w := range neighbours {
		if !scope[w] {
			continue
		}
		if _, seen := st.index[w]; !seen {
			st.connect(g, scope, w)
			if st.low[w] < st.low[v] {
				st.low[v] = st.low[w]
			}
		} else if st.onStack[w] && st.index[w] < st.low[v] {
			st.low[v] = st.index[w]
		}
	}
	if st.low[v] != st.index[v] {
		return
	}
	var comp []int64
	for {
		w := st.stack[len(st.stack)-1]
		st.stack = st.stack[:len(st.stack)-1]
		st.onStack[w] = false
		comp = append(comp, w)
		if w == v {
			break
		}
	}
	st.comps = append(st.comps, comp)
}

func functionNodeLess(a, b Node) bool {
	if a.Path != b.Path {
		return a.Path < b.Path
	}
	if a.StartLine != b.StartLine {
		return a.StartLine < b.StartLine
	}
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	return a.ID < b.ID
}
