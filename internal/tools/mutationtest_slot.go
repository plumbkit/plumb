package tools

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/textfmt"
)

// mutationtest_slot.go is mutation_test's daemon-wide run slot: who holds it,
// how far their run has got, and the release when the holder's connection goes.
//
// ONE RUN PER DAEMON, NOT PER WORKSPACE. A per-workspace slot looks tempting but
// is unsafe: workspaces nest (the ops root and the ./plumb submodule it
// contains are two workspaces over one tree, and a superproject's build compiles
// the submodule), so two "different" workspaces can mutate and build the same
// files and read each other's mutant as their own result. A run also saturates
// the build and test toolchain, which is the machine's, not the workspace's.
//
// What made the single slot painful was never its scope but its opacity: a
// refusal that named nobody, and a slot that stayed held for as long as a
// crashed client's run took to finish. The holder record fixes the first; the
// connection watch in cancelOnDisconnect fixes the second.

// mutationRun is THE slot. Process-global by design, like pathLocks in
// file_write_helpers.go: tool instances are per-connection, so a field on
// MutationTest would let two agents mutate the same working tree at once and
// read each other's breakage as their own result — a mutant reported `survived`
// only because a peer's mutant was the thing failing the suite.
var mutationRun mutationSlot

// Run phases, as a refusal names them.
const (
	stepPreflight = "checking its mutants"
	stepCompile   = "compile gate"
	stepTest      = "test command"
)

// mutationHolder is what a refused caller is told about the run in its way.
type mutationHolder struct {
	session   string // the holder's session name; "" when unwired
	sessionID string
	workspace string
	started   time.Time
	mutants   int    // n: how many mutants the run was given
	current   int    // k: the mutant in flight (1-based); 0 during the baseline
	step      string // one of the step* phases
}

// mutationSlot is the single run slot plus a record of its holder.
//
// Concurrency: every field is guarded by mu. Only the holder moves the progress
// fields, and only between a successful tryAcquire and its release.
type mutationSlot struct {
	mu     sync.Mutex
	held   bool
	holder mutationHolder
}

// tryAcquire takes the slot for h, or reports the current holder. Never waits:
// queueing behind a suite that may run for minutes is worse than an immediate,
// explicit refusal the agent can act on.
func (s *mutationSlot) tryAcquire(h mutationHolder) (current mutationHolder, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held {
		return s.holder, false
	}
	s.held, s.holder = true, h
	return h, true
}

func (s *mutationSlot) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held, s.holder = false, mutationHolder{}
}

// atMutant records that the holder has moved on to mutant k (1-based).
func (s *mutationSlot) atMutant(k int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held {
		s.holder.current = k
	}
}

// atStep records the phase the holder is in: one of the step* constants.
func (s *mutationSlot) atStep(step string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held {
		s.holder.step = step
	}
}

// snapshot reports the holder and whether the slot is held.
func (s *mutationSlot) snapshot() (mutationHolder, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.holder, s.held
}

// describe renders the holder for a refusal: who, where, since when, and how far.
func (h mutationHolder) describe(now time.Time) string {
	who := "an unidentified session"
	switch {
	case h.session != "" && h.sessionID != "":
		who = fmt.Sprintf("session %s (id %s)", h.session, h.sessionID)
	case h.session != "":
		who = "session " + h.session
	case h.sessionID != "":
		who = "session id " + h.sessionID
	}
	where := ""
	if h.workspace != "" {
		where = " on " + h.workspace
	}
	var doing string
	switch {
	case h.step == stepPreflight || h.step == "":
		doing = stepPreflight
	case h.current == 0:
		doing = fmt.Sprintf("running the unmutated baseline's %s, %d %s queued", h.step, h.mutants, textfmt.Plural(h.mutants, "mutant", "mutants"))
	default:
		doing = fmt.Sprintf("at mutant %d of %d, running its %s", h.current, h.mutants, h.step)
	}
	return fmt.Sprintf("held by %s%s, started %s ago, %s", who, where, humaniseAge(now.Sub(h.started)), doing)
}

// busyError is the refusal a second caller gets while the slot is held.
func (h mutationHolder) busyError(now time.Time) error {
	return fmt.Errorf("mutation_test: another mutation run is already in progress on this daemon — %s. "+
		"Concurrent runs would read each other's breakage as their own result, so this one is refused rather than queued. "+
		"Wait for it to finish and retry; if that session has gone, its run is cancelled as soon as its connection closes and the slot frees itself",
		h.describe(now))
}

// cancelOnDisconnect derives a ctx that is also cancelled when the connection
// that issued this call closes (mcp.ConnectionClosed). A run whose client has
// crashed or disconnected can never deliver its report, so carrying on would only
// hold the daemon-wide slot — for up to maxMutants × two maxMutationStepSeconds
// steps — against every other agent. Cancelling takes the ordinary cancellation
// path: the step in flight is killed, the file is restored, the slot released.
func cancelOnDisconnect(ctx context.Context) (context.Context, context.CancelFunc) {
	gone := mcp.ConnectionClosed(ctx)
	ctx, cancel := context.WithCancel(ctx)
	if gone == nil {
		return ctx, cancel
	}
	go func() {
		select {
		case <-gone:
			slog.Info("mutation_test: the owning connection closed mid-run; cancelling it and releasing the run slot")
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// WithSession wires the calling session's name and id, resolved per call so a
// logical agent on a shared connection is named rather than its connection. A
// refusal names the holder with them. Returns the receiver for chaining.
func (t *MutationTest) WithSession(nameFor, idFor func(ctx context.Context) string) *MutationTest {
	t.sessNameFor, t.sessIDFor = nameFor, idFor
	return t
}

// holder builds this call's holder record, before anything has run.
func (t *MutationTest) holder(ctx context.Context, mutants int) mutationHolder {
	h := mutationHolder{started: time.Now(), mutants: mutants, step: stepPreflight}
	if t.sessNameFor != nil {
		h.session = t.sessNameFor(ctx)
	}
	if t.sessIDFor != nil {
		h.sessionID = t.sessIDFor(ctx)
	}
	if t.deps.WorkspaceFn != nil {
		h.workspace = t.deps.WorkspaceFn(ctx)
	}
	return h
}

// skippedNote tells the reader of a cancelled run's report that it covers only
// the mutants that ran: without it a short report reads as a complete one.
func skippedNote(ran, total int) string {
	if ran >= total {
		return ""
	}
	return fmt.Sprintf("\n⚠ cancelled: %d of %d %s never ran — the request was cancelled, or its connection closed, before they started. They prove nothing either way.\n",
		total-ran, total, textfmt.Plural(total, "mutant", "mutants"))
}
