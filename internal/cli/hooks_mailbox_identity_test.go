package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
	"github.com/plumbkit/plumb/internal/mcp"
)

func mailboxProject(t *testing.T, root, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".plumb"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeIdentityFile(t, filepath.Join(root, ".plumb", "config.toml"), contents)
	if !strings.Contains(contents, "???") {
		grantExecTrust(t, root)
	}
}

func TestHookMailbox_ShardUsesRecipientProjectConsent(t *testing.T) {
	for _, tc := range []struct {
		name, ownerConfig, shardConfig string
		want                           int
		disabled                       bool
	}{
		{"owner on recipient off", "", "[collab]\ncross_project = false\n", 0, false},
		{"owner off recipient on", "[collab]\ncross_project = false\n", "", 1, false},
		{"recipient mailbox off", "", "[collab]\nmailbox = false\n", 0, true},
		{"recipient config malformed", "", "[collab]\nmailbox = ???\n", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			world := newIdentityWorld(t)
			world.cfg.Collab.CrossProject = true
			ownerRoot, shardRoot := identityRepo(t), identityRepo(t)
			if tc.ownerConfig != "" {
				mailboxProject(t, ownerRoot, tc.ownerConfig)
			}
			if tc.shardConfig != "" {
				mailboxProject(t, shardRoot, tc.shardConfig)
			}
			conn := world.conn("")
			conn.start("owner", ownerRoot, "owner", nil)
			conn.start("owner/subagent", shardRoot, "owner/subagent", map[string]any{"force": true})
			inbox, ok := conn.s.hookInbox("owner/subagent")
			if !ok || inbox.Root != shardRoot {
				t.Fatalf("known shard unresolved: %+v ok=%v", inbox, ok)
			}
			global := world.pool.acquireGlobal()
			_, err := global.PutNote(context.Background(), collab.NoteInput{
				AuthorSession: "peer", AuthorID: "peer-id", Addressee: inbox.Self, AddresseeID: inbox.SelfID,
				Body: "recipient-only cross-project body", TTL: time.Hour, TargetWorkspace: shardRoot, OriginWorkspace: ownerRoot,
			}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			registerMailboxConn(conn)
			reply, err := world.registry.mailboxProbe(context.Background(), hookMailboxRequest{SessionID: "owner/subagent"})
			if err != nil || reply.Report.Count != tc.want {
				t.Fatalf("wrong recipient consent: report=%+v err=%v, want count %d", reply, err, tc.want)
			}
			read, isErr := conn.call("owner/subagent", "check_messages", nil)
			if isErr || strings.Contains(read, "recipient-only cross-project body") != (tc.want > 0) {
				t.Fatalf("probe and explicit delivery disagree: %s error=%v", read, isErr)
			}
			if tc.disabled && !strings.Contains(read, "disabled") {
				t.Fatalf("unavailable recipient policy not disclosed: %s", read)
			}
		})
	}
}

func TestHookMailbox_InboxKeepsOneRootAcrossRepin(t *testing.T) {
	world := newIdentityWorld(t)
	conn := world.conn("")
	rootA, rootB := identityRepo(t), identityRepo(t)
	conn.start("owner", rootA, "owner", nil)
	before, ok := conn.s.hookInbox("owner")
	if !ok {
		t.Fatal("owner not resolved")
	}
	conn.start("owner", rootB, "owner", map[string]any{"force": true})
	for _, root := range []string{rootA, rootB} {
		store := world.pool.acquire(root)
		_, err := store.PutNote(context.Background(), collab.NoteInput{
			AuthorSession: "peer", AuthorID: "peer-id", Addressee: before.Self, AddresseeID: before.SelfID,
			Body: root, TTL: time.Hour,
		}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err := before.ClaimResult(context.Background())
	if err != nil || len(rows) != 1 || rows[0].Body != rootA {
		t.Fatalf("captured root A read re-pinned store B: rows=%+v err=%v", rows, err)
	}
	after, ok := conn.s.hookInbox("owner")
	if !ok {
		t.Fatal("re-pinned owner not resolved")
	}
	rows, err = after.ClaimResult(context.Background())
	if err != nil || len(rows) != 1 || rows[0].Body != rootB {
		t.Fatalf("new inbox did not read root B: rows=%+v err=%v", rows, err)
	}
}

func TestHookMailbox_PredecessorsBelongOnlyToOwner(t *testing.T) {
	world := newIdentityWorld(t)
	root := identityRepo(t)
	conn := world.conn("")
	conn.start("owner", root, "owner", nil)
	conn.s.mutate(func(v *sessionView) { v.inheritedSessionIDs = []string{"verified-predecessor"} })
	conn.start("owner/subagent", "", "owner/subagent", nil)
	store := world.pool.acquire(root)
	_, err := store.PutNote(context.Background(), collab.NoteInput{
		AuthorSession: "peer", AuthorID: "peer-id", Addressee: "old owner name", AddresseeID: "verified-predecessor",
		Body: "owner continuation only", TTL: time.Hour,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	owner, ok := conn.s.hookInbox("owner")
	if !ok || len(owner.InheritedIDs) != 1 {
		t.Fatal("owner lost verified predecessor")
	}
	sub, ok := conn.s.hookInbox("owner/subagent")
	if !ok || len(sub.InheritedIDs) != 0 {
		t.Fatal("subagent inherited owner rights")
	}
	ownerState, err := owner.Snapshot(context.Background())
	if err != nil || ownerState.Count != 1 {
		t.Fatalf("owner probe=%+v err=%v", ownerState, err)
	}
	subState, err := sub.Snapshot(context.Background())
	if err != nil || subState.Count != 0 {
		t.Fatalf("subagent saw owner mail=%+v err=%v", subState, err)
	}
}

func TestHookMailbox_RecipientAndRootAreOneSnapshot(t *testing.T) {
	world := newIdentityWorld(t)
	conn := world.conn("")
	rootA, rootB := identityRepo(t), identityRepo(t)
	conn.start("owner", rootA, "owner", nil)
	conn.start("owner/subagent", rootA, "owner/subagent", nil)
	ctx := mcp.WithLogicalAgent(context.Background(), "owner/subagent")
	sh := conn.s.shardFor(ctx)
	set := func(root, name, id string) {
		sh.mu.Lock()
		sh.root, sh.rosterFolder, sh.rosterName, sh.rosterID = root, root, name, id
		sh.mu.Unlock()
	}
	set(rootA, "alice", "alice-id")
	var group sync.WaitGroup
	group.Go(func() {
		for range 2000 {
			set(rootB, "bob", "bob-id")
			set(rootA, "alice", "alice-id")
		}
	})
	for range 2000 {
		who, root, _ := conn.s.mailboxRecipient(ctx, false)
		if (root == rootA && who.name == "alice" && who.id == "alice-id") ||
			(root == rootB && who.name == "bob" && who.id == "bob-id") {
			continue
		}
		t.Errorf("mixed recipient and workspace: root=%q recipient=%+v", root, who)
		break
	}
	group.Wait()
	sh.mu.Lock()
	sh.root, sh.rosterFolder = rootB, rootA
	sh.mu.Unlock()
	who, root, _ := conn.s.mailboxRecipient(ctx, false)
	if root != rootB || who.id != "" || who.name != "" {
		t.Fatalf("unfinished roster move borrowed an address: root=%q recipient=%+v", root, who)
	}
}
