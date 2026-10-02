package sessionstate

// resume_credential.go — the daemon's half of the resume credential
// (docs/identity-resume-credential-design.md).
//
// A resume credential proves the same fact the proxy session ID proves — "I am the
// process entrusted with this conversation" — but it outlives the serve process that
// first held it, so it has to cross the wire (disclosed in an initialize or tool
// result `_meta`, presented back in a request `_meta`). That makes it a bearer secret
// a client can copy, and the whole shape of this file follows from pricing that
// honestly:
//
//   - Generated, never derived. The secret is 128 random bits from crypto/rand with
//     no input at all: every value a client can see (the proxy ID, the external ID,
//     the name) is a claim, and deriving a secret from claims would launder a claim
//     into a proof.
//   - Hash only. The table holds SHA-256 of the secret, so a dump of this database
//     discloses nothing a client could present.
//   - One live generation per identity, the previous one retained so a replay can be
//     recognised, anything older pruned. There is NO age expiry: the outage this
//     exists to survive (a machine off for days) is exactly what a TTL would break.
//     Staleness is bounded by rotation and abuse by revocation.
//   - Rotation is a single conditional UPDATE. The generation move matches on the
//     exact hash AND the current state and acts only if rows-affected says it
//     landed, which is the arbitration between two claimants of one secret.
//   - A credential hangs off an identity row (session_names, keyed by the proxy
//     session ID) and is found through that row's external linkage. It never
//     authorises on the strength of a claim: the caller must present the secret.
//
// Only PROVEN branches (established or restored under a proxy credential) may mint,
// write or consume anything here. That rule lives in the callers
// (internal/cli/conn_resume_*.go); the store enforces what it can see, which is that
// a credential cannot exist without an identity row.

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// ResumeSecretPrefix starts every resume credential. The distinct, versioned shape
// is what lets the credential-leak scan and internal/redact match it exactly.
const ResumeSecretPrefix = "rsk1-"

// ResumeFailureLimit is how many presentations that match no retained generation a
// conversation's current credential survives. The third revokes it.
//
// The counter has a named griefing vector, accepted with bounded harm: the request
// `_meta` is client-settable and conversation IDs are client-visible claims, so any
// connection can present junk against a VICTIM's conversation and revoke its
// credential. The worst case is the status quo ante (name-only continuity until the
// victim re-establishes one) and the revocation is logged. Hardening it needs an
// authority the client cannot forge, which is Design B's problem, not this
// counter's.
const ResumeFailureLimit = 3

// ErrNoIdentityRecord is returned when a credential is minted for a proxy session
// that has no identity record. A credential nothing can resolve is worse than none.
var ErrNoIdentityRecord = errors.New("sessionstate: no identity record to hang a resume credential on")

// CredentialState is where one generation of a credential stands.
type CredentialState string

const (
	// CredentialCurrent is the one live generation of an identity.
	CredentialCurrent CredentialState = "current"
	// CredentialSuperseded was current until a rotation, a later mint, or a newer
	// generation elsewhere in the same conversation replaced it. Presenting it is a
	// replay, answered and never punished by the failure counter.
	CredentialSuperseded CredentialState = "superseded"
	// CredentialRevoked was killed: its linkage was replaced, or the failed-ownership
	// counter reached its limit.
	CredentialRevoked CredentialState = "revoked"
)

// CredentialLookup is what a presented secret resolved to for a conversation.
type CredentialLookup struct {
	// State is the matched generation's state, "" when the secret matched no
	// retained generation of the conversation.
	State CredentialState
	// ProxySessionID is the identity row the matched generation hangs off. Empty when
	// State is.
	ProxySessionID string
	// Generation is the matched generation. Zero when State is empty.
	Generation int64
	// HasCurrent reports that the conversation has at least one CURRENT credential.
	// It is what makes an unmatched presentation count toward revocation: a
	// presentation to a daemon with no credential for the conversation (a re-homed
	// machine) is not an attack and counts for nothing.
	HasCurrent bool
}

// NewResumeSecret returns a fresh resume credential: ResumeSecretPrefix and 22
// base64url characters (128 bits, unpadded) from crypto/rand. It takes no input,
// which is how "never derived from anything" is enforced.
func NewResumeSecret() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("sessionstate: generate resume credential: %w", err)
	}
	return ResumeSecretPrefix + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// HashResumeSecret is the stored form of a secret: lowercase hex SHA-256. A 128-bit
// random secret has no dictionary, so a plain hash is not reversible in any sense
// that matters.
func HashResumeSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// MintResumeCredential records hash as proxySessionID's CURRENT credential and
// returns its generation. The previous current generation becomes superseded, and
// anything older than that is pruned. It fails with ErrNoIdentityRecord when the
// proxy session has no identity row. nil-safe (an error: a nil store holds nothing).
func (s *Store) MintResumeCredential(proxySessionID, hash string) (int64, error) {
	if s == nil || proxySessionID == "" || hash == "" {
		return 0, errors.New("sessionstate: mint resume credential: no store, proxy session or hash")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("sessionstate: mint resume credential: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	gen, err := mintLocked(tx, proxySessionID, hash, 0)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("sessionstate: mint resume credential: %w", err)
	}
	return gen, nil
}

// mintLocked inserts the new current generation inside tx. atLeast carries the
// generation a rotation consumed, so the successor is past it even when it hangs off
// a different identity row (a replacement serve has a new proxy session ID): the
// generation is a per-conversation clock, and a clock that restarted would stop
// proving newer-ness. Caller holds s.mu.
func mintLocked(tx *sql.Tx, proxySessionID, hash string, atLeast int64) (int64, error) {
	var rows int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM session_names WHERE proxy_session_id=?`, proxySessionID).Scan(&rows); err != nil {
		return 0, fmt.Errorf("sessionstate: mint resume credential: %w", err)
	}
	if rows == 0 {
		return 0, ErrNoIdentityRecord
	}
	var top int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(generation), 0) FROM resume_credential WHERE proxy_session_id=?`, proxySessionID).Scan(&top); err != nil {
		return 0, fmt.Errorf("sessionstate: mint resume credential: %w", err)
	}
	next := max(top, atLeast) + 1
	now := time.Now().UnixMilli()
	if _, err := tx.Exec(
		`UPDATE resume_credential SET state='superseded', updated_at=? WHERE proxy_session_id=? AND state='current'`,
		now, proxySessionID,
	); err != nil {
		return 0, fmt.Errorf("sessionstate: mint resume credential: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO resume_credential (proxy_session_id, generation, hash, state, failures, updated_at)
		 VALUES (?, ?, ?, 'current', 0, ?)`,
		proxySessionID, next, hash, now,
	); err != nil {
		return 0, fmt.Errorf("sessionstate: mint resume credential: %w", err)
	}
	if err := pruneCredentialsLocked(tx); err != nil {
		return 0, err
	}
	return next, nil
}

// pruneCredentialsLocked keeps, per identity row, the current generation and the
// newest non-current one (the immediately previous: the one a replay presents), and
// drops the rest. Caller holds s.mu.
func pruneCredentialsLocked(tx *sql.Tx) error {
	if _, err := tx.Exec(
		`DELETE FROM resume_credential
		  WHERE state <> 'current'
		    AND EXISTS (SELECT 1 FROM resume_credential r2
		                 WHERE r2.proxy_session_id = resume_credential.proxy_session_id
		                   AND r2.state <> 'current'
		                   AND r2.generation > resume_credential.generation)`,
	); err != nil {
		return fmt.Errorf("sessionstate: prune resume credentials: %w", err)
	}
	return nil
}

// LookupResumeCredential resolves a presented secret's hash for the conversation
// externalID. It is read-only: nothing is consumed, counted or revoked here, which is
// what lets a degraded acceptance leave the generation exactly where it was.
//
// A CURRENT or SUPERSEDED generation matches only through an identity row linked to
// externalID: a secret is evidence for the conversation its row answers to, never a
// wildcard, and a blank externalID matches nothing. A REVOKED generation matches on
// the hash alone, because revocation is often a linkage that has since moved or been
// replaced, so the row can no longer be found through the conversation, and the
// holder is owed "revoked" rather than silence. That is no oracle: only a holder of
// the secret can present it. nil-safe.
func (s *Store) LookupResumeCredential(externalID, hash string) (CredentialLookup, error) {
	if s == nil || externalID == "" || hash == "" {
		return CredentialLookup{}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var lk CredentialLookup
	var state string
	var linked sql.NullString
	err := s.db.QueryRow(
		`SELECT c.proxy_session_id, c.generation, c.state, n.external_id
		   FROM resume_credential c
		   LEFT JOIN session_names n ON n.proxy_session_id = c.proxy_session_id
		  WHERE c.hash = ?`,
		hash,
	).Scan(&lk.ProxySessionID, &lk.Generation, &state, &linked)
	switch {
	case err == nil:
		if CredentialState(state) == CredentialRevoked || linked.String == externalID {
			lk.State = CredentialState(state)
			return lk, nil
		}
		// A live generation of some other conversation: not a match for this one.
		lk = CredentialLookup{}
	case errors.Is(err, sql.ErrNoRows):
		// fall through: no match, but is there anything to count a failure against?
	default:
		return CredentialLookup{}, fmt.Errorf("sessionstate: look up resume credential: %w", err)
	}
	var current int
	if err := s.db.QueryRow(
		`SELECT COUNT(*)
		   FROM resume_credential c
		   JOIN session_names n ON n.proxy_session_id = c.proxy_session_id
		  WHERE n.external_id = ? AND c.state = 'current'`,
		externalID,
	).Scan(&current); err != nil {
		return CredentialLookup{}, fmt.Errorf("sessionstate: look up resume credential: %w", err)
	}
	lk.HasCurrent = current > 0
	return lk, nil
}
