package collab

import (
	"errors"
	"os"
	"path/filepath"

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
// the gate is at DELIVERY, not send. A recipient whose [collab] cross_project is
// off simply never reads this database, and the rows expire unread — so the
// sender needs no knowledge of the recipient's configuration to be safely
// ignored, and one project can never inject text into another's context by
// writing a file it controls.
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

// IsGlobal reports whether this store is the daemon-level cross-project one.
// Used by callers that must refuse workspace-scoped semantics — notably
// AddresseeNext, which has no meaning across projects ("whoever attaches next"
// to which project?) and so is only ever resolved against a workspace store.
func (s *Store) IsGlobal() bool { return s != nil && s.ws == "" }
