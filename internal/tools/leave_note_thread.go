package tools

// leave_note_thread.go — in-thread reply addressing.
//
// A note that quotes a conversation_id but names no addressee is resolved here,
// by session IDENTITY rather than by name: names are reusable and renameable, so
// the IDs recorded on the thread's rows are what decide who the caller is and who
// the reply binds to. Split from leave_note.go, which owns the tool's contract,
// its routing and its send path.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
)

// threadPeer is one other participant in a thread: the stable session ID the
// rows carry for it, plus the name a reply would address it by. The ID is the
// binding; the name is only the address a fallback delivery uses.
//
// The ID is empty when every row naming this peer predates attribution, so the
// peer is known by name alone and the reply is written unbound — the most the
// thread can support for it.
type threadPeer struct {
	id   string
	name string
}

// resolveThreadAddressee answers "who am I replying to?" for a note that quotes
// a conversation_id but names no addressee. It returns either the addressee to
// use, or a refusal to show the caller — never a silent fallback.
//
// Falling back to "next" here is what this replaces, and it failed in both
// directions at once: the reply went to whoever attached next instead of the
// peer being answered, and since "next" matches any claimant, the author's own
// session was frequently the one that took it. The sender saw a success line
// naming a delivery that never happened.
//
// Refusing is the right failure. A reply the caller believes is addressed is
// worse than a reply the caller is told it must address, because only the second
// one is visible.
//
// It resolves by session IDENTITY, not by name. Names are reusable and a live
// session can rename to any free one, so a name comparison gets both directions
// wrong: after a supported rename_session the caller sees its OWN former name
// among the participants and its reply is refused as ambiguous, while a session
// that later draws a departed peer's name is treated as that peer. The rows
// carry AuthorID and AddresseeID; names are consulted only for rows that predate
// attribution, or that were addressed to a peer which was not live — the same
// concession claimable makes for delivery.
//
// It also REFUSES a thread the caller is not in, and names no one when it does.
// A conversation id is the thread's address, so answering "who is in this
// thread?" for any id presented would make this an enumeration oracle for
// exchanges between other agents — the metadata the observability path goes to
// length to withhold.
func (t *LeaveNote) resolveThreadAddressee(ctx context.Context, convID string) (to, peerID, refusal string) {
	participant, others := t.threadParticipants(ctx, convID)

	// Not our thread — or no thread at all. One message for both, deliberately:
	// distinguishing "does not exist" from "exists but is not yours" would tell a
	// caller which conversation ids are real.
	if !participant {
		return "", "", fmt.Sprintf(
			"Not sent: conversation %s is not one of yours, so there is nobody to reply to.\n\n"+
				"Either the id is wrong, or the thread expired — unread notes age out per [collab] "+
				"note_ttl_minutes (falling back to intent_ttl_minutes). Name the recipient explicitly "+
				"with `to`, or start a new thread by omitting conversation_id.", convID)
	}

	switch len(others) {
	case 1:
		return others[0].name, others[0].id, ""
	case 0:
		return "", "", fmt.Sprintf(
			"Not sent: conversation %s has no other participant on record — every note "+
				"in it is yours.\n\nName the recipient explicitly with `to`.", convID)
	default:
		// Safe to name them: the caller is a participant, so these are peers it is
		// already in a thread with.
		names := make([]string, 0, len(others))
		for _, o := range others {
			names = append(names, o.name)
		}
		slices.Sort(names)
		return "", "", fmt.Sprintf(
			"Not sent: conversation %s has %d other participants (%s), so \"reply to the "+
				"other one\" is ambiguous.\n\nName the recipient explicitly with `to`.",
			convID, len(names), strings.Join(names, ", "))
	}
}

// threadParticipants reads the thread and reports whether the caller is in it,
// plus the distinct other participants.
//
// Identity is what decides "is this me": the row's author/addressee ID when it
// carries one, and the name only when it does not. That ordering is the whole
// point — a name comparison gets it wrong in both directions, treating the
// caller as a stranger after a rename and a name-reuser as the caller.
//
// It returns each peer's ID alongside its name because a NAME is not a stable
// address: a peer that renamed since its last note no longer resolves by the
// name recorded here, and a reply bound only when the name happens to resolve
// is written UNBOUND — claimable by whoever later draws that name. The caller
// binds from the ID, so a rename between notes does not unpin the address.
//
// A peer is counted ONCE however its rows straddle attribution: an ID-bearing
// row adopts the name-only entry already standing for that peer. Two peers that
// genuinely share a name keep two IDs and stay two participants, which is the
// ambiguity the caller is still told about.
func (t *LeaveNote) threadParticipants(ctx context.Context, convID string) (participant bool, others []threadPeer) {
	isSelf := t.selfMatcher()
	byID := map[string]int{} // session ID -> index in others
	consider := func(id, name string) {
		name = strings.TrimSpace(name)
		if name == "" || name == collab.AddresseeNext {
			return
		}
		if isSelf(id, name) {
			participant = true
			return
		}
		others = mergeThreadPeer(others, byID, id, name)
	}

	// A cross-project thread lives in the daemon-level store, a same-project one
	// in the workspace's own, and the caller does not say which. The global store
	// is read only if it already exists AND this workspace has opted in to
	// cross-project mail: without that gate a session here could learn the
	// participants of a thread between two OTHER projects, which is precisely what
	// the consent setting exists to prevent.
	stores := []*collab.Store{t.deps.Store()}
	if t.deps.Policy().CrossProject {
		stores = append(stores, t.globalIfExists())
	}
	now := time.Now()
	for _, store := range stores {
		if store == nil {
			continue
		}
		rows, err := store.Conversation(ctx, convID, now)
		if err != nil {
			continue
		}
		for _, r := range rows {
			consider(r.AuthorID, r.AuthorSession)
			consider(r.AddresseeID, r.Addressee)
			// A note addressed to "next" names nobody, so its recipient appears only
			// in the claim — and "next" is the DEFAULT addressee, with RenderMessages
			// handing the claimant a ready-made reply quoting this thread. Without
			// this the ordinary reply flow would tell the recipient it is not theirs.
			// Passing the claimant's ID, not just its name, is what stops a later
			// session that took that name inheriting its place — and what keeps a
			// recipient that renamed itself from being listed as its own peer.
			consider(r.DeliveredToID, r.DeliveredTo)
		}
	}
	return participant, others
}

// mergeThreadPeer files one row's (id, name) under the participant it belongs
// to, returning the updated list.
//
// A row that predates attribution carries a name and no ID. It is the only
// trace of a peer not yet identified, and is already accounted for otherwise —
// so it adds a participant only when the name is new to the thread, and where
// two peers share that name it cannot say which one it is. An ID-bearing row
// whose peer is listed by name alone ADOPTS that entry: keyed on its own ID it
// counted the peer twice, once by name and once by ID, and refused a reply
// whose only other participant it was.
func mergeThreadPeer(peers []threadPeer, byID map[string]int, id, name string) []threadPeer {
	if id == "" {
		if len(threadPeerIndexes(peers, name)) == 0 {
			peers = append(peers, threadPeer{name: name})
		}
		return peers
	}
	if at, ok := byID[id]; ok {
		// A later row carries the peer's more recent name; keep it as the
		// address while the ID remains the binding.
		peers[at].name = name
		return peers
	}
	if at := threadPeerIndexes(peers, name); len(at) == 1 && peers[at[0]].id == "" {
		peers[at[0]].id = id
		peers[at[0]].name = name
		byID[id] = at[0]
		return peers
	}
	byID[id] = len(peers)
	return append(peers, threadPeer{id: id, name: name})
}

// threadPeerIndexes names the participants currently answering to name. Two of
// them sharing one is name reuse — a real ambiguity, not a duplicate.
func threadPeerIndexes(peers []threadPeer, name string) []int {
	var at []int
	for i, peer := range peers {
		if peer.name == name {
			at = append(at, i)
		}
	}
	return at
}

// selfMatcher answers "is this row me?" for one row's (id, name) pair.
//
// It trusts the ID when the row carries one and falls back to the name only
// when it does not. A name comparison alone is wrong in both directions: after a
// rename_session the caller stops recognising its own earlier notes, and a
// session that later draws a departed peer's name starts matching that peer's.
// Inherited predecessor identities count as self, so a restarted session still
// recognises the threads it was in before the restart.
func (t *LeaveNote) selfMatcher() func(id, name string) bool {
	selfName := t.deps.SessionName()
	selfIDs := map[string]bool{}
	if id := t.deps.sessionID(); id != "" {
		selfIDs[id] = true
	}
	if t.deps.InheritedSessionIDs != nil {
		for _, id := range t.deps.InheritedSessionIDs() {
			if id != "" {
				selfIDs[id] = true
			}
		}
	}
	return func(id, name string) bool {
		if id != "" {
			return selfIDs[id]
		}
		return selfName != "" && name == selfName
	}
}
