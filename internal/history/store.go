package history

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/plumbkit/plumb/internal/sqlitex"
)

// Options tunes a Store; zero values take the spec defaults.
type Options struct {
	QueueSize      int           // 1024
	OverflowCap    int           // 8192
	BatchSize      int           // 64
	MaxQueuedBytes int64         // 256 MiB
	BatchWait      time.Duration // 250 ms
	MaxDiffBytes   func() int64  // per batch; nil → 4 MiB
	// BusyTimeout is the writer's wait for another process's lock (a running
	// `plumb history prune`). Zero takes the sqlitex default; tests shorten it.
	BusyTimeout time.Duration
}

// closeGrace bounds how long Close waits, after its own deadline, for a writer
// that degraded to markers but is still stuck on another process's lock.
const closeGrace = 5 * time.Second

func (o Options) withDefaults() Options {
	if o.QueueSize <= 0 {
		o.QueueSize = 1024
	}
	if o.OverflowCap <= 0 {
		o.OverflowCap = 8192
	}
	if o.BatchSize <= 0 {
		o.BatchSize = 64
	}
	if o.MaxQueuedBytes <= 0 {
		o.MaxQueuedBytes = 256 << 20
	}
	if o.BatchWait <= 0 {
		o.BatchWait = 250 * time.Millisecond
	}
	if o.MaxDiffBytes == nil {
		o.MaxDiffBytes = func() int64 { return 4 << 20 }
	}
	return o
}

// Store owns history.db and its single writer goroutine.
//
// Concurrency: Enqueue, Sync, Close and Enqueued are safe for concurrent use.
// Enqueue never blocks (a non-blocking send under mu, O(1)), so it is safe to
// call while holding a per-path write lock. Only the writer goroutine touches
// db for writes.
type Store struct {
	path string
	db   *sql.DB
	opts Options
	enc  *zstd.Encoder

	ch      chan Item
	syncReq chan chan struct{}
	done    chan struct{}
	pause   chan struct{} // test hook; nil in production

	mu          sync.Mutex // guards the fields below
	closed      bool
	overflow    []Item
	queuedBytes int64
	dropped     int64 // since the last batch persisted it
	lastDrop    time.Time
	errsPending int64 // insert errors a failed batch could not persist yet

	enqueued atomic.Int64
	degrade  atomic.Bool // set by Close past its deadline: write markers only

	link     map[string]int64 // writer-only: callID\x00path → latest seq
	linkKeys []string         // writer-only FIFO bound for link
}

// Open opens (creating as needed) history.db at path and starts the writer.
func Open(path string, opts Options) (*Store, error) {
	o := opts.withDefaults()
	db, err := sqlitex.Open(path, sqlitex.Options{Sync: sqlitex.SyncNormal, MaxOpenConns: 1, BusyTimeout: o.BusyTimeout})
	if err != nil {
		return nil, fmt.Errorf("history: open %s: %w", path, err)
	}
	if err := initSchema(db); err != nil {
		db.Close()
		return nil, err
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("history: zstd encoder: %w", err)
	}
	s := &Store{
		path:    path,
		db:      db,
		opts:    o,
		enc:     enc,
		ch:      make(chan Item, o.QueueSize),
		syncReq: make(chan chan struct{}),
		done:    make(chan struct{}),
		link:    make(map[string]int64),
	}
	go s.run()
	return s, nil
}

// Enqueued counts Enqueue calls (the "disabled costs nothing" test's counter).
func (s *Store) Enqueued() int64 { return s.enqueued.Load() }

// Enqueue hands it to the writer without blocking. Order is FIFO: while the
// overflow list is non-empty every new item joins it, and the writer drains the
// channel before the overflow list. A full channel (or the queued-bytes cap)
// downgrades the item to a metadata-only marker — shas kept, so the sha chain
// stays intact; past the overflow cap the row is dropped and counted.
func (s *Store) Enqueue(it Item) {
	s.enqueued.Add(1)
	// Prepare has normally settled these already; an item queued without it
	// is settled here, outside mu.
	it.Before, it.After = settled(it.Before), settled(it.After)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if len(s.overflow) == 0 && s.queuedBytes+it.contentBytes() <= s.opts.MaxQueuedBytes {
		select {
		case s.ch <- it:
			s.queuedBytes += it.contentBytes()
			return
		default:
		}
	}
	if len(s.overflow) >= s.opts.OverflowCap {
		s.dropped++
		s.lastDrop = time.Now()
		return
	}
	s.overflow = append(s.overflow, marker(it))
}

func marker(it Item) Item {
	// Settle first: the hash must outlive the content it is computed from.
	it.Before, it.After = settled(it.Before), settled(it.After)
	it.Before.Content, it.After.Content = nil, nil
	it.Content, it.Added, it.Removed = ContentOverflow, 0, 0
	return it
}

// takeQueued appends everything waiting — the channel's buffered items, then
// the overflow list — to batch, atomically. Enqueue sends under mu, so holding
// mu across both steps closes the window in which an item could reach the
// channel after the drain but before the overflow is taken, and be written
// AFTER an overflow item that is younger than it.
func (s *Store) takeQueued(batch []Item) []Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		select {
		case it, ok := <-s.ch:
			if !ok {
				batch = append(batch, s.overflow...)
				s.overflow = nil
				return batch
			}
			batch = append(batch, it)
		default:
			batch = append(batch, s.overflow...)
			s.overflow = nil
			return batch
		}
	}
}

func (s *Store) release(items []Item) {
	var n int64
	for _, it := range items {
		n += it.contentBytes()
	}
	s.mu.Lock()
	s.queuedBytes = max(0, s.queuedBytes-n)
	s.mu.Unlock()
}

// takeCounts hands the writer the drop and error counts it must persist with
// its next batch, and clears them; giveBack returns them (plus anything the
// batch itself lost) when that batch could not persist them, so no count is
// ever zeroed without reaching meta.
func (s *Store) takeCounts() (drops int64, dropAt time.Time, errs int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	drops, dropAt, errs = s.dropped, s.lastDrop, s.errsPending
	s.dropped, s.errsPending = 0, 0
	return drops, dropAt, errs
}

func (s *Store) giveBack(drops int64, dropAt time.Time, errs int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropped += drops
	s.errsPending += errs
	if drops > 0 && dropAt.After(s.lastDrop) {
		s.lastDrop = dropAt
	}
}

func (s *Store) hasPendingCounts() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped > 0 || s.errsPending > 0
}

// Sync blocks until everything enqueued before the call is committed. For
// tests and the CLI harness — never call it from a write tool.
func (s *Store) Sync(ctx context.Context) error {
	reply := make(chan struct{})
	select {
	case s.syncReq <- reply:
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-reply:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops intake and drains the queue. Past ctx's deadline the writer
// degrades to metadata-only markers so the drain finishes quickly; then the
// WAL is checkpointed and the database closed. Idempotent.
func (s *Store) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.ch)
	s.mu.Unlock()
	select {
	case <-s.done:
	case <-ctx.Done():
		s.degrade.Store(true)
		// Markers are cheap, but a writer stuck on another process's lock is
		// not: bound the wait so a daemon shutdown is never held hostage. The
		// database is left open for the stuck writer rather than closed under it.
		select {
		case <-s.done:
		case <-time.After(closeGrace):
			slog.Warn("history: writer still busy after the close grace; leaving history.db to it", "grace", closeGrace)
			return ctx.Err()
		}
	}
	_, _ = s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	_ = s.enc.Close()
	return s.db.Close()
}

// pauseWriter/resumeWriter are test hooks: they hold the writer before its next
// receive so a test can fill the queue deterministically.
func (s *Store) pauseWriter()  { s.mu.Lock(); s.pause = make(chan struct{}); s.mu.Unlock() }
func (s *Store) resumeWriter() { s.mu.Lock(); close(s.pause); s.pause = nil; s.mu.Unlock() }

func (s *Store) waitIfPaused() {
	s.mu.Lock()
	p := s.pause
	s.mu.Unlock()
	if p != nil {
		<-p
	}
}
