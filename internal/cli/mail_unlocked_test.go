package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/session"
)

// TestRunMail_AnswersWhenTheRegistryLockCannotBeOpened is PLAN-495's sandbox
// case end to end through runMail: the session-directory lock file exists but
// this process may not open it, as in a harness that denies writes under the data
// dir. The probe still resolves the session and counts its mail, and the report
// says the registry was read without its lock.
func TestRunMail_AnswersWhenTheRegistryLockCannotBeOpened(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens a mode-0000 file regardless, so the refused open cannot be simulated")
	}
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Cleanup(resetMailFlags)
	resetMailFlags()

	ws := t.TempDir()
	info, err := session.Register(session.Info{Name: "quiet-mesa", Folder: ws, Language: "go"})
	if err != nil {
		t.Fatalf("registering session: %v", err)
	}
	session.SetExternalID(info.ID, "cc-sandboxed")
	putBoundTestNote(t, ws, "swift-falcon", "quiet-mesa", info.ID, "ratelimit is yours")

	dir, err := session.Dir()
	if err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(dir, ".sessions.lock")
	if err := os.Chmod(lock, 0o000); err != nil {
		t.Fatalf("making the lock unopenable: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(lock, 0o644) })

	mailFlagExternalID = "cc-sandboxed"
	raw := captureMailJSON(t)
	var report mailReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("decoding the report %q: %v", raw, err)
	}
	if report.Session != "quiet-mesa" || report.Count != 1 {
		t.Fatalf("report = %+v, want quiet-mesa with 1 waiting message even though the lock cannot be opened", report)
	}
	if !report.UnlockedRead {
		t.Errorf("unlocked_read = false, but the registry could only have been read without its lock: %s", raw)
	}
	if s := mailSentence(report); !strings.Contains(s, "without its lock") {
		t.Errorf("the human form does not say the registry was read without its lock: %q", s)
	}
}

// TestRunMail_LockedReadOmitsTheCaveat is the control: an ordinary, locked read
// carries no unlocked_read key at all, so every existing consumer of the JSON
// sees exactly what it saw before.
func TestRunMail_LockedReadOmitsTheCaveat(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Cleanup(resetMailFlags)
	resetMailFlags()

	ws := t.TempDir()
	info, err := session.Register(session.Info{Name: "quiet-mesa", Folder: ws, Language: "go"})
	if err != nil {
		t.Fatalf("registering session: %v", err)
	}
	session.SetExternalID(info.ID, "cc-ordinary")

	mailFlagExternalID = "cc-ordinary"
	raw := captureMailJSON(t)
	if strings.Contains(string(raw), "unlocked_read") {
		t.Errorf("a locked read emitted unlocked_read: %s", raw)
	}
	var report mailReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("decoding the report %q: %v", raw, err)
	}
	if s := mailSentence(report); strings.Contains(s, "without its lock") {
		t.Errorf("a locked read's sentence carries the unlocked caveat: %q", s)
	}
}
