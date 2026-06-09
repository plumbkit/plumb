package stats

import "sync"

var (
	sharedROMu   sync.Mutex
	sharedRO     *DB
	sharedROPath string
)

// SharedReadOnly returns a process-wide cached read-only handle to the global
// stats database, opening it on first use. It returns (nil, nil) when the
// database does not exist yet (no stats recorded) — callers treat a nil handle
// as "no stats available". The handle is owned by the package and lives for the
// process lifetime; callers must NOT Close it.
//
// Read-only connections under WAL never block the writer, so sharing one handle
// across a long-lived process (the daemon, the TUI) removes the open/close
// churn of per-call OpenReadOnly while staying fully contention-free. One-shot
// CLI commands keep using OpenReadOnly directly — caching buys them nothing.
func SharedReadOnly() (*DB, error) {
	sharedROMu.Lock()
	defer sharedROMu.Unlock()
	path := DBPathFor()
	// Re-open when the resolved database path changes. In production the data
	// directory is fixed for the process lifetime, so this never fires; it keeps
	// tests that point XDG_DATA_HOME at a fresh temp dir from reading a previous
	// test's cached handle.
	if sharedRO != nil && path == sharedROPath {
		return sharedRO, nil
	}
	if sharedRO != nil {
		sharedRO.Close()
		sharedRO = nil
		sharedROPath = ""
	}
	db, err := OpenReadOnly()
	if err != nil {
		return nil, err
	}
	if db == nil {
		return nil, nil // not created yet; don't cache so a later call retries
	}
	sharedRO = db
	sharedROPath = path
	return sharedRO, nil
}
