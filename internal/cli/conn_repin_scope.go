package cli

// conn_repin_scope.go — moving the CONNECTION's pin deliberately, and refusing
// to move it by accident.
//
// Split from conn_repin.go by responsibility: that file owns resolving and
// applying a pin, this one owns who may move the connection's own pin and how
// that intent is carried. The two guards and the scope: "connection" route are
// a single story and were pushing conn_repin.go past the file-size cap.

import (
	"context"
	"fmt"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/sessionstate"
	"github.com/plumbkit/plumb/internal/toolerror"
)

// connScopeAuthorisedKey marks a re-pin as an AUTHORISED connection-scoped move.
//
// repinConnection has to strip the caller's identity so the re-pin reaches the
// connection instead of being routed to that caller's shard — but the
// anonymous-forced-move guard keys on exactly that absence, so without this
// marker an authorised move would refuse itself. The identity is checked before
// the strip; this records that the check happened.
type connScopeAuthorisedKey struct{}

func withConnScopeAuthorised(ctx context.Context) context.Context {
	return context.WithValue(ctx, connScopeAuthorisedKey{}, true)
}

func connScopeAuthorised(ctx context.Context) bool {
	v, _ := ctx.Value(connScopeAuthorisedKey{}).(bool)
	return v
}

// repinConnection moves the CONNECTION's pin on behalf of an identified agent
// (session_start scope: "connection"), rather than the caller's own shard.
//
// It exists because a shared connection's pin otherwise had no attributable way
// to move at all. An identified caller routes to repinAgent and moves its shard;
// only an ANONYMOUS forced call reached the connection level, which is the hole
// acceptance (a) names. Refusing that without providing this would have frozen
// the pin permanently, and on a client like DSH — one MCP connection multiplexed
// across every agent — a frozen connection pin means no agent can relocate the
// project at all.
//
// The caller must be identified. The move resets every peer shard that has not
// pinned a root of its own (followConnectionShards), including its read, write
// and undo state, so it has to be attributable to the agent that asked for it.
func (s *connSession) repinConnection(ctx context.Context, folder, langOverride string, force bool) (string, error) {
	id := mcp.LogicalAgentFromCtx(ctx)
	if id == "" && s.logicalAgents.sharedWith("") {
		return "", toolerror.Wrap(
			fmt.Errorf(`refusing a connection-scoped re-pin to %s: this connection serves several logical agents and this call carries no identity, so a move that resets every peer's workspace, read tracking and undo state cannot be attributed to the agent that asked for it. Identify yourself — pass session_start.session_id, or a per-call _meta[%s] — and retry with scope: "connection"`, folder, mcp.MetaLogicalAgentKey),
			toolerror.KindPinRefused,
			toolerror.ClassFixArguments,
			toolerror.WithTool("session_start"),
			toolerror.WithDetail("scope", "connection"),
			toolerror.WithDetail("requested", folder),
		)
	}
	s.log().Info("daemon: connection-scoped re-pin", "agent", logicalAgentLabel(id), "requested", folder, "force", force)
	// Deliberately NOT under the caller's ctx identity: this moves the
	// CONNECTION, so it must reach attachOrRepinTo rather than being routed to
	// the caller's shard by repinShard.
	return s.repinWorkspaceFrom(withConnScopeAuthorised(mcp.WithoutLogicalAgent(ctx)), folder, langOverride, sessionstate.PinSourceSessionStart, pinTriggerLive, force)
}

// refuseAnonymousForcedMove refuses a forced connection re-pin that carries no
// identity on a connection serving several agents (PLAN-440 acceptance a).
//
// session_start sits outside the write gate so identity stays declarable, and
// repinShard returns nil without a per-call identity, so an anonymous forced
// call reached the connection level — moving the pin and dragging every peer
// shard that had not chosen a root of its own, resetting its read, write and
// undo state, with nobody to attribute it to.
//
// This could not be refused until there was another way to move the connection:
// an identified caller routed to its own shard, and a roots notification
// re-pins unforced and is refused by the sticky guard, so refusing here alone
// would have frozen a shared connection's pin for good — fatal on a client like
// DSH, which multiplexes every agent over one connection. scope: "connection"
// is that way (repinConnection), and connScopeAuthorised marks a move that came
// through it, since routing requires the identity to be stripped first.
//
// Extracted from attachOrRepinTo to keep that function under the complexity cap.
func (s *connSession) refuseAnonymousForcedMove(ctx context.Context, prev, root string, trigger pinTrigger, force bool) error {
	if !force || trigger != pinTriggerLive || prev == "" || root == prev ||
		mcp.LogicalAgentFromCtx(ctx) != "" || connScopeAuthorised(ctx) ||
		!s.logicalAgents.sharedWith("") {
		return nil
	}
	s.log().Warn("daemon: anonymous forced re-pin refused — several agents share this connection",
		"pinned", prev, "requested", root)
	return toolerror.Wrap(
		fmt.Errorf(`refusing a forced re-pin from %s to %s: this connection serves several logical agents and this call carries no identity, so a move that resets every peer's workspace, read tracking and undo state cannot be attributed to the agent that asked for it. Identify yourself — pass session_start.session_id, or a per-call _meta[%s] — then move the connection deliberately with scope: "connection"`, prev, root, mcp.MetaLogicalAgentKey),
		toolerror.KindPinRefused,
		toolerror.ClassFixArguments,
		toolerror.WithTool("session_start"),
		toolerror.WithDetail("scope", "connection"),
		toolerror.WithDetail("pinned", prev),
		toolerror.WithDetail("requested", root),
	)
}

// refuseStickyRepin builds the sticky-pin refusal (issue #182) and records it
// for the operator. Extracted from attachOrRepinTo to keep that function under
// the complexity cap; the reasoning for each part stays with the statements.
func (s *connSession) refuseStickyRepin(v *sessionView, prev, root string) error {
	remedy := s.repinRemedy()
	s.log().Warn("daemon: session_start re-pin refused — explicit pin held (sticky, issue #182)", "pinned", prev, "requested", root, "contested", s.pinContested())
	// Surface the refused steal attempt to the operator (TUI /
	// dashboard); a later successful re-pin clears Health below. The
	// remedy is appended here too (issue #358) — the dashboard alert
	// renders HealthMessage directly, so a message with no next step
	// left the operator with nothing actionable but a loop.
	s.markBoundaryViolation(fmt.Sprintf("session_start re-pin refused: explicit pin %s is sticky; requested %s (issue #182). %s", prev, root, remedy))
	// Classified at "connection" scope: unlike the per-agent refusal in
	// conn_agent_shard.go, force: true here moves the pin EVERY agent on
	// this connection resolves against — and the pin may have been
	// restored from persistence for another conversation entirely. A
	// client must therefore surface this rather than retry it
	// automatically; the scope in Details is what lets it tell the two
	// apart without parsing the sentence.
	return toolerror.Wrap(
		fmt.Errorf("refusing to re-pin this connection from %s to %s: the current pin was set by an explicit session_start (%s), and silently moving it would retarget every relative-path call made over this shared connection — issue #182: a multiplexing client can run several agent sessions over one plumb serve process. %s", prev, root, pinProvenanceOf(v), remedy),
		toolerror.KindPinRefused,
		toolerror.ClassRepinWorkspace,
		toolerror.WithTool("session_start"),
		toolerror.WithDetail("scope", "connection"),
		toolerror.WithDetail("pinned", prev),
		toolerror.WithDetail("requested", root),
	)
}

// stickyPinHolds answers whether the sticky-pin guard (issue #182) stops this
// re-pin, and with what error. handled=true means attachOrRepinTo must return
// without moving anything; err distinguishes a refusal the caller must see from
// a roots-driven request that is simply kept off an explicit pin.
//
// Extracted from attachOrRepinTo, condition and all, to keep that function
// under the complexity cap as guards accumulate on it.
func (s *connSession) stickyPinHolds(v *sessionView, origin sessionstate.PinSource, prev, root string, trigger pinTrigger, force bool) (handled bool, refused error) {
	// Sticky-pin guard (issue #182). Only a LIVE re-pin away from a pin held
	// by an explicit session_start is gated: a same-root request falls
	// through to the promotion branch below, a restore replay is never
	// blocked (the pin's owner re-attaching is not a peer stealing it), and
	// a roots/auto-attach pin is not sticky — the first explicit pin must
	// always land.
	if !force && trigger == pinTriggerLive && prev != "" && root != prev &&
		v.pinOrigin == sessionstate.PinSourceSessionStart {
		if origin == sessionstate.PinSourceSessionStart {
			return true, s.refuseStickyRepin(v, prev, root)
		}
		// A roots-driven re-pin (the client dropped our root from its
		// reported set) is a weaker signal than the deliberate pin: keep the
		// pin, no error — the live counterpart of the persisted-pin
		// promotion rule. onRootsChanged short-circuits this case up front;
		// this in-lane check is the authoritative one.
		s.log().Info("daemon: roots re-pin skipped — explicit session_start pin held (issue #182)", "pinned", prev, "requested", root)
		return true, nil
	}
	return false, nil
}
