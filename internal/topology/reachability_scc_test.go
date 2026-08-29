package topology

import "testing"

// TestSCCLayerCondensation_TakesMaxNotLastWrite pins the longest-path
// layering semantics in layerCondensation: a node's layer must be
// 1 + MAX(predecessor layers), not simply whatever its LAST-processed
// predecessor happened to write. The graph below is built so the high-layer
// predecessor (reached via R->M->H, two hops) becomes ready and fires BEFORE
// the low-layer predecessor (reached via a direct R->L edge, one hop) — L has
// a larger component index than H, and layerCondensation's queue stays
// sorted by index (insertSorted), so H's node jumps ahead of the
// already-ready-but-larger-index L once H itself becomes ready. An
// unconditional overwrite (dropping the "> " comparison) would leave w's
// layer at L's smaller value (2) instead of the correct 3.
//
//	R(0) --> M(1) --> H(2) --> w(4)   (longest path: 3 edges)
//	R(0) --> L(3) --------------> w(4) (short path: 1 edge)
func TestSCCLayerCondensation_TakesMaxNotLastWrite(t *testing.T) {
	condAdj := []map[int]bool{
		0: {1: true, 3: true}, // R -> M, L
		1: {2: true},          // M -> H
		2: {4: true},          // H -> w
		3: {4: true},          // L -> w
		4: {},                 // w
	}
	indeg := []int{0, 1, 1, 1, 2}

	layer := layerCondensation(condAdj, indeg)

	want := []int{0, 1, 2, 1, 3}
	for i, w := range want {
		if layer[i] != w {
			t.Errorf("layer[%d] = %d, want %d (full result: %v)", i, layer[i], w, layer)
		}
	}
}
