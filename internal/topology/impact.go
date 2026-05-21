package topology

import (
	"context"
	"database/sql"
	"fmt"
)

const (
	defaultImpactDepth    = 3
	defaultImpactMaxNodes = 100
	defaultImpactMaxBytes = 30000
)

// Impact performs two directed BFS traversals from the named symbol:
//   - DependsOn: outward (centre → what it depends on)
//   - DependedOnBy: inward (what depends on centre → centre)
//
// The same hard caps as Explore apply.
func Impact(ctx context.Context, db *sql.DB, name string, opts ImpactOpts) (*ImpactResult, error) {
	opts = clampImpactOpts(opts)
	centre, err := resolveNode(db, name)
	if err != nil {
		return nil, err
	}
	return impactBFS(ctx, db, centre, opts)
}

func clampImpactOpts(opts ImpactOpts) ImpactOpts {
	if opts.Depth <= 0 {
		opts.Depth = defaultImpactDepth
	}
	if opts.Depth > hardCapDepth {
		opts.Depth = hardCapDepth
	}
	if opts.MaxNodes <= 0 {
		opts.MaxNodes = defaultImpactMaxNodes
	}
	if opts.MaxNodes > hardCapNodes {
		opts.MaxNodes = hardCapNodes
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = defaultImpactMaxBytes
	}
	if opts.MaxBytes > hardCapBytes {
		opts.MaxBytes = hardCapBytes
	}
	return opts
}

func impactBFS(ctx context.Context, db *sql.DB, centre Node, opts ImpactOpts) (*ImpactResult, error) {
	outOpts := ExploreOpts{
		Depth:     opts.Depth,
		MaxNodes:  opts.MaxNodes,
		MaxBytes:  opts.MaxBytes,
		EdgeKinds: opts.EdgeKinds,
		Direction: DirectionOutward,
	}
	dependsOn, err := bfs(ctx, db, centre, outOpts)
	if err != nil {
		return nil, fmt.Errorf("topology: impact outward BFS: %w", err)
	}

	inOpts := ExploreOpts{
		Depth:     opts.Depth,
		MaxNodes:  opts.MaxNodes,
		MaxBytes:  opts.MaxBytes,
		EdgeKinds: opts.EdgeKinds,
		Direction: DirectionInward,
	}
	dependedOnBy, err := bfs(ctx, db, centre, inOpts)
	if err != nil {
		return nil, fmt.Errorf("topology: impact inward BFS: %w", err)
	}

	return &ImpactResult{
		Centre:       centre,
		DependsOn:    dependsOn,
		DependedOnBy: dependedOnBy,
	}, nil
}
