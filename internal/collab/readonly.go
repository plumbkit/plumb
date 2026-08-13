package collab

import (
	"errors"
	"os"
	"time"

	"github.com/plumbkit/plumb/internal/sqlitex"
)

// readonly.go opens a collab.db that cannot be written to.
//
// It exists for inspection from OUTSIDE a session — today `plumb mail`, which
// answers "is anything waiting for this session?" so a client-side hook can
// keep a turn going rather than let the agent go quiet with mail unread. (It
// cannot reach an agent that is already idle; nothing can.) That caller is not
// the recipient, and must never consume the recipient's mail: setting the
// delivery watermark on its behalf would turn plumb's exactly-once guarantee
// into exactly-never, the message marked delivered while the agent it was
// written for never saw a word.
//
// The obvious implementation is "call Open and only run SELECTs", and that is
// the one deliberately not taken. It leaves the guarantee resting on every
// future caller's discipline, and Open is not side-effect-free either: it
// applies migrations, stamps the schema version, and writes a .gitignore entry.
// A mode=ro handle makes the property structural instead — a claim issued
// through this Store fails at the driver, so the invariant cannot be eroded by
// someone wiring a different query in later.

// OpenReadOnly opens an existing workspace collab.db strictly for reading.
//
// It never creates the database, never migrates it, and cannot write a row.
// Guard with Exists first when "absent" is a normal answer rather than an
// error — a workspace whose sessions have never used an intents/mailbox feature
// legitimately has no collab.db, and this must not be the thing that gives it
// one.
//
// The schema is NOT migrated, so a pre-v2 file is read with whatever columns it
// has. Every query in this package selects the v2 column set, so a caller that
// meets a v1 file gets a query error rather than wrong data — the right failure
// for an advisory probe, which treats an error as "cannot tell" and proceeds.
func OpenReadOnly(workspace string) (*Store, error) {
	if workspace == "" {
		return nil, errors.New("collab: empty workspace")
	}
	path := DBPath(workspace)
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	// A short busy timeout, not the default: the caller is an inspector on a
	// hook's end-of-turn path, where failing fast and reporting "cannot tell"
	// beats stalling a turn behind a busy daemon.
	db, err := sqlitex.OpenReadOnly(path, sqlitex.ReadOnlyOptions{BusyTimeout: 2 * time.Second})
	if err != nil {
		return nil, err
	}
	return &Store{db: db, ws: workspace}, nil
}
