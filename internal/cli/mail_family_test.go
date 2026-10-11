package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
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
	for _, dir := range []string{common, filepath.Join(other, ".git"), filepath.Join(common, "worktrees", "wt"), wt} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, body := range map[string]string{
		filepath.Join(wt, ".git"):                             "gitdir: " + filepath.Join(common, "worktrees", "wt") + "\n",
		filepath.Join(common, "worktrees", "wt", "commondir"): "../..\n",
		filepath.Join(common, "worktrees", "wt", "gitdir"):    filepath.Join(wt, ".git") + "\n",
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
