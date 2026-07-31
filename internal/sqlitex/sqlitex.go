// Package sqlitex is the single place plumb opens a SQLite database.
//
// It exists because of one specific footgun. plumb uses the modernc.org/sqlite
// driver, which accepts connection pragmas only in the `_pragma=name(value)`
// form and SILENTLY IGNORES the mattn-style `_busy_timeout=`/`_journal_mode=`
// spelling that most SQLite documentation and every Stack Overflow answer
// shows. A DSN with the wrong spelling opens fine, queries fine, and leaves
// busy_timeout at 0 — so recoverable writer contention surfaces as an immediate
// "database is locked" under concurrency, and only under concurrency.
//
// That has been fixed three separate times in this tree, and the warning was
// written out near-verbatim in three different files. Building the DSN here, by
// construction, is the only version of the fix that also covers the fourth
// occurrence nobody has noticed yet.
//
// The second reason is per-connection semantics. busy_timeout, foreign_keys and
// synchronous apply to a single connection, not to the database. Setting them
// with a post-open db.Exec configures only whichever pooled connection served
// that statement and leaves every other one at the default — which is invisible
// while a pool holds one connection, and becomes a bug the moment it holds two.
// Everything here therefore travels in the DSN.
//
// What this package does NOT own is schema policy. Each caller keeps its own
// DDL and decides what a version mismatch means: the topology index is
// rebuildable and drops its tables, stats and sessionstate migrate forward,
// collab and memory stamp a constant, and the read-only stats handle refuses to
// open at all. Those are four genuinely different answers; only the mechanics
// of reading and stamping the version are shared, via Version and StampVersion.
//
// Concurrency: Open returns a *sql.DB, which is itself safe for concurrent use.
package sqlitex

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	// The driver every caller uses. Registered here so a caller cannot open a
	// database with a different one by accident.
	_ "modernc.org/sqlite"
)

// driverName is modernc.org/sqlite's registration name. Note it is "sqlite",
// not "sqlite3" — the latter is mattn/go-sqlite3, whose DSN dialect differs in
// exactly the way this package exists to prevent.
const driverName = "sqlite"

// DefaultBusyTimeout is the writer-contention wait for a read-write handle.
// Long enough to ride out another connection's transaction, short enough that a
// genuine deadlock still surfaces.
const DefaultBusyTimeout = 5 * time.Second

// DirPerm is the permission for a created database directory. These hold a
// user's own workspace state, so they are private rather than world-readable.
const DirPerm os.FileMode = 0o700

// Sync selects the synchronous pragma, which is the durability/throughput
// trade-off for commits.
type Sync int

const (
	// SyncFull is SQLite's default: every commit is durable against a power
	// cut. The right choice for anything a user would notice losing.
	SyncFull Sync = iota

	// SyncNormal skips the per-commit fsync. Under WAL this is still
	// corruption-safe — the exposure is losing the most recent commits on a
	// hard power cut, not a damaged database. Appropriate for telemetry and
	// derived state that is cheap to lose and expensive to fsync.
	SyncNormal
)

func (s Sync) pragma() string {
	if s == SyncNormal {
		return "normal"
	}
	return "full"
}

// Options configures a read-write open. The zero value is valid: a 5s busy
// timeout, WAL, foreign keys on, full durability, and an unbounded pool.
type Options struct {
	// BusyTimeout is how long a connection waits for a lock before returning
	// SQLITE_BUSY. Zero means DefaultBusyTimeout.
	BusyTimeout time.Duration

	// Sync selects the synchronous pragma. The zero value is SyncFull; a
	// caller that wants SyncNormal must say so, because it is a durability
	// decision rather than a tuning knob.
	Sync Sync

	// MaxOpenConns caps the connection pool. Zero leaves it unbounded.
	//
	// With the pragmas in the DSN a cap is no longer needed for correctness —
	// it used to be what made a post-open pragma Exec accidentally safe — so
	// set it only where serialised access is genuinely wanted.
	MaxOpenConns int
}

// ReadOnlyOptions configures a read-only open.
//
// Read-only handles deliberately do not set journal_mode or foreign_keys:
// changing the journal mode requires a write, and constraint enforcement is
// meaningless when nothing can be written.
type ReadOnlyOptions struct {
	// BusyTimeout is how long a reader waits for a lock. Zero means
	// DefaultBusyTimeout. Inspectors that must not hang behind a busy daemon
	// (doctor, the TUI) should set something short.
	BusyTimeout time.Duration
}

// Open opens or creates the database at path, creating its parent directory as
// needed, with WAL journalling, foreign-key enforcement and a busy timeout
// applied to every pooled connection.
//
// foreign_keys is on unconditionally and is not an option. SQLite defaults it
// OFF, which makes a declared ON DELETE CASCADE silently do nothing and orphan
// rows accumulate; there is no case in this codebase where that is wanted, and
// it costs nothing for a schema that declares no foreign keys.
//
// The caller still owns its schema: apply DDL and consult Version afterwards.
func Open(path string, opts Options) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), DirPerm); err != nil {
		return nil, fmt.Errorf("sqlitex: creating database directory: %w", err)
	}
	db, err := sql.Open(driverName, readWriteDSN(path, opts))
	if err != nil {
		return nil, fmt.Errorf("sqlitex: opening %s: %w", path, err)
	}
	if opts.MaxOpenConns > 0 {
		db.SetMaxOpenConns(opts.MaxOpenConns)
	}
	return db, nil
}

// OpenReadOnly opens an existing database strictly for reading.
//
// mode=ro makes the inspection side-effect-free: it never writes the main
// database and, when the writer is down and the WAL has been checkpointed away,
// creates no -wal/-shm sidecars. It does not create the file — a caller wanting
// "absent" to be distinguishable should stat the path first.
func OpenReadOnly(path string, opts ReadOnlyOptions) (*sql.DB, error) {
	db, err := sql.Open(driverName, readOnlyDSN(path, opts))
	if err != nil {
		return nil, fmt.Errorf("sqlitex: opening %s read-only: %w", path, err)
	}
	return db, nil
}

func readWriteDSN(path string, opts Options) string {
	busy := opts.BusyTimeout
	if busy <= 0 {
		busy = DefaultBusyTimeout
	}
	return dsn(path, []string{
		pragma("busy_timeout", strconv.FormatInt(busy.Milliseconds(), 10)),
		pragma("foreign_keys", "1"),
		pragma("journal_mode", "WAL"),
		pragma("synchronous", opts.Sync.pragma()),
	})
}

func readOnlyDSN(path string, opts ReadOnlyOptions) string {
	busy := opts.BusyTimeout
	if busy <= 0 {
		busy = DefaultBusyTimeout
	}
	return dsn(path, []string{
		"mode=ro",
		pragma("busy_timeout", strconv.FormatInt(busy.Milliseconds(), 10)),
	})
}

// dsn renders the connection string as a file: URI.
//
// The scheme is not decoration. SQLite only honours query parameters that
// change how the database is OPENED — mode=ro above all — when the DSN is a
// URI; given a bare path it treats "?mode=ro" as part of the filename and
// opens read-WRITE regardless. That is silent: the handle works, reads work,
// and writes that were supposed to be impossible quietly succeed. plumb was
// doing exactly this in stats.OpenReadOnly and topology.StatusForWorkspace,
// both of which documented themselves as side-effect-free.
//
// url.URL does the percent-encoding, so a workspace path containing a space,
// '#' or '?' survives the trip. RawQuery is assigned rather than built from
// url.Values because the pragma form's parentheses must reach the driver
// literally.
func dsn(path string, params []string) string {
	u := url.URL{Scheme: "file", OmitHost: true, Path: path, RawQuery: strings.Join(params, "&")}
	return u.String()
}

// pragma renders one connection pragma in the only spelling the modernc driver
// honours. Everything about the DSN funnels through here on purpose.
func pragma(name, value string) string {
	return "_pragma=" + name + "(" + value + ")"
}

// Version reads PRAGMA user_version, the schema version stamped in the database
// header. A database this process just created reads 0.
func Version(db *sql.DB) (int, error) {
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("sqlitex: reading user_version: %w", err)
	}
	return v, nil
}

// StampVersion records v as the database's schema version.
//
// The stamp is a WRITE. Callers should issue it only when the version actually
// moved: stamping on every open turns what could be a read-only session into a
// writer contending for the write lock.
//
// The value is interpolated rather than bound because PRAGMA statements do not
// accept parameters in SQLite; v is an integer from a package constant, never
// user input.
func StampVersion(db *sql.DB, v int) error {
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", v)); err != nil {
		return fmt.Errorf("sqlitex: stamping user_version: %w", err)
	}
	return nil
}
