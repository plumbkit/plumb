package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/render"
	"github.com/plumbkit/plumb/internal/topology"
)

// checkTopology reports the health of the per-workspace topology index. It is a
// no-op pass when topology is disabled (it is on by default; this is the
// opted-out case). When enabled, it
// inspects the on-disk index read-only (without starting an indexer): a missing
// or empty index is a failure with a hint to run a daemon session.
func checkTopology(ws string) []checkResult {
	cfg, err := config.Load()
	if err != nil {
		return []checkResult{{
			name:   "topology",
			ok:     false,
			detail: err.Error(),
			fix:    "fix global config at " + contractConfigPath(config.GlobalConfigPath()),
		}}
	}
	if ws != "" {
		if merged, mErr := config.LoadProject(cfg, ws); mErr == nil {
			cfg = merged
		}
	}
	if !cfg.Topology.Enabled {
		return []checkResult{{
			name:   "topology",
			ok:     true,
			detail: "disabled ([topology] enabled = false — on by default)",
		}}
	}
	if ws == "" {
		return []checkResult{{
			name:   "topology",
			ok:     true,
			detail: "enabled (pass --workspace to inspect the index)",
		}}
	}
	return checkTopologyIndex(ws)
}

// checkTopologyIndex inspects the on-disk topology index for an enabled
// workspace. A missing or corrupt DB is a hard failure; the health of an index
// that does exist is classified by topologyIndexHealth.
func checkTopologyIndex(ws string) []checkResult {
	st, err := topology.StatusForWorkspace(ws)
	if err != nil {
		if os.IsNotExist(err) {
			return []checkResult{{
				name:   "topology",
				ok:     false,
				detail: "enabled but no index found",
				fix:    "open a plumb session in this workspace so the daemon builds the index",
			}}
		}
		return []checkResult{{
			name:   "topology",
			ok:     false,
			detail: err.Error(),
			fix:    "the index may be corrupt — remove " + contractConfigPath(topology.DBPath(ws)) + " to rebuild",
		}}
	}
	return []checkResult{topologyIndexHealth(st)}
}

// topologyIndexHealth classifies a topology Status whose DB exists. An index
// that has not finished its first pass is a non-fatal warning rather than a
// failure: a freshly enabled workspace inspected before the background indexer
// completes is healthy-but-pending, not broken, and `plumb doctor` must not
// emit a false negative (or a non-zero exit) during that window. The states:
//
//   - no file processed yet — cold start, warning;
//   - files seen but all errored/skipped — warning;
//   - files indexed but no symbols — warning (legitimate for a docs/config-only
//     tree; also the signature of a broken extractor, so it is surfaced);
//   - symbols present — pass.
func topologyIndexHealth(st topology.Status) checkResult {
	switch {
	case st.IndexedFiles == 0 && st.SkippedFiles == 0:
		return checkResult{
			name:   "topology",
			ok:     true,
			warn:   true,
			detail: "index is empty — initial indexing may still be in progress",
			fix:    "re-run once the first index completes; if it stays empty, check daemon.log",
		}
	case st.IndexedFiles == 0:
		return checkResult{
			name:   "topology",
			ok:     true,
			warn:   true,
			detail: fmt.Sprintf("no files indexed yet (%d skipped)", st.SkippedFiles),
			fix:    "check daemon.log for extractor errors",
		}
	case st.TotalNodes == 0:
		return checkResult{
			name:   "topology",
			ok:     true,
			warn:   true,
			detail: fmt.Sprintf("%d files indexed but no symbols extracted (expected for a docs/config-only tree)", st.IndexedFiles),
			fix:    "if this tree has source files, check daemon.log for extractor errors",
		}
	default:
		detail := fmt.Sprintf("%d files, %d nodes, %d edges, %s",
			st.IndexedFiles, st.TotalNodes, st.TotalEdges, render.HumanBytes(st.DBSizeBytes))
		if len(st.Languages) > 0 {
			detail += "  [" + strings.Join(st.Languages, ", ") + "]"
		}
		return checkResult{name: "topology", ok: true, detail: detail}
	}
}
