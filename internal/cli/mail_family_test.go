package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
	"github.com/plumbkit/plumb/internal/sqlitex"
)

// noGlobalMailbox stands in for a machine with no daemon-level store, so a test
// of the workspace mailbox never reads the host's.
func noGlobalMailbox() (*collab.Store, error) { return nil, nil }

// TestMailWaiting_CountsSameProjectMailFromTheDaemonStore is the wake half of
// #635 outside the daemon: `plumb mail` is what a client hook runs at turn end,
// and a note from this repository's own worktree lives in the daemon-level
// store. Counted without consent — the probe never resolves the workspace's
// config — while another project's note aimed here is not.
func TestMailWaiting_CountsSameProjectMailFromTheDaemonStore(t *testing.T) {
	repo := t.TempDir()
	common := filepath.Join(repo, ".git")
	wt := filepath.Join(t.TempDir(), "wt")
	other := t.TempDir()
	for _, dir := range []string{
		common, filepath.Join(common, "objects"), filepath.Join(other, ".git", "objects"),
		filepath.Join(common, "worktrees", "wt"), wt,
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, body := range map[string]string{
		filepath.Join(wt, ".git"):                             "gitdir: " + filepath.Join(common, "worktrees", "wt") + "\n",
		filepath.Join(common, "worktrees", "wt", "commondir"): "../..\n",
		filepath.Join(common, "worktrees", "wt", "gitdir"):    filepath.Join(wt, ".git") + "\n",
		filepath.Join(common, "HEAD"):                         "ref: refs/heads/main\n",
		filepath.Join(other, ".git", "HEAD"):                  "ref: refs/heads/main\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dbPath := filepath.Join(t.TempDir(), "x.db")
	global, err := collab.OpenGlobalAt(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = global.Close() })
	// A fresh handle per probe, as openGlobalMailbox gives it: the probe closes
	// what it opens.
	open := func() (*collab.Store, error) { return collab.OpenGlobalAt(dbPath) }
	now := time.Now()
	for origin, body := range map[string]string{wt: "family", other: "foreign"} {
		if _, err := global.PutNote(context.Background(), collab.NoteInput{
			AuthorSession: "bob", AuthorID: "sess-bob", Body: body, Addressee: "alice",
			TTL: time.Hour, OriginWorkspace: origin, TargetWorkspace: repo,
		}, now); err != nil {
			t.Fatal(err)
		}
	}

	who := collab.Claimant{Name: "alice", ID: "sess-alice", Workspace: repo}
	ages, err := mailWaitingIn(who, open)
	if err != nil {
		t.Fatal(err)
	}
	if len(ages) != 1 {
		t.Fatalf("plumb mail counted %d; want exactly the note from this project's own worktree", len(ages))
	}
	// Even a caller that claims consent is counted without it: the probe does not
	// resolve the workspace's config, so it must not trust a flag it was handed.
	who.CrossProject = true
	if ages, err = mailWaitingIn(who, open); err != nil || len(ages) != 1 {
		t.Fatalf("plumb mail counted %d, err %v; consent is not this probe's to grant", len(ages), err)
	}
}

// TestMailWaiting_AnUnreadableDaemonStoreKeepsTheWorkspaceCount: the daemon-level
// store is best-effort for this probe, so a store it cannot open or read — one an
// older daemon wrote and has not migrated — must not cost the workspace count.
func TestMailWaiting_AnUnreadableDaemonStoreKeepsTheWorkspaceCount(t *testing.T) {
	ws := t.TempDir()
	local, err := collab.Open(ws)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = local.Close() })
	if _, err := local.PutNote(context.Background(), collab.NoteInput{
		AuthorSession: "bob", AuthorID: "sess-bob", Body: "local", Addressee: "alice", TTL: time.Hour,
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	broken := func() (*collab.Store, error) { return nil, os.ErrPermission }
	ages, err := mailWaitingIn(collab.Claimant{Name: "alice", ID: "sess-alice", Workspace: ws}, broken)
	if err != nil || len(ages) != 1 {
		t.Fatalf("ages = %v, err %v; want the workspace note counted despite the daemon store", ages, err)
	}
}

// TestMailWaiting_AnUnmigratedDaemonStoreKeepsTheWorkspaceCount: the store opens
// but cannot be READ by this binary — an older daemon's, without
// origin_git_common_dir. The workspace count must survive that too.
func TestMailWaiting_AnUnmigratedDaemonStoreKeepsTheWorkspaceCount(t *testing.T) {
	ws := t.TempDir()
	local, err := collab.Open(ws)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = local.Close() })
	if _, err := local.PutNote(context.Background(), collab.NoteInput{
		AuthorSession: "bob", AuthorID: "sess-bob", Body: "local", Addressee: "alice", TTL: time.Hour,
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	// The production opener, pointed at a data dir of this test's own.
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	g, err := collab.OpenGlobalAt(collab.GlobalDBPath())
	if err != nil {
		t.Fatal(err)
	}
	_ = g.Close()
	db, err := sqlitex.Open(collab.GlobalDBPath(), sqlitex.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`DROP INDEX idx_collab_target`,
		`ALTER TABLE collab_rows DROP COLUMN origin_git_common_dir`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	_ = db.Close()
	open := openGlobalMailbox
	ages, err := mailWaitingIn(collab.Claimant{Name: "alice", ID: "sess-alice", Workspace: ws}, open)
	if err != nil || len(ages) != 1 {
		t.Fatalf("ages = %v, err %v; want the workspace note counted despite the unmigrated store", ages, err)
	}
}
