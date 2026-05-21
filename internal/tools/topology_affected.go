package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/golimpio/plumb/internal/topology"
)

var topologyAffectedSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "files": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Workspace-relative file paths to treat as change roots."
    },
    "symbols": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Symbol names to treat as change roots."
    },
    "max_results": {
      "type": "integer",
      "description": "Maximum affected nodes to return. Default 50.",
      "default": 50
    }
  }
}`)

// TopologyAffected traverses inward edges from changed files/symbols to report
// dependents and likely affected tests.
//
// Concurrency: Execute is safe for concurrent use.
type TopologyAffected struct {
	storeFn func() *topology.Store
}

// NewTopologyAffected returns a new TopologyAffected tool.
func NewTopologyAffected(storeFn func() *topology.Store) *TopologyAffected {
	return &TopologyAffected{storeFn: storeFn}
}

func (*TopologyAffected) Name() string                 { return "topology_affected" }
func (*TopologyAffected) InputSchema() json.RawMessage { return topologyAffectedSchema }
func (*TopologyAffected) Description() string {
	return "Given changed files or symbols, returns likely affected files and tests " +
		"by traversing inward dependency edges. " +
		"Primary use: after writing code, suggest which tests to run without a full go test ./... . " +
		"Source is 'topology' (approximate). Returns a clear message when topology is disabled."
}

type topologyAffectedArgs struct {
	Files      []string `json:"files"`
	Symbols    []string `json:"symbols"`
	MaxResults int      `json:"max_results"`
}

// affectedResult collects dependents and likely-affected tests.
type affectedResult struct {
	Dependents []topology.Node
	Tests      []topology.Node
	Truncated  bool
}

func (t *TopologyAffected) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	a, err := parseTopologyAffectedArgs(raw)
	if err != nil {
		return "", err
	}
	if err := a.validate(); err != nil {
		return "", err
	}
	store := t.storeFn()
	result, runErr := t.run(ctx, store, a)
	if runErr != nil {
		return "", runErr
	}
	return formatAffectedResult(result, a), nil
}

func parseTopologyAffectedArgs(raw json.RawMessage) (topologyAffectedArgs, error) {
	var a topologyAffectedArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("topology_affected: invalid arguments: %w", err)
	}
	if a.MaxResults <= 0 {
		a.MaxResults = 50
	}
	return a, nil
}

func (a *topologyAffectedArgs) validate() error {
	if len(a.Files) == 0 && len(a.Symbols) == 0 {
		return fmt.Errorf("topology_affected: at least one file or symbol is required")
	}
	return nil
}

func (t *TopologyAffected) run(ctx context.Context, store *topology.Store, a topologyAffectedArgs) (*affectedResult, error) {
	if store == nil {
		return nil, nil
	}
	roots, err := resolveAffectedRoots(ctx, store, a)
	if err != nil {
		return nil, err
	}
	return collectAffected(ctx, store, roots, a.MaxResults)
}

// resolveAffectedRoots looks up all named symbols and file-path nodes to use as
// inward-BFS starting points.
func resolveAffectedRoots(ctx context.Context, store *topology.Store, a topologyAffectedArgs) ([]topology.Node, error) {
	var roots []topology.Node
	for _, sym := range a.Symbols {
		opts := topology.SearchOpts{Limit: 5}
		results, err := store.Search(ctx, sym, opts)
		if err != nil {
			return nil, fmt.Errorf("topology_affected: search %q: %w", sym, err)
		}
		for _, r := range results {
			if r.Node.Name == sym {
				roots = append(roots, r.Node)
				break
			}
		}
	}
	for _, f := range a.Files {
		opts := topology.SearchOpts{Limit: 5}
		results, err := store.Search(ctx, f, opts)
		if err != nil {
			return nil, fmt.Errorf("topology_affected: search file %q: %w", f, err)
		}
		for _, r := range results {
			if r.Node.Path == f || strings.HasSuffix(r.Node.Path, f) {
				roots = append(roots, r.Node)
				break
			}
		}
	}
	return roots, nil
}

func collectAffected(ctx context.Context, store *topology.Store, roots []topology.Node, maxResults int) (*affectedResult, error) {
	seen := map[int64]bool{}
	var dependents []topology.Node
	var tests []topology.Node
	truncated := false

	for _, root := range roots {
		nb, err := store.Impact(ctx, root.Name, topology.ImpactOpts{
			Depth:     2,
			MaxNodes:  maxResults,
			MaxBytes:  100000,
			EdgeKinds: []string{"calls", "imports", "contains"},
		})
		if err != nil {
			continue
		}
		for _, n := range nb.DependedOnBy.Nodes {
			if seen[n.ID] {
				continue
			}
			seen[n.ID] = true
			if n.Kind == topology.KindTest {
				tests = append(tests, n)
			} else {
				dependents = append(dependents, n)
			}
			if len(dependents)+len(tests) >= maxResults {
				truncated = true
				break
			}
		}
		if truncated {
			break
		}
	}
	return &affectedResult{Dependents: dependents, Tests: tests, Truncated: truncated}, nil
}

func formatAffectedResult(result *affectedResult, a topologyAffectedArgs) string {
	if result == nil {
		return "topology indexing is disabled for this session\n" +
			"Set [topology] enabled = true in .plumb/config.toml to enable."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "topology affected: %d files, %d symbols changed (source=topology)\n\n",
		len(a.Files), len(a.Symbols))

	if len(result.Dependents) == 0 {
		sb.WriteString("affected files: (none found)\n")
	} else {
		fmt.Fprintf(&sb, "affected files (%d):\n", len(result.Dependents))
		for _, n := range result.Dependents {
			fmt.Fprintf(&sb, "  %s %s — %s", string(n.Kind), n.Name, n.Path)
			if n.StartLine > 0 {
				fmt.Fprintf(&sb, " L%d", n.StartLine)
			}
			sb.WriteString("\n")
		}
	}

	sb.WriteString("\n")
	if len(result.Tests) == 0 {
		sb.WriteString("likely affected tests: (none found)\n")
	} else {
		fmt.Fprintf(&sb, "likely affected tests (%d):\n", len(result.Tests))
		for _, n := range result.Tests {
			fmt.Fprintf(&sb, "  %s — %s L%d\n", n.Name, n.Path, n.StartLine)
		}
	}

	if result.Truncated {
		sb.WriteString("\n[truncated: max_results reached]\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}
