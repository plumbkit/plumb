package collab

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// scope_test.go pins the read scope of the daemon-level store (#635): without
// cross_project consent a reader sees exactly the rows its own repository family
// wrote, re-verified against the disk at read time, and with consent it sees what
// it always did. The git layouts are written by hand, as in internal/paths: the
// shapes under test are the link files git writes, and a forged stamp is only
// expressible that way.

func scopeWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// scopeRepo makes a main checkout at root: .git is a directory.
func scopeRepo(t *testing.T, root string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// scopeWorktree links a checkout at wt to the repository whose common directory
// is common, as `git worktree add` writes it.
func scopeWorktree(t *testing.T, common, id, wt string) string {
	t.Helper()
	gitDir := filepath.Join(common, "worktrees", id)
	scopeWrite(t, filepath.Join(wt, ".git"), "gitdir: "+gitDir+"\n")
	scopeWrite(t, filepath.Join(gitDir, "commondir"), "../..\n")
	scopeWrite(t, filepath.Join(gitDir, "gitdir"), filepath.Join(wt, ".git")+"\n")
	return wt
}

// scopeSubmodule makes a submodule checkout at super/name, its git directory in
// the superproject's .git/modules, as `git submodule add` writes it.
func scopeSubmodule(t *testing.T, super, name string) string {
	t.Helper()
	sub := filepath.Join(super, name)
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	scopeWrite(t, filepath.Join(sub, ".git"), "gitdir: ../.git/modules/"+name+"\n")
	scopeWrite(t, filepath.Join(super, ".git", "modules", name, "config"), "[core]\n\tworktree = ../../../"+name+"\n")
	return sub
}

func openScopeGlobal(t *testing.T) *Store {
	t.Helper()
	g, err := OpenGlobalAt(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	return g
}

// sendFrom writes a daemon-level note from a session in origin to alice in target.
func sendFrom(t *testing.T, g *Store, origin, target, body string, now time.Time) {
	t.Helper()
	if _, err := g.PutNote(context.Background(), NoteInput{
		AuthorSession: "bob", AuthorID: "sess-bob", Body: body, Addressee: "alice",
		TTL: time.Hour, OriginWorkspace: origin, TargetWorkspace: target,
	}, now); err != nil {
		t.Fatal(err)
	}
}

// readerCounts is every delivery read the store offers, so one assertion covers
// the probe, the listing, the count, the snapshot and the pure read together —
// they must agree, or a reader is woken for mail it cannot claim.
type readerCounts struct{ probe, claimable, pending, count, snapshot int }

func countsFor(t *testing.T, g *Store, who Claimant, now time.Time) readerCounts {
	t.Helper()
	ctx := context.Background()
	var c readerCounts
	has, err := g.HasPendingNotes(ctx, who, now)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		c.probe = 1
	}
	rows, err := g.ClaimableNotes(ctx, who, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	c.claimable = len(rows)
	listed, err := g.PendingNotes(ctx, who, now)
	if err != nil {
		t.Fatal(err)
	}
	c.pending = len(listed)
	if c.count, err = g.PendingCount(ctx, who, now); err != nil {
		t.Fatal(err)
	}
	snap, err := g.Snapshot(ctx, who, now)
	if err != nil {
		t.Fatal(err)
	}
	c.snapshot = snap.Count
	return c
}

func TestScope_SameFamilyIsReadWithoutConsent(t *testing.T) {
	repo := scopeRepo(t, t.TempDir())
	wt := scopeWorktree(t, filepath.Join(repo, ".git"), "wt", filepath.Join(t.TempDir(), "wt"))
	g := openScopeGlobal(t)
	now := time.Now()
	sendFrom(t, g, wt, repo, "from my own worktree", now)

	alice := Claimant{Name: "alice", ID: "sess-alice", Workspace: repo}
	if got := countsFor(t, g, alice, now); got != (readerCounts{1, 1, 1, 1, 1}) {
		t.Fatalf("a note from this repository's own worktree must reach every reader without consent: %+v", got)
	}
	claimed, err := g.ClaimNotes(context.Background(), alice, now, 0)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim = %d rows, err %v; want the one note", len(claimed), err)
	}
}

func TestScope_ForeignMailStillNeedsConsent(t *testing.T) {
	repo := scopeRepo(t, t.TempDir())
	other := scopeRepo(t, t.TempDir())
	g := openScopeGlobal(t)
	now := time.Now()
	sendFrom(t, g, other, repo, "from an unrelated repository", now)

	off := Claimant{Name: "alice", ID: "sess-alice", Workspace: repo}
	if got := countsFor(t, g, off, now); got != (readerCounts{}) {
		t.Fatalf("an unrelated project's note must stay invisible without consent: %+v", got)
	}
	on := off
	on.CrossProject = true
	if got := countsFor(t, g, on, now); got != (readerCounts{1, 1, 1, 1, 1}) {
		t.Fatalf("with consent the same note must reach every reader (positive control): %+v", got)
	}
}

func TestScope_ANestedCloneIsAnotherProject(t *testing.T) {
	// An unrelated repository cloned INSIDE the project: nested by path, foreign by
	// ownership. Its stamp is its own .git, so its mail needs consent like any other.
	repo := scopeRepo(t, t.TempDir())
	clone := scopeRepo(t, filepath.Join(repo, "vendor", "clone"))
	g := openScopeGlobal(t)
	now := time.Now()
	sendFrom(t, g, clone, repo, "from the nested clone", now)

	if got := countsFor(t, g, Claimant{Name: "alice", ID: "sess-alice", Workspace: repo}, now); got != (readerCounts{}) {
		t.Fatalf("a nested clone must not pass as the same project: %+v", got)
	}
}

func TestScope_APlainNestedDirectoryIsUnstamped(t *testing.T) {
	// A directory inside the project holding no checkout of its own: the sender's
	// workspace verifies as nothing, so the row carries no stamp and only a
	// consenting reader sees it.
	repo := scopeRepo(t, t.TempDir())
	plain := filepath.Join(repo, "tools", "plain")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	g := openScopeGlobal(t)
	now := time.Now()
	sendFrom(t, g, plain, repo, "from a plain directory", now)

	rows, err := g.Conversation(context.Background(), onlyConversation(t, g), now)
	if err != nil || len(rows) != 1 {
		t.Fatalf("conversation = %d rows, err %v", len(rows), err)
	}
	if rows[0].OriginGitCommonDir != "" {
		t.Fatalf("a sender with no checkout must not be stamped; got %q", rows[0].OriginGitCommonDir)
	}
	if got := countsFor(t, g, Claimant{Name: "alice", ID: "sess-alice", Workspace: repo}, now); got != (readerCounts{}) {
		t.Fatalf("an unstamped note must need consent: %+v", got)
	}
}

func TestScope_ADeletedSendersNoteStillArrives(t *testing.T) {
	// The worktree is removed minutes after sending, as `git worktree remove`
	// does: the checkout AND its git directory in <common>/worktrees/ go. The
	// stamp names the common directory, which outlives both.
	repo := scopeRepo(t, t.TempDir())
	common := filepath.Join(repo, ".git")
	wt := scopeWorktree(t, common, "gone", filepath.Join(t.TempDir(), "gone"))
	g := openScopeGlobal(t)
	now := time.Now()
	sendFrom(t, g, wt, repo, "sent, then the worktree was removed", now)
	for _, dir := range []string{wt, filepath.Join(common, "worktrees", "gone")} {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
	}

	if got := countsFor(t, g, Claimant{Name: "alice", ID: "sess-alice", Workspace: repo}, now); got != (readerCounts{1, 1, 1, 1, 1}) {
		t.Fatalf("a note from a since-deleted worktree must still reach every reader: %+v", got)
	}
}

func TestScope_SubmoduleAndSuperprojectEitherWay(t *testing.T) {
	// The documented boundary: ./plumb inside plumb-ops is one project with it.
	super := scopeRepo(t, t.TempDir())
	sub := scopeSubmodule(t, super, "plumb")
	subWT := scopeWorktree(t, filepath.Join(super, ".git", "modules", "plumb"), "fric", filepath.Join(t.TempDir(), "fric"))
	g := openScopeGlobal(t)
	now := time.Now()

	for _, pair := range [][2]string{{sub, super}, {super, sub}, {subWT, super}, {super, subWT}} {
		sendFrom(t, g, pair[0], pair[1], "family", now)
		who := Claimant{Name: "alice", ID: "sess-alice", Workspace: pair[1]}
		claimed, err := g.ClaimNotes(context.Background(), who, now, 0)
		if err != nil || len(claimed) != 1 {
			t.Fatalf("%s -> %s: claimed %d, err %v; want 1", pair[0], pair[1], len(claimed), err)
		}
	}
}

func TestScope_TheStampIsReverifiedAtRead(t *testing.T) {
	// A row whose stamp names a repository that is NOT this reader's family must
	// not be read on the strength of having been stamped: the reader checks the
	// stamp against its own checkout. Written directly, as only a forged or stale
	// row could carry such a stamp.
	repo := scopeRepo(t, t.TempDir())
	other := scopeRepo(t, t.TempDir())
	g := openScopeGlobal(t)
	now := time.Now()
	if _, err := g.db.Exec(`INSERT INTO collab_rows (kind, author_session, author_id, body, addressee,
		created_at, expires_at, conversation_id, origin_workspace, target_workspace, origin_git_common_dir)
		VALUES (?, 'bob', 'sess-bob', 'forged', 'alice', ?, ?, 'c-forged', ?, ?, ?)`,
		string(KindNote), now.UnixNano(), now.Add(time.Hour).UnixNano(), other, repo, filepath.Join(other, ".git")); err != nil {
		t.Fatal(err)
	}
	if got := countsFor(t, g, Claimant{Name: "alice", ID: "sess-alice", Workspace: repo}, now); got != (readerCounts{}) {
		t.Fatalf("a stamp that does not verify against the reader must not be read: %+v", got)
	}
	// Positive control: the same shape stamped with the reader's own common
	// directory IS read.
	if _, err := g.db.Exec(`UPDATE collab_rows SET origin_git_common_dir = ? WHERE conversation_id = 'c-forged'`,
		filepath.Join(repo, ".git")); err != nil {
		t.Fatal(err)
	}
	if got := countsFor(t, g, Claimant{Name: "alice", ID: "sess-alice", Workspace: repo}, now.Add(ownershipTTL)); got.claimable != 1 {
		t.Fatalf("a stamp naming the reader's own common directory must be read: %+v", got)
	}
}

func TestScope_TargetStillScopesFamilyMail(t *testing.T) {
	// Family membership widens WHO may send without consent, never who may read:
	// a note aimed at one worktree is not claimable from another.
	repo := scopeRepo(t, t.TempDir())
	common := filepath.Join(repo, ".git")
	a := scopeWorktree(t, common, "a", filepath.Join(t.TempDir(), "a"))
	b := scopeWorktree(t, common, "b", filepath.Join(t.TempDir(), "b"))
	g := openScopeGlobal(t)
	now := time.Now()
	sendFrom(t, g, a, repo, "for the main checkout", now)

	if got := countsFor(t, g, Claimant{Name: "alice", ID: "sess-alice", Workspace: b}, now); got != (readerCounts{}) {
		t.Fatalf("a sibling worktree must not read a note aimed at the main checkout: %+v", got)
	}
	// Now b has family mail of its own, so the origin lookup finds the family's
	// stamp for b too: only the target term keeps the main checkout's note out.
	sendFrom(t, g, a, b, "for the sibling", now)
	if got := countsFor(t, g, Claimant{Name: "alice", ID: "sess-alice", Workspace: b}, now); got != (readerCounts{1, 1, 1, 1, 1}) {
		t.Fatalf("the sibling must read exactly its own note, not the main checkout's: %+v", got)
	}
}

func TestConversationFor_WithoutConsentKeepsOnlyFamilyRows(t *testing.T) {
	repo := scopeRepo(t, t.TempDir())
	wt := scopeWorktree(t, filepath.Join(repo, ".git"), "wt", filepath.Join(t.TempDir(), "wt"))
	other := scopeRepo(t, t.TempDir())
	g := openScopeGlobal(t)
	now := time.Now()
	ctx := context.Background()

	conv, err := g.PutNote(ctx, NoteInput{
		AuthorSession: "bob", AuthorID: "sess-bob", Body: "family", Addressee: "alice",
		TTL: time.Hour, OriginWorkspace: wt, TargetWorkspace: repo,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	// The foreign row joins the SAME thread — written directly, because PutNote's
	// participant guard would (rightly) refuse a stranger.
	if _, err := g.db.Exec(`INSERT INTO collab_rows (kind, author_session, author_id, body, addressee,
		created_at, expires_at, conversation_id, origin_workspace, target_workspace, origin_git_common_dir)
		VALUES (?, 'carol', 'sess-carol', 'foreign', 'bob', ?, ?, ?, ?, ?, ?)`,
		string(KindNote), now.UnixNano(), now.Add(time.Hour).UnixNano(), conv, other, wt, filepath.Join(other, ".git")); err != nil {
		t.Fatal(err)
	}

	off, err := g.ConversationFor(ctx, conv, Claimant{Workspace: repo}, now)
	if err != nil || len(off) != 1 || off[0].Body != "family" {
		t.Fatalf("without consent the thread must show only the family's row; got %d rows, err %v", len(off), err)
	}
	on, err := g.ConversationFor(ctx, conv, Claimant{Workspace: repo, CrossProject: true}, now)
	if err != nil || len(on) != 2 {
		t.Fatalf("with consent the whole thread is visible (positive control); got %d rows, err %v", len(on), err)
	}
}

func TestConversationSummariesForWorkspace_FamilyWithoutConsent(t *testing.T) {
	repo := scopeRepo(t, t.TempDir())
	wt := scopeWorktree(t, filepath.Join(repo, ".git"), "wt", filepath.Join(t.TempDir(), "wt"))
	other := scopeRepo(t, t.TempDir())
	g := openScopeGlobal(t)
	now := time.Now()
	sendFrom(t, g, wt, repo, "family", now)
	sendFrom(t, g, other, repo, "foreign", now)

	ctx := context.Background()
	off, err := g.ConversationSummariesForWorkspace(ctx, Claimant{Workspace: repo}, now, 0)
	if err != nil || len(off) != 1 {
		t.Fatalf("without consent only the family's thread is counted; got %d, err %v", len(off), err)
	}
	on, err := g.ConversationSummariesForWorkspace(ctx, Claimant{Workspace: repo, CrossProject: true}, now, 0)
	if err != nil || len(on) != 2 {
		t.Fatalf("with consent both threads are counted (positive control); got %d, err %v", len(on), err)
	}
}

// onlyConversation returns the id of the single conversation in g.
func onlyConversation(t *testing.T, g *Store) string {
	t.Helper()
	var conv string
	if err := g.db.QueryRow(`SELECT conversation_id FROM collab_rows LIMIT 1`).Scan(&conv); err != nil {
		t.Fatal(err)
	}
	return conv
}
