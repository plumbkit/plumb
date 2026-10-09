package collab

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestMailboxSnapshot_EligibilityAndWatermarks(t *testing.T) {
	store, root := openTestStore(t)
	ctx, now := context.Background(), time.Now()
	who := Claimant{Name: "renamed-alice", ID: "current", InheritedIDs: []string{"predecessor"}, Workspace: root}
	for _, note := range []NoteInput{
		{AuthorID: "peer", Addressee: "old-alice", AddresseeID: "current"},
		{AuthorID: "peer", Addressee: "old-alice", AddresseeID: "predecessor"},
		{AuthorID: "peer", Addressee: "renamed-alice"},
		{AuthorID: "peer", Addressee: AddresseeNext},
		{AuthorID: "current", Addressee: AddresseeNext},
		{AuthorID: "predecessor", Addressee: AddresseeNext},
		{AuthorID: "peer", Addressee: "renamed-alice", AddresseeID: "stranger"},
		{AuthorID: "peer", Addressee: "renamed-alice", TargetWorkspace: "/another-workspace"},
		{AuthorID: "peer", Addressee: "renamed-alice", TTL: -time.Second},
	} {
		note.AuthorSession, note.Body = "bob", "SECRET BODY never included in a probe"
		noteTime := now
		if note.TTL < 0 {
			note.TTL = time.Hour
			noteTime = now.Add(-2 * time.Hour)
		}
		mustPut(t, store, note, noteTime)
	}
	before, err := store.Snapshot(ctx, who, now)
	if err != nil || before.Count != 4 || len(before.Fingerprint) != 64 || strings.Contains(before.Fingerprint, "SECRET") {
		t.Fatalf("snapshot = %+v, err=%v", before, err)
	}
	rows, err := store.ClaimNotes(ctx, who, now, 0)
	if err != nil || len(rows) != before.Count {
		t.Fatalf("probe changed eligibility or watermarks: rows=%d err=%v", len(rows), err)
	}
	after, err := store.Snapshot(ctx, who, now)
	if err != nil || after.Count != 0 || after.Fingerprint == before.Fingerprint {
		t.Fatalf("delivered rows still pending: %+v, err=%v", after, err)
	}
	mustPut(t, store, NoteInput{AuthorID: "peer", AuthorSession: "bob", Addressee: AddresseeNext, Body: "new arrival"}, now)
	newState, err := store.Snapshot(ctx, who, now)
	if err != nil || newState.Count != 1 || newState.Fingerprint == before.Fingerprint || newState.Fingerprint == after.Fingerprint {
		t.Fatalf("new arrival did not change fingerprint: %+v err=%v", newState, err)
	}
}

func TestMailboxSnapshot_FailureIsNotEmpty(t *testing.T) {
	store, _ := openTestStore(t)
	_ = store.Close()
	if _, err := store.Snapshot(context.Background(), Claimant{Name: "alice", ID: "id"}, time.Now()); err == nil {
		t.Fatal("closed store asserted empty")
	}
}

func TestMailboxSnapshot_RecoveryReusingIDsStillChangesState(t *testing.T) {
	first, _ := openTestStore(t)
	second, _ := openTestStore(t)
	now := time.Now()
	note := NoteInput{AuthorID: "peer", AuthorSession: "bob", Addressee: "alice", Body: "arrival"}
	mustPut(t, first, note, now)
	mustPut(t, second, note, now.Add(time.Millisecond))
	who := Claimant{Name: "alice", ID: "id"}
	a, err := first.Snapshot(context.Background(), who, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	b, err := second.Snapshot(context.Background(), who, now.Add(time.Second))
	if err != nil || a.Count != b.Count || a.Fingerprint == b.Fingerprint {
		t.Fatalf("new arrivals with reused IDs indistinguishable: a=%+v b=%+v err=%v", a, b, err)
	}
}
