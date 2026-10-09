package cli

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
	"github.com/plumbkit/plumb/internal/sqlitex"
	"github.com/plumbkit/plumb/internal/tools"
)

func TestHookMailbox_FirstMessageWithOtherStoreAbsent(t *testing.T) {
	for _, global := range []bool{false, true} {
		t.Run(map[bool]string{false: "local with no global", true: "global with no local"}[global], func(t *testing.T) {
			world := newIdentityWorld(t)
			world.cfg.Collab.CrossProject = true
			root := identityRepo(t)
			recipient := world.conn("")
			recipient.start("conversation", root, "conversation", nil)
			registerMailboxConn(recipient)
			var store *collab.Store
			absent := collab.GlobalDBPath()
			if global {
				store = world.pool.acquireGlobal()
				absent = collab.DBPath(root)
			} else {
				store = world.pool.acquire(root)
			}
			_, err := store.PutNote(context.Background(), collab.NoteInput{
				AuthorSession: "peer", AuthorID: "peer-id", Addressee: recipient.s.sessionName(),
				AddresseeID: recipient.s.sessionID(), Body: "first valid note", TTL: time.Hour,
				TargetWorkspace: root, OriginWorkspace: "/peer",
			}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			reply, err := world.registry.mailboxProbe(context.Background(), hookMailboxRequest{SessionID: "conversation", Stop: true})
			if err != nil || !reply.Notify || reply.Report.Count != 1 {
				t.Fatalf("absent other store suppressed first note: %+v err=%v", reply, err)
			}
			if _, err := os.Stat(absent); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("probe created the absent mailbox: %v", err)
			}
			read, isErr := recipient.call("conversation", "check_messages", nil)
			if isErr || !strings.Contains(read, "first valid note") {
				t.Fatalf("probe consumed first note: %s error=%v", read, isErr)
			}
		})
	}
}

func TestHookMailbox_ColdLockedStoreIsUnavailableWithoutOpen(t *testing.T) {
	world := newIdentityWorld(t)
	rootA, rootB := identityRepo(t), identityRepo(t)
	alice, bob := world.conn(""), world.conn("")
	alice.start("alice", rootA, "alice", nil)
	bob.start("bob", rootB, "bob", nil)
	registerMailboxConn(alice)
	registerMailboxConn(bob)
	cold, err := collab.Open(rootA)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cold.Close() })
	_, err = cold.PutNote(context.Background(), collab.NoteInput{
		AuthorSession: "peer", AuthorID: "peer-id", Addressee: alice.s.sessionName(),
		AddresseeID: alice.s.sessionID(), Body: "cold valid note", TTL: time.Hour,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	healthy := world.pool.acquire(rootB)
	_, err = healthy.PutNote(context.Background(), collab.NoteInput{
		AuthorSession: "peer", AuthorID: "peer-id", Addressee: bob.s.sessionName(),
		AddresseeID: bob.s.sessionID(), Body: "healthy recipient", TTL: time.Hour,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	locker, err := sqlitex.Open(collab.DBPath(rootA), sqlitex.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Close()
	if _, err := locker.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = locker.Exec("ROLLBACK") }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, probeErr := world.registry.mailboxProbe(ctx, hookMailboxRequest{SessionID: "alice", Stop: true})
		done <- probeErr
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("cold existing mailbox was asserted readable")
		}
	case <-time.After(time.Second):
		// Release the fixture before reporting failure so no test goroutine leaks.
		_, _ = locker.Exec("ROLLBACK")
		<-done
		t.Error("hook opened SQLite and outlived its cancellation")
	}
	reply, err := world.registry.mailboxProbe(context.Background(), hookMailboxRequest{SessionID: "bob", Stop: true})
	if err != nil || !reply.Notify || reply.Report.Count != 1 {
		t.Fatalf("cold recipient interfered with healthy mailbox: %+v err=%v", reply, err)
	}
	if world.pool.stores[rootA] != nil {
		t.Fatal("hook cached a new SQLite handle")
	}
	_, _ = locker.Exec("ROLLBACK")
	read, isErr := alice.call("alice", "check_messages", nil)
	if isErr || !strings.Contains(read, "cold valid note") || world.pool.stores[rootA] == nil {
		t.Fatalf("explicit delivery failed to open the cold mailbox: %s error=%v", read, isErr)
	}
}

func TestHookMailbox_BusyPoolAllowsCompletion(t *testing.T) {
	world := newIdentityWorld(t)
	recipient := world.conn("")
	recipient.start("conversation", identityRepo(t), "conversation", nil)
	registerMailboxConn(recipient)
	world.pool.mu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := world.registry.mailboxProbe(ctx, hookMailboxRequest{SessionID: "conversation", Stop: true})
		done <- err
	}()
	select {
	case err := <-done:
		world.pool.mu.Unlock()
		if err == nil {
			t.Fatal("busy pool was asserted empty")
		}
	case <-time.After(time.Second):
		world.pool.mu.Unlock()
		<-done
		t.Fatal("hook waited on the pool mutex past cancellation")
	}
}

func TestMailboxProbe_SlowRecipientDoesNotDelayOtherRecipients(t *testing.T) {
	for _, delayResolution := range []bool{false, true} {
		t.Run(map[bool]string{false: "query", true: "resolution"}[delayResolution], func(t *testing.T) {
			registry := newConnRegistry()
			store, err := collab.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			_, err = store.PutNote(context.Background(), collab.NoteInput{
				AuthorSession: "peer", AuthorID: "peer-id", Addressee: "bob", AddresseeID: "bob-id",
				Body: "independent recipient", TTL: time.Hour,
			}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			block := func() { once.Do(func() { close(entered) }); <-release }
			registry.add("recipients", connHandle{mailboxInbox: func(external string) (tools.Inbox, bool) {
				if external == "alice" && delayResolution {
					block()
				}
				return tools.Inbox{
					Self: external, SelfID: external + "-id", Root: "/ws", Policy: tools.CollabPolicy{Mailbox: true},
					WorkspaceReader: func() (*collab.Store, error) {
						if external == "alice" && !delayResolution {
							block()
						}
						return store, nil
					},
				}, true
			}})
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _ = registry.mailboxProbe(context.Background(), hookMailboxRequest{SessionID: "alice", Stop: true})
			}()
			<-entered
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			reply, err := registry.mailboxProbe(ctx, hookMailboxRequest{SessionID: "bob", Stop: true})
			close(release)
			<-done
			if err != nil || !reply.Notify || reply.Report.Count != 1 {
				t.Fatalf("unrelated recipient held up by slow probe: %+v err=%v", reply, err)
			}
		})
	}
}
