package collab

// membership.go answers one question: did this caller take part in this
// conversation?
//
// It is split from store.go because the answer is a security boundary rather
// than a storage detail. A conversation id is an ADDRESS — a thread is written
// into by quoting it — and nothing downstream re-checks who is entitled to use
// one, so this predicate is the only thing standing between "knows an id" and
// "may write into that thread". Keeping it in one file keeps the read path
// (which threads a session may SEE) and the write path (which it may WRITE
// INTO) using literally the same rule, so they cannot drift apart.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// membershipPredicate builds "this caller took part in the thread" as SQL plus
// its arguments, so the placeholder count and the values bound to them cannot
// disagree — the same reason addresseeMatch is built this way.
//
// Every name arm is gated on the corresponding ID being EMPTY. That gate is the
// whole security of it. Names are reusable: a session that ends frees its name,
// and rename_session lets any live session take a free one. An ungated
// `addressee = ?` would therefore let an outsider join a fully id-bound thread
// simply by renaming itself to a departed participant's name — restoring both
// attacks this guard exists to stop. Gated, a name only ever speaks for a row
// that recorded no identity to speak for it, which is exactly the concession
// claimable already makes for delivery (`addressee_id = ” OR addressee_id IN
// (…)`), and no more.
//
// The claim arms exist because a "next" note has no other evidence of its
// recipient. Such a note is deliberately stored unbound, so the session that
// claimed it appears nowhere except in the claim — and "next" is the DEFAULT
// addressee, with RenderMessages printing the recipient a copy-pasteable reply
// naming that thread. Without them the ordinary reply flow refuses itself.
//
// They are id-gated for the same reason as the others, and this is the trap an
// independent review caught: ClaimNotes stamps the claim on EVERY row it hands
// over, not only "next" ones, so an ungated `delivered_to = ?` would give every
// note an unbounded name arm the moment its recipient read it — re-opening the
// rename bypass on essentially every live thread. delivered_to_id carries the
// claimant's identity for exactly this; the name is consulted only for claims
// recorded before that column existed.
//
// Both columns are written by ClaimNotes, never by a caller, and only after the
// row passed claimable — so this is the store's own record of who received it.
//
// An anonymous caller — no identities and no name — matches nothing. Returning a
// literal false rather than an empty predicate matters: an empty one would be
// dropped from the WHERE and admit everything.
func membershipPredicate(ids []string, name string) (string, []any) {
	var arms []string
	var args []any

	var known []string
	for _, id := range ids {
		if id != "" && !slices.Contains(known, id) {
			known = append(known, id)
		}
	}
	if len(known) > 0 {
		marks := strings.TrimSuffix(strings.Repeat("?, ", len(known)), ", ")
		for _, col := range []string{"author_id", "addressee_id"} {
			arms = append(arms, `(`+col+` != '' AND `+col+` IN (`+marks+`))`)
			for _, id := range known {
				args = append(args, id)
			}
		}
	}
	if len(known) > 0 {
		marks := strings.TrimSuffix(strings.Repeat("?, ", len(known)), ", ")
		arms = append(arms, `(delivered_to_id != '' AND delivered_to_id IN (`+marks+`))`)
		for _, id := range known {
			args = append(args, id)
		}
	}
	if name != "" {
		arms = append(arms,
			`(author_id = '' AND author_session = ?)`,
			`(addressee_id = '' AND addressee = ?)`,
			`(delivered_to_id = '' AND delivered_to != '' AND delivered_to = ?)`)
		args = append(args, name, name, name)
	}
	if len(arms) == 0 {
		return "0 = 1", nil
	}
	return "(" + strings.Join(arms, " OR ") + ")", args
}

// authorIdentities is every session ID that speaks for this note's author: its
// own, plus any it provably inherited. Always holds at least one element (the
// author's own, possibly empty), which membershipPredicate then filters.
func (in NoteInput) authorIdentities() []string {
	return append([]string{in.AuthorID}, in.AuthorInheritedIDs...)
}

// participantGuard suppresses the insert unless the author took part in the
// thread it names.
//
// Unlike the budget, membership does NOT filter on expires_at. Having been in a
// thread is a historical fact, and expiry is about what is still deliverable,
// not about who was there: a long exchange whose opening notes have aged out
// would otherwise lock its own participants out of it. Nothing is loosened by
// this — an outsider was never in the thread at any time, expired or not — and
// once the reaper actually deletes the rows there is no membership evidence
// left, so a genuinely dead id stops being a way in.
func participantGuard(ids []string, name string) (string, []any) {
	member, args := membershipPredicate(ids, name)
	return `EXISTS (SELECT 1 FROM collab_rows
	        WHERE kind = ? AND conversation_id = ? AND ` + member + `)`,
		append([]any{string(KindNote)}, args...)
}

// isParticipant reports whether authorID/authorSession appears in the thread.
// Used only to explain a refusal, never to authorise: the authorising check is
// participantGuard, inside the insert, where it cannot race.
func (s *Store) isParticipant(ctx context.Context, conv string, in NoteInput) (bool, error) {
	member, memberArgs := membershipPredicate(in.authorIdentities(), in.AuthorSession)
	args := append([]any{string(KindNote), conv}, memberArgs...)
	var one int
	//nolint:gosec // G202: member is generated by membershipPredicate from an
	// identity count, not from caller data — it contains only "?" placeholders and
	// fixed SQL, and every value is bound as a parameter.
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM collab_rows
		 WHERE kind = ? AND conversation_id = ? AND `+member+`
		 LIMIT 1`, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("collab: check participation: %w", err)
	}
	return true, nil
}
