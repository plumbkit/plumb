package topology

import "sort"

// SCC is one strongly-connected component of the directory-level import
// graph, restricted to some scope (typically a reachability result's
// Reachable set). Packages is sorted for determinism. Cycle is true when the
// component holds more than one package — a single mutually-recursive import
// cycle IS the finding the caller wants flagged, per PLAN-371's "an SCC >1
// package IS the finding".
type SCC struct {
	Packages []string
	Cycle    bool
	Layer    int // topological layer of the condensation DAG; 0 = no reachable-scope predecessor
}

// CondenseSCCs computes the strongly-connected components of g restricted to
// scope (Tarjan's algorithm), then layers the resulting condensation DAG by
// longest path from a source component (Layer = 1 + max(predecessor layers),
// 0 when there is none) so every edge in the condensation points from a lower
// layer to a higher one. Traversal order is sorted at every step so the
// result — SCC membership, and which layer each lands in — is deterministic
// across runs on an unchanged graph.
func CondenseSCCs(g *PackageGraph, scope map[string]bool) []SCC {
	sccs := tarjanSCC(g, scope)
	compOf := componentIndex(sccs)
	condAdj, indeg := condensationAdjacency(len(sccs), g, scope, compOf)
	layer := layerCondensation(condAdj, indeg)

	out := make([]SCC, len(sccs))
	for i, comp := range sccs {
		out[i] = SCC{Packages: comp, Cycle: len(comp) > 1, Layer: layer[i]}
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Layer != out[b].Layer {
			return out[a].Layer < out[b].Layer
		}
		return out[a].Packages[0] < out[b].Packages[0]
	})
	return out
}

// componentIndex maps every directory to the index of its SCC in sccs.
func componentIndex(sccs [][]string) map[string]int {
	compOf := make(map[string]int, len(sccs))
	for i, comp := range sccs {
		for _, d := range comp {
			compOf[d] = i
		}
	}
	return compOf
}

// condensationAdjacency builds the condensation DAG's adjacency (deduped,
// self-loops on a component dropped) and each component's in-degree, from
// every g.Edges pair whose endpoints both fall in scope. n is the number of
// components (len of the sccs slice compOf was built from).
func condensationAdjacency(n int, g *PackageGraph, scope map[string]bool, compOf map[string]int) ([]map[int]bool, []int) {
	condAdj := make([]map[int]bool, n)
	indeg := make([]int, n)
	for i := range condAdj {
		condAdj[i] = map[int]bool{}
	}
	for from, tos := range g.Edges {
		ci, ok := compOf[from]
		if !ok {
			continue
		}
		for to := range tos {
			if !scope[to] {
				continue
			}
			cj := compOf[to]
			if cj == ci || condAdj[ci][cj] {
				continue
			}
			condAdj[ci][cj] = true
			indeg[cj]++
		}
	}
	return condAdj, indeg
}

// layerCondensation assigns each condensation-DAG component a topological
// layer by longest path from a source (in-degree 0) component: layer =
// 1 + max(predecessor layers), 0 when there is none. Kahn's algorithm,
// sorted at every step for a deterministic result on an unchanged graph.
func layerCondensation(condAdj []map[int]bool, indeg []int) []int {
	n := len(condAdj)
	layer := make([]int, n)
	remaining := append([]int(nil), indeg...)
	queue := make([]int, 0, n)
	for i, d := range indeg {
		if d == 0 {
			queue = append(queue, i)
		}
	}
	sort.Ints(queue)
	for len(queue) > 0 {
		v := queue[0]
		queue = queue[1:]
		succs := make([]int, 0, len(condAdj[v]))
		for w := range condAdj[v] {
			succs = append(succs, w)
		}
		sort.Ints(succs)
		for _, w := range succs {
			if layer[v]+1 > layer[w] {
				layer[w] = layer[v] + 1
			}
			remaining[w]--
			if remaining[w] == 0 {
				insertSorted(&queue, w)
			}
		}
	}
	return layer
}

// insertSorted inserts v into the sorted queue, keeping it sorted without a
// full re-sort per insertion (layerCondensation's queue.sort.Ints was doing a
// full sort on every drain iteration, which is where its share of the
// function's cognitive-complexity budget went — a single insertion point is
// both cheaper and simpler to read).
func insertSorted(queue *[]int, v int) {
	q := *queue
	i := sort.SearchInts(q, v)
	q = append(q, 0)
	copy(q[i+1:], q[i:])
	q[i] = v
	*queue = q
}

// tarjanSCC runs Tarjan's strongly-connected-components algorithm over g,
// restricted to nodes in scope. Each returned component's Packages is sorted;
// components themselves are returned in Tarjan's natural (reverse
// topological) discovery order, which CondenseSCCs re-sorts by layer.
func tarjanSCC(g *PackageGraph, scope map[string]bool) [][]string {
	st := &tarjanState{
		index:   map[string]int{},
		low:     map[string]int{},
		onStack: map[string]bool{},
	}
	dirs := make([]string, 0, len(scope))
	for d := range scope {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	for _, d := range dirs {
		if _, seen := st.index[d]; !seen {
			st.strongconnect(g, d, scope)
		}
	}
	return st.sccs
}

type tarjanState struct {
	index   map[string]int
	low     map[string]int
	onStack map[string]bool
	stack   []string
	counter int
	sccs    [][]string
}

func (st *tarjanState) strongconnect(g *PackageGraph, v string, scope map[string]bool) {
	st.index[v] = st.counter
	st.low[v] = st.counter
	st.counter++
	st.stack = append(st.stack, v)
	st.onStack[v] = true

	var neighbours []string
	for nb := range g.Edges[v] {
		if scope[nb] {
			neighbours = append(neighbours, nb)
		}
	}
	sort.Strings(neighbours)
	for _, w := range neighbours {
		if _, seen := st.index[w]; !seen {
			st.strongconnect(g, w, scope)
			if st.low[w] < st.low[v] {
				st.low[v] = st.low[w]
			}
		} else if st.onStack[w] {
			if st.index[w] < st.low[v] {
				st.low[v] = st.index[w]
			}
		}
	}

	if st.low[v] == st.index[v] {
		var comp []string
		for {
			w := st.stack[len(st.stack)-1]
			st.stack = st.stack[:len(st.stack)-1]
			st.onStack[w] = false
			comp = append(comp, w)
			if w == v {
				break
			}
		}
		sort.Strings(comp)
		st.sccs = append(st.sccs, comp)
	}
}
