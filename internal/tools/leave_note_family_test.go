package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/collab"
)

// leave_note_family_test.go is #635 end to end: two sessions pinned to two
// checkouts of ONE repository family — a worktree and its main checkout, a
// submodule and its superproject — exchange mail with cross_project OFF on both
// sides, through the real send path, every delivery reader (check_messages, the
// preview, the wake snapshot) and the thread-reply path. The refusals are pinned
// alongside: a plain directory nested in the project and an unrelated clone
// nested in it are other projects, and still need consent.

func familyWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func familyRepo(t *testing.T, root string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func familyWorktree(t *testing.T, common, id, wt string) string {
	t.Helper()
	gitDir := filepath.Join(common, "worktrees", id)
	familyWrite(t, filepath.Join(wt, ".git"), "gitdir: "+gitDir+"\n")
	familyWrite(t, filepath.Join(gitDir, "commondir"), "../..\n")
	familyWrite(t, filepath.Join(gitDir, "gitdir"), filepath.Join(wt, ".git")+"\n")
	return wt
}

func familySubmodule(t *testing.T, super, name string) string {
	t.Helper()
	sub := filepath.Join(super, name)
	familyWrite(t, filepath.Join(sub, ".git"), "gitdir: ../.git/modules/"+name+"\n")
	familyWrite(t, filepath.Join(super, ".git", "modules", name, "config"), "[core]\n\tworktree = ../../../"+name+"\n")
	return sub
}

// familySession is one session pinned to ws, with consent OFF and no target ever
// consenting: everything that arrives does so as same-project mail or not at all.
type familySession struct {
	deps  CollabDeps
	local *collab.Store
}

func newFamilySession(t *testing.T, ws, self string, global *collab.Store) familySession {
	t.Helper()
	local, err := collab.Open(ws)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = local.Close() })
	return familySession{local: local, deps: CollabDeps{
		Workspace:                func() string { return ws },
		ResolvePeer:              func(string) (PeerSession, bool) { return PeerSession{}, false },
		SessionName:              func() string { return self },
		SessionID:                func() string { return "sess-" + self },
		Policy:                   func() CollabPolicy { return CollabPolicy{Mailbox: true} },
		Store:                    func() *collab.Store { return local },
		StoreIfExists:            func() *collab.Store { return local },
		GlobalStore:              func() *collab.Store { return global },
		GlobalStoreIfExists:      func() *collab.Store { return global },
		Notifier:                 collab.NewNotifier(),
		TargetAllowsCrossProject: func(string) bool { return false },
	}}
}

func (s familySession) inbox() Inbox {
	d := s.deps
	return Inbox{
		Self: d.SessionName(), SelfID: d.SessionID(), Root: d.Workspace(), Policy: d.Policy(),
		Workspace: d.StoreIfExists, Global: d.GlobalStoreIfExists,
	}
}

// sees points s at peer, so a leave_note to peer's name resolves to its checkout.
func (s familySession) sees(peer familySession) familySession {
	name, id, ws := peer.deps.SessionName(), peer.deps.SessionID(), peer.deps.Workspace()
	s.deps.ResolvePeer = func(to string) (PeerSession, bool) {
		if to != name {
			return PeerSession{}, false
		}
		return PeerSession{Workspace: ws, ID: id}, true
	}
	return s
}

func familyGlobal(t *testing.T) *collab.Store {
	t.Helper()
	g, err := collab.OpenGlobalAt(filepath.Join(t.TempDir(), "collab-xproject.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	return g
}

func send(t *testing.T, from familySession, args string) string {
	t.Helper()
	out, err := NewLeaveNote(from.deps).Execute(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// mustReceive asserts that every reader agrees one note is waiting for s — the
// wake snapshot, the preview — and that check_messages then delivers body.
func mustReceive(t *testing.T, s familySession, body string) {
	t.Helper()
	ctx := context.Background()
	snap, err := s.inbox().Snapshot(ctx)
	if err != nil || snap.Count != 1 {
		t.Fatalf("wake snapshot = %+v, err %v; want one note — a delivered note nobody is woken for is the bug #635 is about", snap, err)
	}
	if peeked := s.inbox().Peek(ctx); len(peeked) != 1 {
		t.Fatalf("preview = %d notes; want 1", len(peeked))
	}
	out, err := NewCheckMessages(s.deps).Execute(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, body) {
		t.Fatalf("check_messages did not deliver %q:\n%s", body, out)
	}
}

func TestLeaveNote_WorktreeAndMainCheckoutNeedNoConsent(t *testing.T) {
	repo := familyRepo(t, t.TempDir())
	wt := familyWorktree(t, filepath.Join(repo, ".git"), "feature", filepath.Join(repo, "repo-wt-feature"))
	global := familyGlobal(t)
	alice := newFamilySession(t, repo, "alice", global)
	bob := newFamilySession(t, wt, "bob", global)
	bob = bob.sees(alice)
	alice = alice.sees(bob)

	out := send(t, bob, `{"to":"alice","body":"from the nested worktree"}`)
	if !strings.Contains(out, "same project") {
		t.Fatalf("the send must say why no opt-in was needed; got:\n%s", out)
	}
	mustReceive(t, alice, "from the nested worktree")

	// And back the other way, from the main checkout to the worktree.
	send(t, alice, `{"to":"bob","body":"from the main checkout"}`)
	mustReceive(t, bob, "from the main checkout")
}

func TestLeaveNote_FamilyThreadReplyWithoutConsent(t *testing.T) {
	// The reply path resolves the thread's participants from the daemon-level
	// store; without consent it must still find a same-project thread.
	repo := familyRepo(t, t.TempDir())
	wt := familyWorktree(t, filepath.Join(repo, ".git"), "feature", filepath.Join(t.TempDir(), "wt"))
	global := familyGlobal(t)
	alice := newFamilySession(t, repo, "alice", global)
	bob := newFamilySession(t, wt, "bob", global)
	bob = bob.sees(alice)
	alice = alice.sees(bob)

	send(t, bob, `{"to":"alice","body":"opening"}`)
	rows, err := alice.inbox().ClaimResult(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatalf("alice claimed %d, err %v", len(rows), err)
	}
	out := send(t, alice, `{"body":"the reply","conversation_id":`+jsonStr(rows[0].ConversationID)+`}`)
	if !strings.Contains(out, "Message sent") {
		t.Fatalf("a reply into a same-project thread must be sent; got:\n%s", out)
	}
	mustReceive(t, bob, "the reply")
}

func TestLeaveNote_ADeletedWorktreesNoteStillArrives(t *testing.T) {
	repo := familyRepo(t, t.TempDir())
	common := filepath.Join(repo, ".git")
	wt := familyWorktree(t, common, "gone", filepath.Join(t.TempDir(), "gone"))
	global := familyGlobal(t)
	alice := newFamilySession(t, repo, "alice", global)
	bob := newFamilySession(t, wt, "bob", global)
	bob = bob.sees(alice)

	send(t, bob, `{"to":"alice","body":"sent before the worktree was removed"}`)
	// `git worktree remove`: the checkout and its git directory both go.
	for _, dir := range []string{wt, filepath.Join(common, "worktrees", "gone")} {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
	}
	mustReceive(t, alice, "sent before the worktree was removed")
}

func TestLeaveNote_SubmoduleAndSuperprojectNeedNoConsent(t *testing.T) {
	// The live case behind the documented boundary change: ./plumb inside
	// plumb-ops, and a worktree of ./plumb.
	super := familyRepo(t, t.TempDir())
	sub := familySubmodule(t, super, "plumb")
	subWT := familyWorktree(t, filepath.Join(super, ".git", "modules", "plumb"), "fric", filepath.Join(super, "plumb-wt-fric"))
	global := familyGlobal(t)
	ops := newFamilySession(t, super, "ops", global)
	inSub := newFamilySession(t, sub, "insub", global)
	inWT := newFamilySession(t, subWT, "inwt", global)

	inSub = inSub.sees(ops)
	send(t, inSub, `{"to":"ops","body":"from the submodule"}`)
	mustReceive(t, ops, "from the submodule")

	inWT = inWT.sees(ops)
	send(t, inWT, `{"to":"ops","body":"from the submodule's worktree"}`)
	mustReceive(t, ops, "from the submodule's worktree")

	ops = ops.sees(inWT)
	send(t, ops, `{"to":"inwt","body":"from the superproject"}`)
	mustReceive(t, inWT, "from the superproject")
}

func TestLeaveNote_NestedButForeignStillNeedsConsent(t *testing.T) {
	repo := familyRepo(t, t.TempDir())
	plain := filepath.Join(repo, "tools", "plain")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	clone := familyRepo(t, filepath.Join(repo, "vendor", "clone"))
	global := familyGlobal(t)
	alice := newFamilySession(t, repo, "alice", global)

	for name, ws := range map[string]string{"a plain nested directory": plain, "an unrelated nested clone": clone} {
		sender := newFamilySession(t, ws, "carol", global)
		sender = sender.sees(alice)
		out := send(t, sender, `{"to":"alice","body":"from inside, but not of, the project"}`)
		if !strings.HasPrefix(out, "Not sent") || !strings.Contains(out, "cross_project") {
			t.Errorf("%s is another project: the send must be refused for want of consent; got:\n%s", name, out)
		}
	}
	if snap, err := alice.inbox().Snapshot(context.Background()); err != nil || snap.Count != 0 {
		t.Fatalf("nothing may have reached alice: %+v, err %v", snap, err)
	}
}

func TestWorkspaceSessions_FamilyVolumeNeedsNoConsent(t *testing.T) {
	// The roster's conversation volume reads the daemon-level store through the
	// same scope as delivery: a thread with this project's own worktree is shown
	// without consent, and another project's traffic aimed here is not.
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	repo := familyRepo(t, t.TempDir())
	wt := familyWorktree(t, filepath.Join(repo, ".git"), "feature", filepath.Join(t.TempDir(), "wt"))
	other := familyRepo(t, t.TempDir())
	local := openStore(t, repo)
	global := openGlobalStore(t)

	put(t, global, "stranger", "alice", "from another project", "", other, repo)
	if out := listing(t, observabilityTool(t, repo, "sess-alice", "alice", local, global, false)); strings.Contains(out, "conversation volume") {
		t.Fatalf("another project's traffic was disclosed without consent:\n%s", out)
	}

	bob := newFamilySession(t, wt, "bob", global)
	bob = bob.sees(familySession{deps: CollabDeps{
		SessionName: func() string { return "alice" }, SessionID: func() string { return "sess-alice" },
		Workspace: func() string { return repo },
	}})
	send(t, bob, `{"to":"alice","body":"from my worktree"}`)
	if out := listing(t, observabilityTool(t, repo, "sess-alice", "alice", local, global, false)); !strings.Contains(out, "conversation volume") {
		t.Fatalf("a thread with this project's own worktree must be shown without consent:\n%s", out)
	}
}

func TestLeaveNote_ConsentingReplyIntoAForeignThread(t *testing.T) {
	// The thread-reply path reads participants through the reader's scope, so
	// consent must reach it: a project that opted in replies into a thread with an
	// unrelated project without naming `to`, exactly as before #635.
	repo := familyRepo(t, t.TempDir())
	other := familyRepo(t, t.TempDir())
	global := familyGlobal(t)
	alice := newFamilySession(t, repo, "alice", global)
	alice.deps.Policy = func() CollabPolicy { return CollabPolicy{Mailbox: true, CrossProject: true} }
	carol := newFamilySession(t, other, "carol", global)
	carol.deps.TargetAllowsCrossProject = func(string) bool { return true }
	carol = carol.sees(alice)
	alice = alice.sees(carol)

	send(t, carol, `{"to":"alice","body":"from an unrelated project"}`)
	rows, err := alice.inbox().ClaimResult(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatalf("alice claimed %d, err %v; want carol's note", len(rows), err)
	}
	alice.deps.TargetAllowsCrossProject = func(string) bool { return true }
	out := send(t, alice, `{"body":"reply","conversation_id":`+jsonStr(rows[0].ConversationID)+`}`)
	if !strings.Contains(out, "Message sent") {
		t.Fatalf("a consenting participant must be able to reply into its cross-project thread; got:\n%s", out)
	}
}
