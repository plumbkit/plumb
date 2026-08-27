package cli

// conn_pin_contest.go — the connection's history of FORCED pin displacements,
// and the "this connection is contested" signal derived from it.
//
// The sticky-pin guard (issue #182) refuses a peer's re-pin and names force:
// true as the remedy. For one agent switching its own project that is right.
// For several agents multiplexing one `plumb serve` it is the engine of a
// fight: each refusal teaches the caller to take the pin, and the agent it was
// taken from is told only that the connection is pinned somewhere it never
// named. In the incident this file was written for, two agents alternated a
// forced re-pin fourteen times in thirty-five minutes, and the displaced one
// concluded its workspace had drifted during a daemon restart.
//
// plumb cannot tell those agents apart — neither declared a session_id, so
// PLAN-286's per-agent shards never engaged (see conn_logical_agent.go). But it
// can see the SHAPE: a pin force-taken between different roots, repeatedly, in
// a short window. That is not a project switch, and it is enough to stop
// recommending the move that produces it.
//
// This changes what plumb RECOMMENDS, never what it allows. A forced re-pin on
// a contested connection still succeeds: the daemon cannot know which of two
// undeclared agents is entitled to the workspace, and guessing wrong would
// strand real work. Modelled on logicalAgentState — a small mutex-guarded
// struct on connSession, not a sessionView field, because this is append-only
// history rather than part of the copy-on-write view.

import (
	"fmt"
	"sync"
	"time"

	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/sessionstate"
)

const (
	// pinContestWindow is how far back a displacement counts. Long enough to
	// span an agent's turn (the observed alternations were 2–9 minutes apart),
	// short enough that a connection legitimately reused across a day's worth of
	// separate conversations is never called contested.
	pinContestWindow = 30 * time.Minute
	// pinContestMinDisplacements is the count inside the window that flips the
	// signal. Two, not one: a single forced re-pin is the ordinary "new
	// conversation deliberately switching this connection", which the refusal
	// message exists to permit. It takes a SECOND one to show the pin is being
	// passed around rather than moved.
	pinContestMinDisplacements = 2
	// pinDisplacementRing bounds the retained history. Only the window matters
	// for the verdict, so this exists to cap memory on a long-lived connection,
	// not to bound the count.
	pinDisplacementRing = 8
)

// pinDisplacement is one forced re-pin that took the connection away from a
// root another caller had deliberately pinned.
type pinDisplacement struct {
	from string
	to   string
	at   time.Time
}

// pinContestState is the connection's recent forced-displacement history.
// Guarded by mu: displacements are recorded on the mutation lane while
// pinContested is read from buildPathPolicy and from provenance accessors on
// other goroutines.
type pinContestState struct {
	mu sync.Mutex
	// ring holds the most recent displacements, oldest first, capped at
	// pinDisplacementRing.
	ring []pinDisplacement
	// contested latches once the signal has fired, so the WARN and the health
	// mark happen once per connection rather than on every subsequent
	// displacement. It deliberately does NOT decay out of the window: a
	// connection that has been fought over is one whose agents are not declaring
	// identities, and that does not stop being true because they paused.
	contested bool
}

// record appends a displacement and reports whether it is the one that made the
// connection contested (true exactly once per connection).
func (p *pinContestState) record(from, to string, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ring = append(p.ring, pinDisplacement{from: from, to: to, at: now})
	if len(p.ring) > pinDisplacementRing {
		p.ring = p.ring[len(p.ring)-pinDisplacementRing:]
	}
	if p.contested {
		return false
	}
	if !contestedRing(p.ring, now) {
		return false
	}
	p.contested = true
	return true
}

// contested reports the latched signal.
func (p *pinContestState) isContested() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.contested
}

// summary renders the in-window displacement count and the distinct roots
// involved, for the operator-facing WARN and health message. Returns 0 when
// nothing is in the window.
func (p *pinContestState) summary(now time.Time) (count int, roots []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]struct{}{}
	for _, d := range p.ring {
		if now.Sub(d.at) > pinContestWindow {
			continue
		}
		count++
		for _, r := range []string{d.from, d.to} {
			if _, ok := seen[r]; r != "" && !ok {
				seen[r] = struct{}{}
				roots = append(roots, r)
			}
		}
	}
	return count, roots
}

// contestedRing is the verdict: at least pinContestMinDisplacements
// displacements inside the window, naming at least two DISTINCT destination
// roots.
//
// The distinct-destination term is what separates a fight from a retry. An
// agent that forces its way to the same project twice — because its first
// attempt raced a peer's roots notification, or because it re-oriented after a
// restart — has moved the pin once as far as anyone is concerned. Counting that
// as contested would suppress the force advice for a connection nobody is
// contending, which is the false positive that matters: the advice is correct
// there, and withholding it strands a single agent that cannot switch projects.
func contestedRing(ring []pinDisplacement, now time.Time) bool {
	count := 0
	dests := map[string]struct{}{}
	for _, d := range ring {
		if now.Sub(d.at) > pinContestWindow {
			continue
		}
		count++
		if d.to != "" {
			dests[d.to] = struct{}{}
		}
	}
	return count >= pinContestMinDisplacements && len(dests) >= 2
}

// pinContested reports whether this connection's pin is contested. Safe to call
// from inside the mutation lane: it takes only the contest mutex.
func (s *connSession) pinContested() bool { return s.pinContest.isContested() }

// recordForcedDisplacement records a forced re-pin that moved the connection off
// a root someone had deliberately pinned. It reports whether THIS displacement
// is the one that made the connection contested, so the caller can announce it.
//
// Recording and announcing are separate on purpose, and the split is
// load-bearing rather than stylistic. Recording must happen EARLY in
// attachOrRepinTo's mutate closure — before buildPathPolicy — so the re-pin's
// own policy already carries the contested verdict and the displaced agent's
// very next boundary error is the corrected one. Announcing must happen LATE,
// after the same closure's session.Patch resets Health to "" on a successful
// re-pin: marked any earlier, the contested condition would be wiped by the
// very re-pin that created it.
//
// It also owns the question of WHETHER this re-pin is a displacement at all,
// rather than leaving that condition at the call site: a forced move only
// displaces someone when there was a root to displace and it was pinned
// deliberately. A forced FIRST attach, or a force over a roots-origin pin, took
// nothing from anybody.
func (s *connSession) recordForcedDisplacement(v *sessionView, prev, root string, force bool) (justContested bool) {
	if !force || prev == "" || root == "" || prev == root {
		return false
	}
	if v.pinOrigin != sessionstate.PinSourceSessionStart {
		return false
	}
	return s.pinContest.record(prev, root, time.Now())
}

// announceContestedPin emits the one-per-connection operator signal for a
// connection that has just become contested: a daemon-log WARN naming what was
// observed, and a health mark for the TUI and dashboard. A no-op unless this
// re-pin is the one that flipped the signal, so the caller needs no condition of
// its own. Call at the END of the re-pin, after Health has been reset — see
// recordForcedDisplacement.
func (s *connSession) announceContestedPin(justContested bool) {
	if !justContested {
		return
	}
	count, roots := s.pinContest.summary(time.Now())
	s.log().Warn("daemon: connection pin is contested — it has been force-re-pinned between projects repeatedly; the agents sharing this connection are not declaring session_id, so per-agent state cannot be separated (issue #182)",
		"displacements", count, "roots", boundedForLog(roots, 4), "remedy", repinContestedRemedy)
	s.markContestedPin(count, roots)
}

// markContestedPin records the contested condition on the session record for the
// operator (TUI, dashboard), without clobbering a more specific mark.
//
// "blocked" (markBoundaryViolation) and "shared_connection_detected"
// (markSharedConnectionDetected) both describe something the operator must act
// on more urgently than this: the first says calls are being refused, the second
// says agents ARE declaring identities and plumb is already isolating them. This
// is the weaker, inferred signal — nobody declared anything and plumb is
// guessing from behaviour — so it defers to either rather than overwriting it.
func (s *connSession) markContestedPin(count int, roots []string) {
	if s.sessionID() == "" {
		return
	}
	msg := fmt.Sprintf(
		"this connection's workspace pin has been force-taken %d times between %v in the last %s: "+
			"several agents are multiplexing one plumb serve without declaring an identity, so plumb "+
			"cannot keep their pins apart — pass session_start.session_id on every call, or run one "+
			"plumb serve per agent",
		count, boundedForLog(roots, 4), pinContestWindow)
	session.Patch(s.sessionID(), func(info *session.Info) {
		if info.Health != "" && info.Health != "contested_pin" {
			return
		}
		info.Health = "contested_pin"
		info.HealthMessage = msg
	})
}
