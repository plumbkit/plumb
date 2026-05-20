package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/golimpio/plumb/internal/topology"
)

var topologyStatusSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "workspace": {
      "type": "string",
      "description": "Absolute path to the workspace root. Defaults to the session workspace."
    }
  }
}`)

// TopologyStatus reports the health and statistics of the topology index.
//
// Concurrency: Execute is safe for concurrent use.
type TopologyStatus struct {
	storeFn   func() *topology.Store
	workspace func() string
}

// NewTopologyStatus returns a new TopologyStatus tool.
// storeFn returns the current topology.Store for the session, or nil if disabled.
// workspaceFn returns the resolved workspace path for the session.
func NewTopologyStatus(storeFn func() *topology.Store, workspaceFn func() string) *TopologyStatus {
	return &TopologyStatus{storeFn: storeFn, workspace: workspaceFn}
}

func (*TopologyStatus) Name() string                 { return "topology_status" }
func (*TopologyStatus) InputSchema() json.RawMessage { return topologyStatusSchema }
func (*TopologyStatus) Description() string {
	return "Report the health and statistics of the topology index for this workspace: " +
		"indexer state, indexed/skipped file counts, total nodes and edges, database size, " +
		"last sync time, indexed languages, and the most recent indexing error if any. " +
		"Returns a clear message when topology indexing is disabled."
}

type topologyStatusArgs struct {
	Workspace string `json:"workspace"`
}

func (t *TopologyStatus) Execute(_ context.Context, raw json.RawMessage) (string, error) {
	a, err := parseTopologyStatusArgs(raw)
	if err != nil {
		return "", err
	}
	ws := a.Workspace
	if ws == "" && t.workspace != nil {
		ws = t.workspace()
	}
	store := t.storeFn()
	return formatTopologyStatus(store, ws), nil
}

func parseTopologyStatusArgs(raw json.RawMessage) (topologyStatusArgs, error) {
	var a topologyStatusArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("topology_status: invalid arguments: %w", err)
	}
	return a, nil
}

func formatTopologyStatus(store *topology.Store, workspace string) string {
	if store == nil {
		return "topology indexing is disabled for this session\n" +
			"Set [topology] enabled = true in .plumb/config.toml or ~/.config/plumb/config.toml to enable."
	}
	s := store.Status()
	return topology.FormatStatus(s, workspace)
}
