package session_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/session"
)

// TestRecordUnended_ReportsTheRecordNotTheConnection is PLAN-517's probe against
// REAL session files, which is the part the receipt's own tests cannot cover: they
// inject a fake probe, so without this the production probe could answer `false`
// for everything and every receipt test would stay green.
func TestRecordUnended_ReportsTheRecordNotTheConnection(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	id, err := registerID(session.Info{Language: "go", Folder: "/tmp", Adapter: "gopls"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !session.RecordUnended(id) {
		t.Errorf("a registered, un-ended record (%s) reads as ended", id)
	}
	session.Unregister(id)
	if session.RecordUnended(id) {
		t.Error("a record reads as un-ended after Unregister marked it ended")
	}

	// Every way the record can be absent must answer false: the receipt's arm
	// claims something about the bound session, so anything it cannot read has to
	// leave the old wording in place.
	if session.RecordUnended("no-such-session") {
		t.Error("a missing record reads as un-ended")
	}
	if session.RecordUnended("") {
		t.Error("an empty id reads as un-ended")
	}
	dir, err := session.Dir()
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "not-json.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write unparsable record: %v", err)
	}
	if session.RecordUnended("not-json") {
		t.Error("an unparsable record reads as un-ended")
	}

	// The id becomes a file name, so a traversing one must not reach a real,
	// un-ended record outside the session directory.
	escaped := filepath.Join(filepath.Dir(dir), "escape.json")
	if err := os.WriteFile(escaped, []byte(`{"ended_at":"0001-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatalf("write record outside the session dir: %v", err)
	}
	if session.RecordUnended("../escape") {
		t.Error("a traversing id resolved to a record outside the session directory")
	}
}
