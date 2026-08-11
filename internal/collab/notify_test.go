package collab

import (
	"context"
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
