package tools

import "github.com/plumbkit/plumb/internal/topology"

// How far a test is from the change in the dependent walk, and which of two
// paths to the same test to keep. context_for_task's named tests rank by these.

// betterPath reports whether reaching t again in hops, aside or not, beats the
// path it was recorded with: exercising the change beats not, then fewer hops
// beat more, and any count beats none (0).
func betterPath(t affectedTest, hops int, aside bool) bool {
	if t.Aside != aside {
		return !aside
	}
	return t.Hops == 0 || hops < t.Hops
}

// inwardHops is the fewest inward edges from root to each node of an inward
// neighbourhood. An edge FromID -> ToID means FromID depends on ToID, so the walk
// steps from a node to everything that depends on it. A node the edges do not
// connect to root is absent (0).
func inwardHops(root int64, edges []topology.Edge) map[int64]int {
	dependents := map[int64][]int64{}
	for _, e := range edges {
		dependents[e.ToID] = append(dependents[e.ToID], e.FromID)
	}
	hops := map[int64]int{root: 0}
	frontier := []int64{root}
	for len(frontier) > 0 {
		var next []int64
		for _, id := range frontier {
			for _, d := range dependents[id] {
				if _, ok := hops[d]; !ok {
					hops[d] = hops[id] + 1
					next = append(next, d)
				}
			}
		}
		frontier = next
	}
	delete(hops, root)
	return hops
}
