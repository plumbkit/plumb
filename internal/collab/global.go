package collab

import (
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/plumbkit/plumb/internal/paths"
	"github.com/plumbkit/plumb/internal/sqlitex"
)

// global.go holds the daemon-level store that carries CROSS-PROJECT messages.
//
// Same-project messages stay in <workspace>/.plumb/collab.db, where the
// package's per-workspace privacy property holds: a project's message bodies
// never leave that project's directory. A message between two projects has no
// such home. The obvious alternative — having the sender write into the
// recipient's <their-workspace>/.plumb/collab.db — was rejected: it would have a
// session write outside its pinned workspace, which is exactly the class of bug
// the workspace-pin hardening closed. No session ever writes into another
// project's directory.
//
// So a cross-project message lands in one daemon-level database beside stats.db
// and session_state.db, addressed by recipient session name and stamped with the
// sender's workspace. That location is also what makes consent cheap to enforce:
// a recipient whose [collab] cross_project is off simply never reads this
// database, so a row this package accepts is never handed to a project that
// declined it, and one project can never inject text into another's context by
// writing a file it controls.
//
// This package itself gates only that DELIVERY side — it has no opinion on
// whether a row should have been written at all. internal/tools' leave_note
// ALSO checks the target's consent before it ever calls PutNote here, so a
// send to an unconsenting project is refused up front rather than accepted
// and left to expire unread with the sender never told. That send-side check
// is a courtesy for an honest reply, not a second privacy boundary: this
// package's own delivery gate is what actually protects a recipient that
// never opted in, including against a row written before the send-side check
// existed, or by a caller that skips it.
//
// The schema is identical to the per-workspace one, so the same Store type and
// every query serve both. Only the file location and the origin_workspace stamp
// differ.

// globalDBName is the daemon-level cross-project message store, kept in the data
// directory (persistent, alongside stats.db) rather than the state directory:
// like a per-workspace collab.db its rows are the only copy of the data, not a
// rebuildable index.
const globalDBName = "collab-xproject.db"

// GlobalDBPath returns the canonical path of the daemon-level cross-project
// store.
func GlobalDBPath() string { return filepath.Join(paths.DataDir(), globalDBName) }

// GlobalExists reports whether the daemon-level store already exists, without
// creating it. Delivery and prune paths call this first so a daemon whose
// sessions never send a cross-project message never materialises the file.
func GlobalExists() bool {
	_, err := os.Stat(GlobalDBPath())
	return err == nil
}

// OpenGlobal opens or creates the daemon-level cross-project store. Only the
// send path should call it unconditionally; delivery and prune paths guard with
// GlobalExists first.
func OpenGlobal() (*Store, error) {
	if paths.DataDir() == "" {
		return nil, errors.New("collab: no data directory for the cross-project store")
	}
	return OpenGlobalAt(GlobalDBPath())
}

// OpenGlobalAt opens a cross-project store at an explicit path. Production uses
// OpenGlobal; this exists so tests can exercise the REAL daemon-level store
// semantics — an empty workspace, so IsGlobal reports true and the
// target-workspace addressing rules actually apply — without writing to the
// user's data directory.
func OpenGlobalAt(path string) (*Store, error) {
	db, err := sqlitex.Open(path, sqlitex.Options{})
	if err != nil {
		return nil, err
	}
	if err := initDB(db); err != nil {
		db.Close()
		return nil, err
	}
	// ws is deliberately empty: this store belongs to the daemon, not to any one
	// workspace, and nothing may treat it as a workspace's own database.
	return &Store{db: db, ws: ""}, nil
}

// OpenGlobalReadOnly opens the daemon-level cross-project store strictly for
// reading, without creating or migrating it.
//
// It exists for a reader outside the daemon process — today the TUI dashboard,
// which is a separate process talking to the daemon only over its control
// socket for a handful of live commands, so it has no access to the daemon's
// in-memory collabPool the way internal/cli's connSession does and must read
// collab-xproject.db off disk the same way OpenReadOnly lets `plumb mail` read
// a workspace's collab.db. Guard with GlobalExists first when "absent" is a
// normal answer rather than an error.
func OpenGlobalReadOnly() (*Store, error) {
	path := GlobalDBPath()
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	db, err := sqlitex.OpenReadOnly(path, sqlitex.ReadOnlyOptions{BusyTimeout: 2 * time.Second})
	if err != nil {
		return nil, err
	}
	// ws is deliberately empty, as in OpenGlobalAt: this store belongs to the
	// daemon, not to any one workspace.
	return &Store{db: db, ws: ""}, nil
}

// IsGlobal reports whether this store is the daemon-level cross-project one.
// Used by callers that must refuse workspace-scoped semantics — notably
// AddresseeNext, which has no meaning across projects ("whoever attaches next"
// to which project?) and so is only ever resolved against a workspace store.
func (s *Store) IsGlobal() bool { return s != nil && s.ws == "" }
