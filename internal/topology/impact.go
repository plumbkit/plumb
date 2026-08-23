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
	centre, err := resolveNode(db, name)
	if err != nil {
		return nil, err
	}
	return ImpactFrom(ctx, db, centre, opts)
}

// ImpactFrom performs the bidirectional BFS from an already-resolved centre
// node, so a caller that disambiguated an ambiguous name (via ResolveNodes)
// controls exactly which node anchors the blast-radius analysis.
func ImpactFrom(ctx context.Context, db *sql.DB, centre Node, opts ImpactOpts) (*ImpactResult, error) {
	return impactBFS(ctx, db, centre, clampImpactOpts(opts))
}

// ClampTraversalOpts reports what ImpactFrom will actually run with after the
// traversal's own ceilings are applied.
//
// It is exported for the assertion an in-process caller owes itself: the budget
// it sizes lives in its package and the ceilings live here, so nothing at the
// edit site shows that raising one past the other silently reinstates the
// clamping PLAN-407 removed. A test in the calling package that compares this
// against the opts it passes closes that gap, and follows the clamp if the
// clamp changes — which a test reading the constants would not.
func ClampTraversalOpts(opts ImpactOpts) ImpactOpts { return clampImpactOpts(opts) }

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
