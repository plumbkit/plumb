package cli

// conn_pin_contest_test.go — the contested-pin signal (conn_pin_contest.go) and
// the remedy it changes.
//
// The incident behind it: two agents multiplexed one `plumb serve` without
// declaring a session_id, so PLAN-286's per-agent shards never engaged and both
// fought over the connection-level pin. Each refusal named force: true as the
// remedy, each agent took it, and the pin changed hands fourteen times in
// thirty-five minutes. These tests pin the shape plumb now recognises, and that
// recognising it stops plumb from recommending the move that produced it.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/tools"
)

// TestContestedRing_VerdictShape drives the verdict as a pure function, so the
// window and distinct-destination terms can be tested without a clock or a
// workspace. contestedRing takes `now`, which is what makes the expiry case
// possible at all.
func TestContestedRing_VerdictShape(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }

	cases := []struct {
		name string
		ring []pinDisplacement
		want bool
	}{
		{
			name: "one displacement is a project switch, not a fight",
			ring: []pinDisplacement{{from: "/a", to: "/b", at: ago(time.Minute)}},
			want: false,
		},
		{
			name: "two displacements alternating between two roots",
			ring: []pinDisplacement{
				{from: "/a", to: "/b", at: ago(9 * time.Minute)},
				{from: "/b", to: "/a", at: ago(2 * time.Minute)},
			},
			want: true,
		},
		{
			// The false positive that matters: one agent forcing its way to the
			// same project twice (a raced roots notification, a re-orientation
			// after a restart) has moved the pin once as far as anyone is
			// concerned. Calling that contested would withhold correct advice
			// from a connection nobody is contending.
			name: "two displacements to the SAME destination is not contested",
			ring: []pinDisplacement{
				{from: "/a", to: "/b", at: ago(9 * time.Minute)},
				{from: "/a", to: "/b", at: ago(2 * time.Minute)},
			},
			want: false,
		},
		{
			name: "displacements outside the window do not count",
			ring: []pinDisplacement{
				{from: "/a", to: "/b", at: ago(pinContestWindow + time.Minute)},
				{from: "/b", to: "/a", at: ago(2 * time.Minute)},
			},
			want: false,
		},
		{
			name: "three roots in the window",
			ring: []pinDisplacement{
				{from: "/a", to: "/b", at: ago(20 * time.Minute)},
				{from: "/b", to: "/c", at: ago(10 * time.Minute)},
			},
			want: true,
		},
		{name: "empty ring", ring: nil, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := contestedRing(tc.ring, now); got != tc.want {
				t.Errorf("contestedRing = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestContestState_LatchesOnce: record reports "just became contested" exactly
// once, so the WARN and the health mark do not repeat on every later
// displacement — and the verdict does not decay out of the window afterwards. A
// connection that has been fought over is one whose agents are not declaring
// identities, and that does not stop being true because they paused.
func TestContestState_LatchesOnce(t *testing.T) {
	var p pinContestState
	now := time.Unix(1_800_000_000, 0)

	if p.record("/a", "/b", now) {
		t.Fatal("a single displacement must not flip the signal")
	}
	if p.isContested() {
		t.Fatal("isContested true after one displacement")
	}
	if !p.record("/b", "/a", now.Add(time.Minute)) {
		t.Fatal("the second displacement to a different root must flip the signal")
	}
	if p.record("/a", "/b", now.Add(2*time.Minute)) {
		t.Error("the signal flipped a second time; the WARN and health mark would repeat")
	}
	if !p.isContested() {
		t.Error("the latch did not hold")
	}
	// Far outside the window: still contested.
	if !p.isContested() {
		t.Error("the latch decayed")
	}
}

// TestContestState_RingIsBounded: history is capped, and the cap keeps the most
// RECENT entries — dropping the newest would make the verdict blind to the
// fight actually in progress.
func TestContestState_RingIsBounded(t *testing.T) {
	var p pinContestState
	now := time.Unix(1_800_000_000, 0)
	for i := range pinDisplacementRing + 5 {
		p.record("/a", "/b", now.Add(time.Duration(i)*time.Second))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.ring) != pinDisplacementRing {
		t.Fatalf("ring holds %d entries, want the cap %d", len(p.ring), pinDisplacementRing)
	}
	newest := now.Add(time.Duration(pinDisplacementRing+4) * time.Second)
	if !p.ring[len(p.ring)-1].at.Equal(newest) {
		t.Errorf("newest retained entry is %v, want %v — the cap dropped the wrong end", p.ring[len(p.ring)-1].at, newest)
	}
}

// TestForcedRepin_MarksProvenanceAndContests drives the real re-pin path: an
// explicit pin, then a forced move off it, then a forced move back. It asserts
// the two facts the displaced agent depends on — that the pin records it was
// FORCED and what it displaced, and that the second alternation makes the
// connection contested.
func TestForcedRepin_MarksProvenanceAndContests(t *testing.T) {
	store, ss := newOriginStore(t)
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)
	ctx := context.Background()

	s := newPersistSession(t, store, ss, "proxyX")
	if _, err := s.repinWorkspace(ctx, rootA, "", false); err != nil {
		t.Fatalf("repinWorkspace(A): %v", err)
	}
	if prov := s.pinProvenance(); prov.Forced || prov.Contested {
		t.Fatalf("a first explicit pin is neither forced nor contested: %+v", prov)
	}

	// A peer takes the pin, exactly as the sticky refusal tells it to.
	if _, err := s.repinWorkspace(ctx, rootB, "", true); err != nil {
		t.Fatalf("forced repin(B): %v", err)
	}
	prov := s.pinProvenance()
	if !prov.Forced {
		t.Error("a forced re-pin did not record Forced; the displaced agent gets no notice")
	}
	if prov.Previous != rootA {
		t.Errorf("Previous = %q, want the displaced root %q", prov.Previous, rootA)
	}
	if prov.Contested {
		t.Error("one displacement made the connection contested; that is an ordinary project switch")
	}

	// The displaced agent takes it back — the second half of the alternation.
	if _, err := s.repinWorkspace(ctx, rootA, "", true); err != nil {
		t.Fatalf("forced repin back to A: %v", err)
	}
	if !s.pinProvenance().Contested {
		t.Fatal("two forced alternations between two roots did not make the connection contested")
	}

	// The operator-facing half, and an ORDERING assertion, not a duplicate of the
	// line above. attachOrRepinTo resets Health to "" late in the same mutate
	// closure — a successful re-pin means the session is healthy again — so a
	// contested mark written before that reset is silently wiped by the very
	// re-pin that earned it. The signal survives only because
	// announceContestedPin runs AFTER the reset, and this is what says so: move
	// the announce earlier and every other assertion in this file still passes.
	if health, msg := sessionHealth(t, s.sessionID()); health != "contested_pin" {
		t.Errorf("session health = %q (%q), want contested_pin — the re-pin's own Health reset wiped the mark it just earned", health, msg)
	}
}

// TestUnforcedRepin_DoesNotContest: the guard is on force, not on movement. An
// agent that re-pins its own connection without overriding anything, however
// often, is not contending with anyone.
func TestUnforcedRepin_DoesNotContest(t *testing.T) {
	store, ss := newOriginStore(t)
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)
	ctx := context.Background()

	s := newPersistSession(t, store, ss, "proxyX")
	// Attach from client roots first: a roots-origin pin is not sticky, so the
	// re-pins below land without force and displace nothing deliberate.
	s.attachWorkspace(ctx, "file://"+rootA)
	if _, err := s.repinWorkspace(ctx, rootB, "", false); err != nil {
		t.Fatalf("repinWorkspace(B): %v", err)
	}
	if s.pinContested() {
		t.Fatal("an unforced re-pin off a roots-origin pin contested the connection")
	}
	if prov := s.pinProvenance(); prov.Forced {
		t.Error("an unforced re-pin recorded Forced")
	}
}

// TestContestedPin_RemedyStopsLeadingWithForce is the behavioural point of the
// whole change. Once contested, neither the sticky refusal nor the boundary
// error may hand the caller "retry with force: true" as its headline move —
// that sentence is what both agents were following.
func TestContestedPin_RemedyStopsLeadingWithForce(t *testing.T) {
	store, ss := newOriginStore(t)
	rootA, rootB, rootC := freshTempDir(t), freshTempDir(t), freshTempDir(t)
	for _, r := range []string{rootA, rootB, rootC} {
		mustGitDir(t, r)
	}
	ctx := context.Background()

	s := newPersistSession(t, store, ss, "proxyX")
	if _, err := s.repinWorkspace(ctx, rootA, "", false); err != nil {
		t.Fatalf("repinWorkspace(A): %v", err)
	}

	// Before contest: the ordinary remedy, which names force first.
	_, err := s.repinWorkspace(ctx, rootB, "", false)
	if err == nil {
		t.Fatal("a plain re-pin off an explicit pin must be refused (sticky, issue #182)")
	}
	if !strings.Contains(err.Error(), repinStickyRemedy) {
		t.Errorf("uncontested refusal does not carry the ordinary remedy: %v", err)
	}

	// Two forced alternations make it contested.
	if _, err := s.repinWorkspace(ctx, rootB, "", true); err != nil {
		t.Fatalf("forced repin(B): %v", err)
	}
	if _, err := s.repinWorkspace(ctx, rootA, "", true); err != nil {
		t.Fatalf("forced repin(A): %v", err)
	}

	_, err = s.repinWorkspace(ctx, rootC, "", false)
	if err == nil {
		t.Fatal("the sticky guard stopped refusing once contested; ownership semantics must not change")
	}
	if !strings.Contains(err.Error(), repinContestedRemedy) {
		t.Errorf("contested refusal does not carry the contested remedy: %v", err)
	}
	if strings.Contains(err.Error(), repinStickyRemedy) {
		t.Error("the contested refusal still carries the remedy that leads with force: true")
	}

	// The boundary error the DISPLACED agent hits must be corrected too — it is
	// the surface that agent actually reads, and it carries the same advice.
	berr := s.checkBoundary(rootC+"/x.go", tools.AccessRead)
	if berr == nil {
		t.Fatal("a path in a third project must still be refused")
	}
	if !strings.Contains(berr.Error(), "identify yourself with session_start.session_id") {
		t.Errorf("contested boundary error does not name the real remedy: %v", berr)
	}
	if strings.Contains(berr.Error(), "retry with force: true") {
		t.Errorf("contested boundary error still leads with force: true: %v", berr)
	}
}

// TestContestedPin_DisplacedAgentIsTold: the victim asking for a file in the
// project it was displaced from is told the workspace was taken, not merely
// that it is pinned elsewhere. This is the signal whose absence made the
// incident read as a daemon-restart bug.
func TestContestedPin_DisplacedAgentIsTold(t *testing.T) {
	store, ss := newOriginStore(t)
	rootA, rootB := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, rootA)
	mustGitDir(t, rootB)
	ctx := context.Background()

	s := newPersistSession(t, store, ss, "proxyX")
	if _, err := s.repinWorkspace(ctx, rootA, "", false); err != nil {
		t.Fatalf("repinWorkspace(A): %v", err)
	}
	if _, err := s.repinWorkspace(ctx, rootB, "", true); err != nil {
		t.Fatalf("forced repin(B): %v", err)
	}

	err := s.checkBoundary(rootA+"/main.go", tools.AccessRead)
	if err == nil {
		t.Fatal("a path in the displaced project must still be refused — the pin really did move")
	}
	if !strings.Contains(err.Error(), "force-re-pinned away from "+rootA) {
		t.Errorf("the displaced agent is not told its workspace was taken: %v", err)
	}
	if !strings.Contains(err.Error(), "session_start.session_id") {
		t.Errorf("the displacement notice does not name the fix: %v", err)
	}
}

// TestContestedPin_DoesNotClobberSharedConnectionHealth: contested is the
// WEAKER, inferred signal — nobody declared anything and plumb is reading
// behaviour. It must defer to a mark made on evidence.
func TestContestedPin_DoesNotClobberSharedConnectionHealth(t *testing.T) {
	store, ss := newOriginStore(t)
	s := newPersistSession(t, store, ss, "proxyX")

	// Two declared identities: the connection is shared on evidence.
	s.recordLogicalAgentAttach("agent-a")
	s.recordLogicalAgentAttach("agent-b")
	if got, _ := sessionHealth(t, s.sessionID()); got != "shared_connection_detected" {
		t.Fatalf("health = %q, want shared_connection_detected as the precondition", got)
	}

	s.markContestedPin(2, []string{"/a", "/b"})
	if got, _ := sessionHealth(t, s.sessionID()); got != "shared_connection_detected" {
		t.Errorf("health = %q; the inferred contested mark overwrote a mark made on evidence", got)
	}
}

// TestContestedPin_MarksHealthWhenUnmarked is the other direction: with nothing
// else claiming the field, the operator does get told.
func TestContestedPin_MarksHealthWhenUnmarked(t *testing.T) {
	store, ss := newOriginStore(t)
	s := newPersistSession(t, store, ss, "proxyX")

	s.markContestedPin(2, []string{"/a", "/b"})
	got, msg := sessionHealth(t, s.sessionID())
	if got != "contested_pin" {
		t.Errorf("health = %q, want contested_pin", got)
	}
	if !strings.Contains(msg, "session_start.session_id") {
		t.Errorf("health message gives the operator no next step: %q", msg)
	}
}
