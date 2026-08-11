package collab

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestNotifier_GenAdvancesPerRecipient: the generation counter is what lets the
// hot delivery path skip a database read, so it must move for the addressed
// recipient and stay put for everyone else.
func TestNotifier_GenAdvancesPerRecipient(t *testing.T) {
	n := NewNotifier()
	before := n.Gens([]string{"alice", "bob"})
	n.Bump("alice")
	after := n.Gens([]string{"alice", "bob"})

	if after[0] == before[0] {
		t.Error("the addressed recipient's generation must advance")
	}
	if after[1] != before[1] {
		t.Error("an unrelated recipient's generation must not move — that would force a needless query")
	}
}

// TestNotifier_WaitWakesOnBump is the turn-taking primitive: a session parked on
// its inbox is released by a peer's send.
func TestNotifier_WaitWakesOnBump(t *testing.T) {
	n := NewNotifier()
	keys := []string{"alice", AddresseeNext}
	since := n.Gens(keys)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan bool, 1)
	go func() { done <- n.Wait(ctx, keys, since) }()

	// Give the waiter a moment to park, then send.
	time.Sleep(20 * time.Millisecond)
	n.Bump("alice")

	select {
	case woke := <-done:
		if !woke {
			t.Fatal("Wait returned false despite a bump for a watched key")
		}
	case <-ctx.Done():
		t.Fatal("Wait did not return after a bump")
	}
}

// TestNotifier_WaitTimesOutWithoutBump: no message means the wait expires
// cleanly rather than hanging until the client's own call timeout.
func TestNotifier_WaitTimesOutWithoutBump(t *testing.T) {
	n := NewNotifier()
	keys := []string{"alice"}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if n.Wait(ctx, keys, n.Gens(keys)) {
		t.Error("Wait should report false when nothing arrived")
	}
}

// TestNotifier_WaitSeesBumpBetweenSnapshotAndPark closes the race that would
// otherwise lose a message: the caller snapshots generations, a peer sends, and
// only then does the caller park. Wait re-checks under the lock, so it must
// return immediately rather than sleeping through a message already written.
func TestNotifier_WaitSeesBumpBetweenSnapshotAndPark(t *testing.T) {
	n := NewNotifier()
	keys := []string{"alice"}
	since := n.Gens(keys)
	n.Bump("alice") // arrives before the caller parks

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if !n.Wait(ctx, keys, since) {
		t.Error("a bump between the snapshot and the park must not be missed")
	}
}

// TestNotifier_ConcurrentWaitersAllWake: several sessions may watch the same
// "next" address, and a single send must release all of them.
func TestNotifier_ConcurrentWaitersAllWake(t *testing.T) {
	n := NewNotifier()
	keys := []string{AddresseeNext}
	since := n.Gens(keys)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	woke := make([]bool, 4)
	for i := range woke {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			woke[i] = n.Wait(ctx, keys, since)
		}(i)
	}
	time.Sleep(30 * time.Millisecond)
	n.Bump(AddresseeNext)
	wg.Wait()

	for i, ok := range woke {
		if !ok {
			t.Errorf("waiter %d did not wake", i)
		}
	}
}

// TestNotifyKey_NextIsScopedToWorkspace: "whoever attaches next" is a per-project
// address, so the key it is watched under must be too. One shared key made a note
// left in any workspace wake every connection in the daemon, each then paying a
// SQLite claim to find out the note was never for it.
func TestNotifyKey_NextIsScopedToWorkspace(t *testing.T) {
	n := NewNotifier()
	mine := NotifyKey("/proj/mine", AddresseeNext)
	theirs := NotifyKey("/proj/theirs", AddresseeNext)
	keys := []string{mine, theirs}

	before := n.Gens(keys)
	n.Bump(theirs)
	after := n.Gens(keys)

	if after[1] == before[1] {
		t.Error("the addressed workspace's 'next' generation must advance")
	}
	if after[0] != before[0] {
		t.Error("a 'next' note in another workspace must not wake sessions pinned elsewhere")
	}
	if got := NotifyKey("/proj/mine", "bob"); got != "bob" {
		t.Errorf("a session name is a daemon-wide address and must be its own key; got %q", got)
	}
	if got := NotifyKey("", AddresseeNext); got != AddresseeNext {
		t.Errorf("with no workspace there is nothing to scope to; got %q", got)
	}
}

// TestNotifier_GenMapIsBounded: an entry is created by the mere act of sending to
// a name, and the daemon runs for weeks, so the map must not grow for as long as
// the process lives.
func TestNotifier_GenMapIsBounded(t *testing.T) {
	n := NewNotifier()
	for i := range maxTrackedKeys * 2 {
		n.Bump("peer-" + strconv.Itoa(i))
	}
	n.mu.Lock()
	size := len(n.gen)
	n.mu.Unlock()
	if size > maxTrackedKeys {
		t.Errorf("tracked %d keys after %d distinct recipients; the map must stay bounded",
			size, maxTrackedKeys*2)
	}
}

// TestNotifier_EvictionCannotSuppressDelivery is the invariant that makes
// eviction safe at all. A session snapshots a generation, its entry is evicted
// while it works, and the peer then sends: the new stamp must still read as newer
// than the snapshot. A per-key counter would restart at 1 and read as OLDER,
// leaving check_messages parked for its whole wait while the answer sat in the
// database.
func TestNotifier_EvictionCannotSuppressDelivery(t *testing.T) {
	n := NewNotifier()
	keys := []string{"alice"}
	n.Bump("alice")
	since := n.Gens(keys)

	// Push alice out of the map: she is the oldest entry and has no waiter.
	for i := range maxTrackedKeys * 2 {
		n.Bump("filler-" + strconv.Itoa(i))
	}
	n.mu.Lock()
	_, stillTracked := n.gen["alice"]
	n.mu.Unlock()
	if stillTracked {
		t.Fatal("precondition: the eviction under test did not drop the snapshotted key")
	}

	n.Bump("alice")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if !n.Wait(ctx, keys, since) {
		t.Error("a send after an eviction must still read as newer than the pre-eviction snapshot")
	}
}

// TestNotifier_NilSafe: the notifier is optional wiring, so every accessor must
// tolerate a nil receiver rather than forcing a guard at each call site.
func TestNotifier_NilSafe(t *testing.T) {
	var n *Notifier
	n.Bump("alice")
	if g := n.Gen("alice"); g != 0 {
		t.Errorf("nil Gen = %d, want 0", g)
	}
	if got := n.Gens([]string{"a", "b"}); len(got) != 2 {
		t.Errorf("nil Gens returned %d entries, want 2", len(got))
	}
}
