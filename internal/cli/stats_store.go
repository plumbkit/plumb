package cli

import (
	"log/slog"
	"sync"

	"github.com/plumbkit/plumb/internal/stats"
)

// statsStore owns the global stats writer, opened lazily on first write. The
// writer batches inserts through a single goroutine (see stats.Writer), so
// concurrent sessions never contend for the SQLite write lock.
//
// Concurrency: safe for concurrent Record / RenameSession / Close.
type statsStore struct {
	mu     sync.Mutex
	writer *stats.Writer
	closed bool
	failed bool // NewWriter failed once; don't retry-spam the log
}

func newStatsStore() *statsStore {
	return &statsStore{}
}

// writer returns the lazily-opened stats writer, or nil when the store is
// closed or the database could not be opened.
func (s *statsStore) ensureWriter() *stats.Writer {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.failed {
		return nil
	}
	if s.writer == nil {
		w, err := stats.NewWriter()
		if err != nil {
			s.failed = true
			slog.Warn("stats: cannot open global DB", "err", err)
			return nil
		}
		s.writer = w
	}
	return s.writer
}

// Record enqueues one call to the global stats writer, opening it on first use.
// Non-blocking — the MCP response path must never wait on stats SQLite.
func (s *statsStore) Record(workspace string, call stats.Call) {
	if s == nil {
		return
	}
	w := s.ensureWriter()
	if w == nil {
		return
	}
	call.Workspace = workspace
	w.Record(call)
}

// RenameSession backfills the display name for the global stats DB.
func (s *statsStore) RenameSession(sessionID, name string) {
	if s == nil || sessionID == "" {
		return
	}
	w := s.ensureWriter()
	if w == nil {
		return
	}
	w.RenameSession(sessionID, name)
}

// RecordEpisodic enqueues a generated episodic summary to the global stats DB.
func (s *statsStore) RecordEpisodic(e stats.Episodic) {
	if s == nil || e.Workspace == "" {
		return
	}
	if w := s.ensureWriter(); w != nil {
		w.RecordEpisodic(e)
	}
}

// Close drains and flushes in-flight writes, then shuts the writer. Intended to
// be called once at daemon shutdown.
func (s *statsStore) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	w := s.writer
	s.writer = nil
	s.mu.Unlock()
	if w != nil {
		w.Close()
	}
}
