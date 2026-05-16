package cli

import (
	"log/slog"
	"sync"

	"github.com/golimpio/plumb/internal/stats"
)

// statsStore owns a map of per-workspace stats databases, opened lazily on
// first write. The daemon uses this to record tool-call telemetry next to
// the project being worked on (<workspace>/.plumb/stats.db), rather than
// in a single global database where unrelated projects mingle.
//
// Concurrency: safe for concurrent Record / Close.
type statsStore struct {
	mu  sync.Mutex
	dbs map[string]*stats.DB
}

func newStatsStore() *statsStore {
	return &statsStore{dbs: make(map[string]*stats.DB)}
}

// Record writes one call to the workspace's stats DB, opening it on first
// use. Calls with an empty workspace are dropped — we can't attribute them
// to any project, and the alternative (a fallback global DB) would
// reintroduce the cross-project mingling we're trying to avoid.
func (s *statsStore) Record(workspace string, call stats.Call) {
	if s == nil || workspace == "" {
		return
	}
	s.mu.Lock()
	db, ok := s.dbs[workspace]
	if !ok {
		var err error
		db, err = stats.Open(stats.DBPathFor(workspace))
		if err != nil {
			s.mu.Unlock()
			slog.Warn("stats: cannot open per-project DB", "workspace", workspace, "err", err)
			return
		}
		s.dbs[workspace] = db
	}
	s.mu.Unlock()
	if err := db.Record(call); err != nil {
		slog.Warn("stats: cannot record tool call",
			"workspace", workspace,
			"session", call.SessionID,
			"tool", call.Tool,
			"err", err)
	}
}

// Close shuts all open DBs. Intended to be called once at daemon shutdown.
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
