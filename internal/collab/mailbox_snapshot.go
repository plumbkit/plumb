package collab

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"time"
)

// MailboxSnapshot is metadata only. Fingerprint identifies the eligible row set,
// never a body, sender or thread; the query does not change delivery watermarks.
type MailboxSnapshot struct {
	Count       int
	Fingerprint string
}

// Snapshot uses delivery's exact predicate and hashes row IDs and arrival times.
// Arrival times distinguish a new store reusing IDs after storage recovery.
// Memory stays constant even for a large backlog; callers bound query time.
func (s *Store) Snapshot(ctx context.Context, who Claimant, now time.Time) (MailboxSnapshot, error) {
	if s == nil || s.db == nil || who.Name == "" {
		return MailboxSnapshot{}, nil
	}
	where, args := claimable(who, now)
	//nolint:gosec // G202: claimable emits fixed SQL and placeholder lists only;
	// every identity is bound as a parameter, as on ClaimNotes.
	rows, err := s.db.QueryContext(ctx, "SELECT id, created_at FROM collab_rows WHERE "+where+" ORDER BY id", args...)
	if err != nil {
		return MailboxSnapshot{}, fmt.Errorf("collab: mailbox snapshot: %w", err)
	}
	defer rows.Close()
	hash := sha256.New()
	var result MailboxSnapshot
	var encoded [8]byte
	for rows.Next() {
		var id, created uint64
		if err := rows.Scan(&id, &created); err != nil {
			return MailboxSnapshot{}, fmt.Errorf("collab: mailbox metadata: %w", err)
		}
		binary.BigEndian.PutUint64(encoded[:], id)
		_, _ = hash.Write(encoded[:])
		binary.BigEndian.PutUint64(encoded[:], created)
		_, _ = hash.Write(encoded[:])
		result.Count++
	}
	if err := rows.Err(); err != nil {
		return MailboxSnapshot{}, fmt.Errorf("collab: mailbox metadata: %w", err)
	}
	result.Fingerprint = hex.EncodeToString(hash.Sum(nil))
	return result, nil
}
