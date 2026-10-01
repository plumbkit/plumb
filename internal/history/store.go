package history

import (
	"context"
	"database/sql"
	"fmt"
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
}

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

	enqueued atomic.Int64
	degrade  atomic.Bool // set by Close past its deadline: write markers only

	link     map[string]int64 // writer-only: callID\x00path → latest seq
	linkKeys []string         // writer-only FIFO bound for link
}

// Open opens (creating as needed) history.db at path and starts the writer.
func Open(path string, opts Options) (*Store, error) {
	db, err := sqlitex.Open(path, sqlitex.Options{Sync: sqlitex.SyncNormal, MaxOpenConns: 1})
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
	o := opts.withDefaults()
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
	it.Before.Content, it.After.Content = nil, nil
	it.Content, it.Added, it.Removed = ContentOverflow, 0, 0
	return it
}

func (s *Store) takeOverflow() []Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.overflow
	s.overflow = nil
	return out
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

func (s *Store) takeDrops() (int64, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, at := s.dropped, s.lastDrop
	s.dropped = 0
	return n, at
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
		<-s.done
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
