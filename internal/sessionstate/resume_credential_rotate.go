package sessionstate

// resume_credential_rotate.go — consuming a credential, counting failed
// presentations, and revoking (design §5: rotation, fencing, revocation).

import (
	"database/sql"
	"fmt"
	"time"
)

// ClaimResumeCredential consumes the CURRENT generation presentedHash names for the
// conversation externalID: it marks that one row superseded and nothing else. It
// reports won=false, changing nothing, when no current generation matches: a
// superseded or revoked secret, a secret for another conversation, or a claimant
// that lost the race for this one. consumed is the generation it took.
//
// The conditional UPDATE is the arbitration. It matches on the exact hash AND the
// current state and acts only if rows-affected says it landed, so two claimants
// presenting one secret cannot both be told they won, whatever order they arrive in.
// It is the FIRST step of a resume, ahead of any adoption or durable write: a loser
// has therefore applied nothing and has nothing to undo, and a claimant that finds the
// generation already moved is told so deterministically, however far the winner has
// got. A claimant whose restore then cannot complete gives the generation back with
// ReleaseResumeCredential; one that completes it calls IssueResumeSuccessor.
//
// A claim touches only the row presentedHash names. Every other current credential
// of the conversation, belonging to another identity, stays current: a connection
// can present the credential it holds for a conversation it merely claimed, and that
// must never be a way to revoke the credential of whoever really owns it. nil-safe.
func (s *Store) ClaimResumeCredential(externalID, presentedHash string) (won bool, consumed int64, err error) {
	if s == nil || externalID == "" || presentedHash == "" {
		return false, 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return false, 0, fmt.Errorf("sessionstate: claim resume credential: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(
		`UPDATE resume_credential SET state='superseded', updated_at=?
		  WHERE hash=? AND state='current'
		    AND proxy_session_id IN (SELECT proxy_session_id FROM session_names WHERE external_id=?)`,
		time.Now().UnixMilli(), presentedHash, externalID,
	)
	if err != nil {
		return false, 0, fmt.Errorf("sessionstate: claim resume credential: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, 0, fmt.Errorf("sessionstate: claim resume credential: %w", err)
	}
	if n == 0 {
		return false, 0, nil
	}
	if err := tx.QueryRow(`SELECT generation FROM resume_credential WHERE hash=?`, presentedHash).Scan(&consumed); err != nil {
		return false, 0, fmt.Errorf("sessionstate: claim resume credential: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, 0, fmt.Errorf("sessionstate: claim resume credential: %w", err)
	}
	return true, consumed, nil
}

// ReleaseResumeCredential gives back a generation ClaimResumeCredential took, for a
// claimant whose restore could not complete (the ID is held by a live session, the
// name is reserved elsewhere, the store hiccuped): a degraded attempt must leave the
// legitimate claimant's credential valid for the retry. It acts only on a SUPERSEDED
// row whose identity holds no other current generation, so a generation that has
// since been revoked, or overtaken by a newer one (a zombie serve that restored under
// its own proxy credential meanwhile), stays exactly where that left it. released
// reports whether the row went back to current. nil-safe.
func (s *Store) ReleaseResumeCredential(presentedHash string) (released bool, err error) {
	if s == nil || presentedHash == "" {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(
		`UPDATE resume_credential SET state='current', updated_at=?
		  WHERE hash=? AND state='superseded'
		    AND NOT EXISTS (SELECT 1 FROM resume_credential r2
		                     WHERE r2.proxy_session_id = resume_credential.proxy_session_id
		                       AND r2.state = 'current')`,
		time.Now().UnixMilli(), presentedHash,
	)
	if err != nil {
		return false, fmt.Errorf("sessionstate: release resume credential: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("sessionstate: release resume credential: %w", err)
	}
	return n > 0, nil
}

// IssueResumeSuccessor records hash as proxySessionID's CURRENT credential, with a
// generation past the one a claim consumed, and returns it. The generation is a
// per-conversation clock: the successor hangs off the claimant's own identity row,
// which is a different row from the one it consumed, and a clock that restarted there
// would stop proving newer-ness. Like every mint it supersedes that one identity's
// previous current generation and nothing else, and fails with ErrNoIdentityRecord
// when the proxy session has no identity row. nil-safe (an error: a nil store holds
// nothing).
func (s *Store) IssueResumeSuccessor(proxySessionID, hash string, consumed int64) (int64, error) {
	return s.mint(proxySessionID, hash, consumed)
}

// NoteFailedResumePresentation counts a presentation that matched no retained
// generation of a conversation that has a current credential, and revokes that
// credential on the ResumeFailureLimit-th. counted is false when there was nothing to
// count against (no credential for the conversation, or none current), so a
// presentation to a daemon that holds no record is never an attack. See
// ResumeFailureLimit for the griefing vector this accepts. nil-safe.
func (s *Store) NoteFailedResumePresentation(externalID string) (counted, revoked bool, err error) {
	if s == nil || externalID == "" {
		return false, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return false, false, fmt.Errorf("sessionstate: note failed resume presentation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UnixMilli()
	res, err := tx.Exec(
		`UPDATE resume_credential SET failures = failures + 1, updated_at=?
		  WHERE state='current'
		    AND proxy_session_id IN (SELECT proxy_session_id FROM session_names WHERE external_id=?)`,
		now, externalID,
	)
	if err != nil {
		return false, false, fmt.Errorf("sessionstate: note failed resume presentation: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, false, fmt.Errorf("sessionstate: note failed resume presentation: %w", err)
	}
	if n == 0 {
		return false, false, nil
	}
	res, err = tx.Exec(
		`UPDATE resume_credential SET state='revoked', updated_at=?
		  WHERE state='current' AND failures >= ?
		    AND proxy_session_id IN (SELECT proxy_session_id FROM session_names WHERE external_id=?)`,
		now, ResumeFailureLimit, externalID,
	)
	if err != nil {
		return false, false, fmt.Errorf("sessionstate: note failed resume presentation: %w", err)
	}
	revokedRows, err := res.RowsAffected()
	if err != nil {
		return false, false, fmt.Errorf("sessionstate: note failed resume presentation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, false, fmt.Errorf("sessionstate: note failed resume presentation: %w", err)
	}
	return true, revokedRows > 0, nil
}

// revokeCredentialsLocked kills every credential of one identity row. It runs inside
// SaveIdentity's transaction when a save REPLACES the row's external linkage, so
// revocation is a consequence of the linkage write and there is no detach path that
// needs machinery of its own. Caller holds s.mu.
func revokeCredentialsLocked(tx *sql.Tx, proxySessionID string) error {
	if _, err := tx.Exec(
		`UPDATE resume_credential SET state='revoked', updated_at=? WHERE proxy_session_id=? AND state <> 'revoked'`,
		time.Now().UnixMilli(), proxySessionID,
	); err != nil {
		return fmt.Errorf("sessionstate: revoke resume credentials: %w", err)
	}
	return nil
}
