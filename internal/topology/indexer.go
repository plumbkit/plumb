package topology

import (
	"context"
	"database/sql"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	tsg "github.com/odvcencio/gotreesitter"
)

// The indexer is split across files by concern, all in package topology:
//   - indexer.go         — lifecycle, the queue, and the background worker loop
//   - indexer_extract.go — per-file path: read, hash, grammar cap, extract
//   - indexer_persist.go — SQLite persistence of nodes/edges within a tx
//   - indexer_resync.go  — whole-tree walk, pacing, prune, single-file delete

// Indexer manages background extraction and persistence for one workspace.
// It owns a queue channel and a single background goroutine.
//
// Concurrency: all exported methods are safe for concurrent use.
type Indexer struct {
	workspace   string
	db          *sql.DB
	extractors  []Extractor
	maxSize     int64
	resyncMins  int           // 0 disables periodic resync
	resyncBatch int           // files per pause during a full resync; 0 disables pacing
	resyncPause time.Duration // pause between resync batches; 0 disables pacing

	// excludePatterns is [topology] exclude_patterns, already sanitised by
	// sanitizeExcludePatterns. It excludes paths ON TOP OF shouldSkipDir and the
	// tree's own .gitignore / .ignore files, and is the only one of the three
	// that can reach a tree the repository deliberately tracks. Read by the
	// resync walk; set once before Start and not mutated afterwards.
	excludePatterns []string

	// extractTimeout caps one file's parse. The size gates bound how much source
	// a grammar sees, not how long it spends on it — a pathological error-recovery
	// path can burn tens of seconds on a file well inside maxSize — and the worker
	// below is a single goroutine, so one such file stalls the whole index. This
	// may lower the maxExtractTimeout ceiling but not remove it, so 0 resolves to
	// the ceiling rather than to "unbounded" — see effectiveExtractTimeout.
	extractTimeout time.Duration

	queue chan indexOp
	done  chan struct{}
	wg    sync.WaitGroup

	// passCtx is cancelled by Stop so a derived-edge rebuild can abandon its
	// transaction instead of making wg.Wait() sit out a full pass — the cost
	// `plumb restart` used to pay. It is deliberately NOT threaded into per-file
	// dispatch: processUpsert records a file as errored when extraction fails,
	// so a shutdown cancel there would write an error row for every in-flight
	// file. See the note in runQueueCycle.
	passCtx    context.Context
	passCancel context.CancelFunc

	// forceFullRebuild makes the next derived-edge rebuild wholesale. It is set
	// when a pass fails partway, so a cycle that got one of the two passes
	// committed cannot leave a scoped rebuild reasoning from a half-updated
	// graph. Only the single background worker goroutine touches it.
	forceFullRebuild bool

	// idleReclaim is the debounce delay before draining the parse-arena pool once
	// the queue goes quiet. A steady trickle of single-file edits never trips the
	// per-cycle burst gate (shouldReclaimAfterBurst), so without this the pooled
	// high-water-mark arenas would sit resident on an otherwise idle daemon. 0
	// disables idle reclamation.
	idleReclaim time.Duration
	// reclaimFn releases pooled parse arenas to the GC and returns freed pages to
	// the OS. A struct field so tests can observe it; production uses drainArenas.
	reclaimFn func()

	// retryBase and retryMax bound the failure-retry backoff: a cycle that ends
	// in error schedules a full resync after retryBase, doubling per consecutive
	// failure up to retryMax, until a cycle succeeds. Struct fields so tests can
	// shorten them; 0 retryBase disables the retry.
	retryBase time.Duration
	retryMax  time.Duration

	mu            sync.RWMutex
	state         string
	lastSync      time.Time
	lastErr       string
	failing       bool // a cycle failed and no full resync has succeeded since
	resyncPending bool // set when Enqueue overflows; triggers a recovery resync
}

// defaultIdleReclaim is the debounce window after the indexer goes quiet before
// it drains the pooled parse arenas. Long enough that an active edit loop's
// brief pauses do not each pay a stop-the-world GC, short enough that a daemon
// left idle settles back to its lean resident set promptly.
const defaultIdleReclaim = 30 * time.Second

// defaultRetryBase and defaultRetryMax bound the failure-retry backoff. The
// first retry comes quickly enough that a transient error clears itself before
// an agent notices; the cap keeps a persistent one (a damaged database) from
// costing more than one full resync per half hour.
const (
	defaultRetryBase = 30 * time.Second
	defaultRetryMax  = 30 * time.Minute
)

// drainArenas releases gotreesitter's pooled parse arenas to the GC and hands the
// freed pages back to the OS. The arena pools are package-global strong-reference
// free-lists, so without an explicit drain a single large parse leaves a
// high-water-mark arena (tens of MB) resident until the process exits.
func drainArenas() {
	tsg.DrainArenaPools()
	debug.FreeOSMemory()
}

// newIndexer creates an Indexer. Call Start() before enqueuing operations.
// resyncMins controls the optional periodic full-resync interval; 0 disables it.
func newIndexer(workspace string, db *sql.DB, exts []Extractor, maxSize int64, resyncMins int) *Indexer {
	if maxSize <= 0 {
		maxSize = 512 * 1024
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Indexer{
		workspace:   workspace,
		db:          db,
		extractors:  exts,
		maxSize:     maxSize,
		resyncMins:  resyncMins,
		idleReclaim: defaultIdleReclaim,
		reclaimFn:   drainArenas,
		retryBase:   defaultRetryBase,
		retryMax:    defaultRetryMax,
		queue:       make(chan indexOp, 256),
		done:        make(chan struct{}),
		state:       "idle",
		passCtx:     ctx,
		passCancel:  cancel,
	}
}

// Start launches the background worker and enqueues an initial full resync.
func (idx *Indexer) Start() {
	idx.wg.Go(func() {
		idx.backgroundWorker()
	})
	idx.Enqueue("", opResync)
}

// Stop signals the background worker to exit and waits for it to drain its
// current operation before returning. The wg.Wait() ensures any in-progress
// transaction completes before the caller may close the database.
// Safe to call more than once; subsequent calls are no-ops.
func (idx *Indexer) Stop() {
	select {
	case <-idx.done:
	default:
		close(idx.done)
	}
	// Cancel before waiting: a derived-edge rebuild in flight aborts its
	// transaction rather than running to completion under wg.Wait().
	idx.passCancel()
	idx.wg.Wait()
}

// Enqueue adds a file operation to the background queue. Non-blocking; drops
// silently if the queue is full (capacity 256 is generous for typical usage).
func (idx *Indexer) Enqueue(path string, kind opKind) {
	select {
	case idx.queue <- indexOp{kind: kind, path: path}:
	default:
		// Queue full: rather than silently lose this change, flag a full resync
		// so the next worker cycle reconciles the whole tree and the index does
		// not drift out of sync with the filesystem.
		idx.mu.Lock()
		idx.resyncPending = true
		idx.mu.Unlock()
	}
}

// State returns the current indexer state string.
func (idx *Indexer) State() string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.state
}

// LastSync returns the time of the most recent completed resync.
func (idx *Indexer) LastSync() time.Time {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.lastSync
}

// LastError returns the most recent indexing error, or "".
func (idx *Indexer) LastError() string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.lastErr
}

// Health returns the indexer's liveness in one consistent read.
func (idx *Indexer) Health() Health {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return Health{State: idx.state, LastSync: idx.lastSync, LastError: idx.lastErr, Failing: idx.failing}
}

// setState records a state transition. failing follows only the states that
// end a cycle: "running" leaves it alone, so a cycle in flight after a failure
// still reports the index as failing. runQueueCycle reports "idle" after a
// failure only from a cycle that completed a full resync.
func (idx *Indexer) setState(state, errMsg string) {
	idx.mu.Lock()
	idx.state = state
	if errMsg != "" {
		idx.lastErr = errMsg
	}
	switch state {
	case "idle":
		idx.lastSync = time.Now()
		idx.failing = false
	case "error":
		idx.failing = true
	}
	idx.mu.Unlock()
}

func (idx *Indexer) backgroundWorker() {
	// Set up an optional periodic-resync ticker. A nil channel blocks forever,
	// so the select case is never chosen when resync is disabled.
	var tickC <-chan time.Time
	if idx.resyncMins > 0 {
		ticker := time.NewTicker(time.Duration(idx.resyncMins) * time.Minute)
		defer ticker.Stop()
		tickC = ticker.C
	}

	// Idle-reclaim timer: once the queue goes quiet for idleReclaim, drain the
	// pooled parse arenas so a steady trickle of single-file edits — which never
	// trips the per-cycle burst gate — cannot leave high-water-mark arenas
	// resident on an otherwise idle daemon. Created stopped; armed only after a
	// queue cycle that did not already reclaim.
	idleTimer := time.NewTimer(time.Hour)
	idleTimer.Stop()
	defer idleTimer.Stop()
	reclaimPending := false

	// Failure-retry timer: a cycle that ends in error arms it, and it re-runs a
	// full resync with exponential backoff until one succeeds. Nothing else
	// retries a failed cycle: the periodic tick below runs only from idle and is
	// off entirely under the file watcher, so a failed derived-edge rebuild left
	// the index in error until the next file event — on a quiet workspace,
	// indefinitely. Created stopped.
	retryTimer := time.NewTimer(time.Hour)
	retryTimer.Stop()
	defer retryTimer.Stop()
	var retry retryBackoff

	for {
		select {
		case <-idx.done:
			return
		case <-tickC:
			// Only enqueue when idle — don't pile up resyncs if a previous one
			// is still running.
			if idx.State() == "idle" {
				idx.Enqueue("", opResync)
			}
		case op := <-idx.queue:
			if idx.runQueueCycle(op) {
				// The burst gate already reclaimed; cancel any pending idle drain.
				reclaimPending = false
				idleTimer.Stop()
			} else if idx.idleReclaim > 0 {
				reclaimPending = true
				idleTimer.Reset(idx.idleReclaim)
			}
			idx.scheduleRetry(retryTimer, &retry)
		case <-idleTimer.C:
			if reclaimPending {
				idx.reclaimFn()
				reclaimPending = false
			}
		case <-retryTimer.C:
			retry.armed = false
			if idx.State() == "error" {
				idx.Enqueue("", opResync)
			}
		}
	}
}

// retryBackoff is the worker's failure-retry bookkeeping. Only the single
// background worker goroutine touches it.
type retryBackoff struct {
	delay time.Duration // the delay last armed; the next retry failure doubles it
	armed bool          // the timer is pending
}

// scheduleRetry arms or disarms the failure-retry timer after a cycle. A cycle
// that did not end in error stops the timer and resets the backoff. A failed
// cycle arms it with the next delay unless a retry is already pending: only a
// failed retry escalates the backoff, so a persistent error costs one full
// resync per retryMax at most, while a burst of failing file events during a
// brief fault neither escalates it nor pushes the pending retry out.
func (idx *Indexer) scheduleRetry(timer *time.Timer, r *retryBackoff) {
	if idx.State() != "error" || idx.retryBase <= 0 {
		timer.Stop()
		*r = retryBackoff{}
		return
	}
	if r.armed {
		return
	}
	r.delay = nextRetryDelay(r.delay, idx.retryBase, idx.retryMax)
	r.armed = true
	timer.Reset(r.delay)
}

// nextRetryDelay doubles prev, starting from base and capped at limit. A zero
// or negative limit leaves the delay uncapped.
func nextRetryDelay(prev, base, limit time.Duration) time.Duration {
	next := base
	if prev > 0 {
		next = prev * 2
	}
	if limit > 0 && next > limit {
		next = limit
	}
	return next
}

// runQueueCycle drains and processes all buffered ops, then runs a full resync
// when one was flagged by Enqueue after a queue overflow, so dropped per-file
// updates cannot leave the index permanently stale. The indexer state is set to
// error or idle based on the combined outcome. It reports whether it reclaimed
// the parse-arena pool — true after a large enough burst or a successful resync
// (both drain the pool) — so the caller can cancel a pending idle drain.
func (idx *Indexer) runQueueCycle(initial indexOp) bool {
	ops := idx.drain(initial)
	idx.setState("running", "")
	// processOps reports a resync's reclaim only when the resync succeeded, so
	// its flag doubles as "a full resync completed this cycle".
	changes, resynced, lastErr := idx.processOps(ops)
	if idx.takeResyncPending() {
		changed, err := idx.processResyncChanged(context.Background())
		if err != nil {
			slog.Warn("topology: recovery resync error", "err", err)
			lastErr = err
		} else {
			resynced = true
			if changed {
				changes.markFull()
			}
		}
	}
	// Cross-file edges are resolved after the batch, not during it: an import can
	// only be linked to a package once that package has been indexed, and a
	// call's target may live in a file indexed later in this very batch, so
	// nothing per-file can see either. Running it per drain rather than per file
	// also collapses a checkout-sized burst into one pass.
	if err := idx.rebuildDerived(changes); err != nil {
		slog.Warn("topology: derived edge rebuild error", "err", err)
		lastErr = err
	}
	switch {
	case lastErr != nil:
		idx.setState("error", lastErr.Error())
	case idx.Health().Failing && !resynced:
		// An earlier cycle failed and this one succeeded without a full resync,
		// so whatever the failed cycle was indexing may still be missing: an event
		// for some other file must not declare the index healthy. The fault has
		// evidently cleared, so catch up now rather than at the next retry, and
		// stay failing until that resync succeeds.
		idx.setState("running", "")
		idx.Enqueue("", opResync)
	default:
		idx.setState("idle", "")
	}
	// A successful resync drained the arena pool as its final step.
	reclaimed := resynced
	if shouldReclaimAfterBurst(len(ops)) {
		// A coalesced burst (git checkout, a formatter) left a large transient
		// parse working set. A single small edit must NOT pay a stop-the-world GC,
		// so this is gated on the burst size; the trickle case is covered by the
		// idle-reclaim timer in backgroundWorker instead.
		idx.reclaimFn()
		reclaimed = true
	}
	return reclaimed
}

// processOps runs one drain's operations and reports what they actually changed.
//
// "Actually changed" is narrower than "was asked to do": an editor that rewrites
// a file with identical bytes, or a resync revisiting an unchanged tree, reaches
// here as work and leaves the graph untouched. Distinguishing the two is what
// lets rebuildDerived skip entirely, which is the single biggest saving of the
// derived-edge lifecycle — a save that changes nothing now costs nothing.
func (idx *Indexer) processOps(ops []indexOp) (indexChanges, bool, error) {
	var changes indexChanges
	reclaimed := false
	var lastErr error
	for _, o := range ops {
		// Background() is deliberate: the per-file extract timeout wraps the
		// parse itself (extractFile), so it does not need an upstream deadline,
		// and under a parent cancel every remaining op in the drain would fail
		// individually (the loop continues past errors). If a cancellable
		// parent is ever threaded through here (and into processResync above),
		// processUpsert — the caller that decides whether to record — must
		// first distinguish errors.Is(err, context.Canceled) (the worker is
		// stopping; the file did not earn an error row) from
		// context.DeadlineExceeded, which it did; otherwise every in-flight
		// file is recorded as an error on shutdown.
		changed, err := idx.dispatch(context.Background(), o)
		if err != nil {
			slog.Warn("topology: indexer error", "op", o.kind, "path", o.path, "err", err)
			lastErr = err
			continue
		}
		if o.kind == opResync {
			// Credit the reclaim only when the resync SUCCEEDED — processResync
			// drains the pool as its final step, so a walk/prune failure means no
			// drain ran. Crediting on intent would still cancel the idle-reclaim
			// backstop, narrowly re-opening the retention this guards against.
			reclaimed = true
		}
		if !changed {
			continue
		}
		if o.kind == opResync {
			changes.markFull()
		} else {
			changes.mark(o.path)
		}
	}
	return changes, reclaimed, lastErr
}

// rebuildDerived brings the derived cross-file edges back into agreement with
// what this cycle changed.
//
// forceFullRebuild is raised BEFORE the passes run and lowered only once both
// have committed and the fingerprint is stored. A cycle that commits the import
// pass and then fails the call pass therefore leaves the next cycle no choice
// but a wholesale rebuild, rather than letting a scoped rebuild reason from a
// graph only half of which was updated.
func (idx *Indexer) rebuildDerived(changes indexChanges) error {
	if idx.passCtx.Err() != nil {
		// Shutting down. The next Start() enqueues a full resync, and the missing
		// fingerprint update forces that cycle to rebuild wholesale, so nothing
		// is lost by not running here.
		return nil
	}
	mode, fingerprint, err := idx.planRebuild(idx.passCtx, changes)
	if err != nil {
		idx.forceFullRebuild = true
		return err
	}
	if mode == rebuildSkip {
		return nil
	}
	idx.forceFullRebuild = true
	if err := idx.linkImportsContext(idx.passCtx, mode, changes); err != nil {
		return err
	}
	if err := idx.resolveCallsContext(idx.passCtx, mode, changes); err != nil {
		return err
	}
	if err := writeMeta(idx.passCtx, idx.db, fingerprint); err != nil {
		return err
	}
	idx.forceFullRebuild = false
	return nil
}

// reclaimAfterOps is the burst size at which runQueueCycle reclaims transient
// parse memory. Below it the cost of a forced GC + FreeOSMemory outweighs the
// at-most-one pooled arena a small edit leaves behind.
const reclaimAfterOps = 64

// shouldReclaimAfterBurst reports whether a queue cycle processed enough files to
// warrant draining the parse-arena pool and returning pages to the OS.
func shouldReclaimAfterBurst(n int) bool {
	return n >= reclaimAfterOps
}

// takeResyncPending atomically reads and clears the pending-resync flag.
func (idx *Indexer) takeResyncPending() bool {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.resyncPending {
		idx.resyncPending = false
		return true
	}
	return false
}

// drain coalesces all buffered ops into a slice keeping the last op per unique
// path. This ensures every distinct file gets processed, while still collapsing
// rapid successive writes to the same file into a single operation.
func (idx *Indexer) drain(initial indexOp) []indexOp {
	seen := map[string]indexOp{initial.path: initial}
	for {
		select {
		case op := <-idx.queue:
			seen[op.path] = op // last write per path wins
		default:
			ops := make([]indexOp, 0, len(seen))
			for _, op := range seen {
				ops = append(ops, op)
			}
			return ops
		}
	}
}

// dispatch runs one operation and reports whether it changed any indexed rows.
func (idx *Indexer) dispatch(ctx context.Context, op indexOp) (bool, error) {
	switch op.kind {
	case opUpsert:
		return idx.processUpsertChanged(ctx, op.path)
	case opDelete:
		return idx.processDeleteChanged(ctx, op.path)
	default:
		return idx.processResyncChanged(ctx)
	}
}
