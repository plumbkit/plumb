package sessionstate

// resume_credential_rotate.go — consuming a credential, counting failed
// presentations, and revoking (design §5: rotation, fencing, revocation).

import (
	"database/sql"
	"fmt"
	"time"
)

// RotateResumeCredential consumes the CURRENT generation presentedHash names for the
// conversation externalID and issues newHash as the successor under newProxyID's
// identity row, in one transaction. It reports won=false, changing nothing, when no
// current generation matches: a superseded or revoked secret, a secret for another
// conversation, or a claimant that lost the race for this one.
//
// The arbitration is the conditional UPDATE: it matches on the exact hash AND the
// current state and acts only if rows-affected says it landed. Two claimants
// presenting one secret therefore cannot both be told they won, whatever order they
// arrive in. The rest of the transaction leaves exactly one row holding a current
// generation per conversation (every other current credential of the conversation is
// superseded) and a successor whose generation is past the consumed one.
//
// A transaction that cannot finish — the successor has no identity row to hang off —
// consumes nothing. nil-safe.
func (s *Store) RotateResumeCredential(externalID, presentedHash, newProxyID, newHash string) (won bool, generation int64, err error) {
	if s == nil || externalID == "" || presentedHash == "" || newProxyID == "" || newHash == "" {
		return false, 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return false, 0, fmt.Errorf("sessionstate: rotate resume credential: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UnixMilli()
	res, err := tx.Exec(
		`UPDATE resume_credential SET state='superseded', updated_at=?
		  WHERE hash=? AND state='current'
		    AND proxy_session_id IN (SELECT proxy_session_id FROM session_names WHERE external_id=?)`,
		now, presentedHash, externalID,
	)
	if err != nil {
		return false, 0, fmt.Errorf("sessionstate: rotate resume credential: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, 0, fmt.Errorf("sessionstate: rotate resume credential: %w", err)
	}
	if n == 0 {
		return false, 0, nil
	}
	var consumed int64
	if err := tx.QueryRow(`SELECT generation FROM resume_credential WHERE hash=?`, presentedHash).Scan(&consumed); err != nil {
		return false, 0, fmt.Errorf("sessionstate: rotate resume credential: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE resume_credential SET state='superseded', updated_at=?
		  WHERE state='current'
		    AND proxy_session_id IN (SELECT proxy_session_id FROM session_names WHERE external_id=?)`,
		now, externalID,
	); err != nil {
		return false, 0, fmt.Errorf("sessionstate: rotate resume credential: %w", err)
	}
	generation, err = mintLocked(tx, newProxyID, newHash, consumed)
	if err != nil {
		return false, 0, err
	}
	if err := tx.Commit(); err != nil {
		return false, 0, fmt.Errorf("sessionstate: rotate resume credential: %w", err)
	}
	return true, generation, nil
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
