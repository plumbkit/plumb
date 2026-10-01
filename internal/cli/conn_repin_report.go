package cli

// conn_repin_report.go — telling session_start WHICH pin a re-pin moved
// (issue #517).
//
// The daemon decides whether a re-pin moves the calling agent's own shard
// (repinShard: an identified caller on a shared connection) or the
// connection's pin (scope "connection", an anonymous caller, or a
// single-agent connection), and shards that never chose a root follow the
// connection (followConnectionShards). The tool used to receive only the
// previous root and render a neutral line, so an anonymous caller that
// dragged its peers with a connection move was never told, and an agent that
// moved the connection's pin while holding its own was shown its own root as
// "from" and the connection's new root as its workspace.

import (
	"context"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/tools"
)

// repinOutcome is what one re-pin did: the requested folder's resolved root,
// which pin it moved, that pin's root before the call (equal to root when
// nothing moved), the ids of the shards that followed a connection move, and
// the root the caller resolves against afterwards.
type repinOutcome struct {
	root      string
	scope     tools.PinScope
	from      string
	followed  []string
	effective string
}

// repinReport renders a completed re-pin's outcome in session_start's terms.
//
// Followers excludes the caller: a scope "connection" move by an agent whose
// shard still sits where the connection seeded it drags that shard too, but
// the caller is not one of the OTHER agents the report counts.
//
// Effective is derived from the move itself (repinWorkspaceFrom and
// repinConnection set it), not re-read through workspaceFor afterwards, where
// a peer's concurrent move could already have changed what the connection
// resolves to and so mislabel whose pin the caller is on.
func (s *connSession) repinReport(ctx context.Context, out repinOutcome) tools.RepinReport {
	caller := mcp.LogicalAgentFromCtx(ctx)
	followers := 0
	for _, id := range out.followed {
		if id != caller {
			followers++
		}
	}
	return tools.RepinReport{
		Root:      out.root,
		Scope:     out.scope,
		From:      out.from,
		Followers: followers,
		Effective: out.effective,
	}
}
