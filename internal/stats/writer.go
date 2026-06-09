package stats

import (
	"log/slog"
	"sync/atomic"
	"time"
)

const (
	// writerBufferSize bounds queued-but-unwritten commands. A sustained storm
	// that outpaces SQLite drops the overflow (counted) rather than blocking the
	// caller's response path.
	writerBufferSize = 2048
	// writerMaxBatch caps how many inserts ride in one transaction.
	writerMaxBatch = 256
	// writerFlushPeriod is the longest a buffered insert waits before commit.
	writerFlushPeriod = 200 * time.Millisecond
	// checkpointEvery counts flush ticks between WAL truncations (~30 s at the
	// flush period) so the WAL cannot grow unbounded between autocheckpoints.
	checkpointEvery = 150
)

// writerCommand is one unit of work for the writer goroutine: either an insert
// or a session rename. Exactly one field is non-nil.
type writerCommand struct {
	call   *Call
	rename *renameCommand
}

type renameCommand struct {
	sessionID string
	name      string
}

// Writer is the single owner of the read-write stats database. Every write
// funnels through one goroutine that batches inserts into transactions, so the
// writer never contends with itself — the cause of the SQLITE_BUSY storm under
// concurrent sessions.
//
// Concurrency: Record and RenameSession are safe for concurrent use and never
// block on SQLite (they enqueue onto a buffered channel). Close is called once
// at shutdown and drains the queue before returning.
type Writer struct {
	db   *DB
	ch   chan writerCommand
	stop chan struct{}
	done chan struct{}

	closed  atomic.Bool
	dropped atomic.Int64
	// loggedDropped is touched only by the writer goroutine (logDropped); it
	// tracks how many drops have already been reported so the warning logs the
	// new delta rather than re-counting.
	loggedDropped int64
}

// NewWriter opens the global stats database read-write and starts the writer
// goroutine. The caller owns the returned Writer and must Close it.
func NewWriter() (*Writer, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	w := newWriter(db, writerBufferSize)
	go w.run()
	return w, nil
}

// newWriter builds a Writer around an open DB without starting its goroutine.
// NewWriter is the production entry point; tests use this to size the buffer
// and exercise the drop path deterministically.
func newWriter(db *DB, buffer int) *Writer {
	return &Writer{
		db:   db,
		ch:   make(chan writerCommand, buffer),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
}

// Record enqueues a call for batched insertion. Non-blocking: a full buffer
// drops the call and increments the drop counter rather than stalling.
func (w *Writer) Record(c Call) {
	if w == nil || w.closed.Load() {
		return
	}
	select {
	case w.ch <- writerCommand{call: &c}:
	default:
		w.dropped.Add(1)
	}
}

// RenameSession enqueues a session-name backfill, ordered behind any pending
// inserts so it names rows that are already stored.
func (w *Writer) RenameSession(sessionID, name string) {
	if w == nil || w.closed.Load() || sessionID == "" {
		return
	}
	select {
	case w.ch <- writerCommand{rename: &renameCommand{sessionID: sessionID, name: name}}:
	default:
		w.dropped.Add(1)
	}
}

// Dropped reports the cumulative number of commands dropped because the buffer
// was full. Exposed for tests and diagnostics.
func (w *Writer) Dropped() int64 {
	if w == nil {
		return 0
	}
	return w.dropped.Load()
}

// Close stops the writer, drains and flushes buffered work, truncates the WAL,
// and closes the database. Idempotent.
func (w *Writer) Close() {
	if w == nil || w.closed.Swap(true) {
		return
	}
	close(w.stop)
	<-w.done
	w.db.checkpoint()
	w.db.Close()
}

// run accumulates inserts and commits them in batches — on the size threshold
// or the periodic tick — until Close signals stop, then drains and returns.
func (w *Writer) run() {
	defer close(w.done)
	ticker := time.NewTicker(writerFlushPeriod)
	defer ticker.Stop()

	batch := make([]Call, 0, writerMaxBatch)
	ticks := 0
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if _, err := w.db.RecordBatch(batch); err != nil {
			slog.Warn("stats: batch insert failed", "n", len(batch), "err", err)
		}
		batch = batch[:0]
	}
	apply := func(cmd writerCommand) {
		switch {
		case cmd.call != nil:
			batch = append(batch, *cmd.call)
			if len(batch) >= writerMaxBatch {
				flush()
			}
		case cmd.rename != nil:
			flush() // name rows that are already inserted
			if err := w.db.RenameSession(cmd.rename.sessionID, cmd.rename.name); err != nil {
				slog.Warn("stats: rename session failed", "session", cmd.rename.sessionID, "err", err)
			}
		}
	}

	for {
		select {
		case cmd := <-w.ch:
			apply(cmd)
		case <-ticker.C:
			flush()
			if ticks++; ticks >= checkpointEvery {
				w.db.checkpoint()
				ticks = 0
			}
			w.logDropped()
		case <-w.stop:
			w.drain(apply, flush)
			return
		}
	}
}

// drain consumes every buffered command without blocking, then flushes — the
// shutdown path, so a clean Close never loses queued stats.
func (w *Writer) drain(apply func(writerCommand), flush func()) {
	for {
		select {
		case cmd := <-w.ch:
			apply(cmd)
		default:
			flush()
			return
		}
	}
}

// logDropped emits a single warning carrying the newly-dropped count, so a
// sustained overflow is visible without one log line per dropped row.
func (w *Writer) logDropped() {
	if total := w.dropped.Load(); total > w.loggedDropped {
		slog.Warn("stats: dropped calls — write buffer full", "dropped", total-w.loggedDropped, "total", total)
		w.loggedDropped = total
	}
}
