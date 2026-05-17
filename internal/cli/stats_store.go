package cli

import (
	"log/slog"
	"sync"

	"github.com/golimpio/plumb/internal/stats"
)

// statsStore owns the global stats database, opened lazily on first write.
//
// Concurrency: safe for concurrent Record / RenameSession / Close.
type statsStore struct {
	mu  sync.Mutex
	dbs map[string]*stats.DB
}

func newStatsStore() *statsStore {
	return &statsStore{dbs: make(map[string]*stats.DB)}
}

// Record writes one call to the global stats DB, opening it on first use.
func (s *statsStore) Record(workspace string, call stats.Call) {
	if s == nil {
		return
	}
	s.mu.Lock()
	db, ok := s.dbs["global"]
	if !ok {
		var err error
		db, err = stats.Open()
		if err != nil {
			s.mu.Unlock()
			slog.Warn("stats: cannot open global DB", "err", err)
			return
		}
		s.dbs["global"] = db
	}
	s.mu.Unlock()
	call.Workspace = workspace
	if err := db.Record(call); err != nil {
		slog.Warn("stats: cannot record tool call",
			"workspace", workspace,
			"session", call.SessionID,
			"tool", call.Tool,
			"err", err)
	}
}

// RenameSession backfills the display name for the global stats DB.
func (s *statsStore) RenameSession(sessionID, name string) {
	if s == nil || sessionID == "" {
		return
	}
	s.mu.Lock()
	db, ok := s.dbs["global"]
	if !ok {
		var err error
		db, err = stats.Open()
		if err != nil {
			s.mu.Unlock()
			slog.Warn("stats: cannot open global DB for rename", "err", err)
			return
		}
		s.dbs["global"] = db
	}
	s.mu.Unlock()
	if err := db.RenameSession(sessionID, name); err != nil {
		slog.Warn("stats: cannot rename session",
			"session", sessionID,
			"err", err)
	}
}

// Close shuts the global DB. Intended to be called once at daemon shutdown.
func (s *statsStore) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, db := range s.dbs {
		db.Close()
	}
}
