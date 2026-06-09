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

	queue chan indexOp
	done  chan struct{}
	wg    sync.WaitGroup

	mu            sync.RWMutex
	state         string
	lastSync      time.Time
	lastErr       string
	resyncPending bool // set when Enqueue overflows; triggers a recovery resync
}

// newIndexer creates an Indexer. Call Start() before enqueuing operations.
// resyncMins controls the optional periodic full-resync interval; 0 disables it.
func newIndexer(workspace string, db *sql.DB, exts []Extractor, maxSize int64, resyncMins int) *Indexer {
	if maxSize <= 0 {
		maxSize = 512 * 1024
	}
	return &Indexer{
		workspace:  workspace,
		db:         db,
		extractors: exts,
		maxSize:    maxSize,
		resyncMins: resyncMins,
		queue:      make(chan indexOp, 256),
		done:       make(chan struct{}),
		state:      "idle",
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

func (idx *Indexer) setState(state, errMsg string) {
	idx.mu.Lock()
	idx.state = state
	if errMsg != "" {
		idx.lastErr = errMsg
	}
	if state == "idle" {
		idx.lastSync = time.Now()
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
			idx.runQueueCycle(op)
		}
	}
}

// runQueueCycle drains and processes all buffered ops, then runs a full resync
// when one was flagged by Enqueue after a queue overflow, so dropped per-file
// updates cannot leave the index permanently stale. The indexer state is set to
// error or idle based on the combined outcome.
func (idx *Indexer) runQueueCycle(initial indexOp) {
	ops := idx.drain(initial)
	idx.setState("running", "")
	var lastErr error
	for _, o := range ops {
		if err := idx.dispatch(context.Background(), o); err != nil {
			slog.Warn("topology: indexer error", "op", o.kind, "path", o.path, "err", err)
			lastErr = err
		}
	}
	if idx.takeResyncPending() {
		if err := idx.processResync(context.Background()); err != nil {
			slog.Warn("topology: recovery resync error", "err", err)
			lastErr = err
		}
	}
	if lastErr != nil {
		idx.setState("error", lastErr.Error())
	} else {
		idx.setState("idle", "")
	}
	if shouldReclaimAfterBurst(len(ops)) {
		// A coalesced burst (git checkout, a formatter) left a large transient
		// parse working set. A single small edit must NOT pay a stop-the-world GC,
		// so this is gated on the burst size.
		tsg.DrainArenaPools()
		debug.FreeOSMemory()
	}
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

func (idx *Indexer) dispatch(ctx context.Context, op indexOp) error {
	switch op.kind {
	case opUpsert:
		return idx.processUpsert(ctx, op.path)
	case opDelete:
		return idx.processDelete(ctx, op.path)
	default:
		return idx.processResync(ctx)
	}
}
