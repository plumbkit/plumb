package cli

// conn_restore.go — recovering a connection's identity, as ONE operation.
//
// This used to be three: restoreName applied the persisted name, adoptSessionID
// separately adopted the replayed ID, and persistName ran between and after them
// on several paths — including the refusal paths. That arrangement had a defect
// no single function could see: a TRANSIENT problem (the predecessor connSession
// had not finished detaching, so its name and ID were still held) reached a
// persistName that recorded the temporary replacement identity over the proven
// one. The next reconnect then restored the replacement, and the original ID,
// name and mailbox were durably gone. A momentary overlap became a permanent
// identity fork.
//
// So restoration is a single operation with one rule: the durable record is the
// authority, and nothing but a legitimate rename or a genuine first contact may
// write to it. Every failure below preserves the record exactly as it was and
// classifies the outcome, because next time the overlap will have cleared and
// the same record will restore correctly.
//
// The authorisation is unchanged and is the whole security argument: the proxy
// session ID is a 122-bit secret the serve process generates for itself, replays
// only inside its own initialize handshake, and which plumb never writes to a
// session file, a log line, or any tool result. It selects the record. A plumb
// session ID or a session NAME authorises nothing — both are disclosed to
// clients, so presenting one is a claim, never proof.

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/sessionstate"
)

// recoveryOutcome classifies what happened to this connection's identity during
// the initialize exchange. It is reported to the proxy in the initialize result
// `_meta`, which is what lets a reconnect note state an outcome instead of
// asserting one.
//
// The values are a deliberate four-way split rather than a boolean, because
// "not restored" covers two situations an agent must not confuse: continuity was
// never on offer (no credential, persistence disabled), or it was on offer and
// could not be delivered this time. The first is a configuration fact; the
// second is a transient failure that the next reconnect will probably resolve.
type recoveryOutcome string

const (
	// recoveryUnavailable: no durable continuity is possible for this
	// connection — no proxy credential (an ordinary MCP client rather than
	// `plumb serve`), or [session] persist_state is off. The session works
	// normally; it simply must not claim it will come back as itself.
	recoveryUnavailable recoveryOutcome = "unavailable"
	// recoveryEstablished: first contact under this credential. An identity was
	// minted and durably committed, so the NEXT reconnect can recover it.
	recoveryEstablished recoveryOutcome = "established"
	// recoveryRestored: a durable record was found and this connection is now
	// that session — same internal ID, same name, same authorised linkage.
	recoveryRestored recoveryOutcome = "restored"
	// recoveryDegraded: a durable record exists and was NOT fully applied. The
	// record is preserved untouched; this connection runs under a temporary
	// identity and a later reconnect will try again.
	recoveryDegraded recoveryOutcome = "degraded"
)

// restoreIdentity resolves this connection's identity from the durable record
// selected by its proxy session ID, and is the only path that may do so.
//
// Order is fixed and matters:
//
//  1. Resolve the authenticated record. No record means first contact.
//  2. Adopt the proven internal session ID, carrying its authorised external
//     linkage. Identity before name: everything the rename then touches (the
//     session file, the stats row, the durable record) keys on the ID, so
//     doing it the other way round writes the name against an ID that is about
//     to be replaced.
//  3. Restore the name, which is a mailbox address and must follow the ID that
//     mail is bound to.
//  4. Commit only on first contact. A successful restore has nothing to write —
//     the record already says exactly this — and a failed one must not write.
//
// It runs during initialize, before OnInit attaches a workspace, so it may not
// depend on any attach state.
func (s *connSession) restoreIdentity(proxyID string) {
	v := s.view()
	if !s.namePersistEnabled(v) {
		s.setRecovery(recoveryUnavailable)
		return
	}
	rec, ok, err := s.sessionState.LoadIdentity(proxyID)
	if err != nil {
		// An unreadable store is not evidence that this proxy is new. Claiming
		// first contact here would mint a replacement identity and, worse, the
		// commit below would write it over a record that may be perfectly
		// intact and merely momentarily unreadable.
		s.log().Warn("daemon: could not read the durable identity record; continuing under a temporary identity",
			"err", err)
		s.setRecovery(recoveryDegraded)
		return
	}
	if !ok {
		s.persistIdentity()
		s.setRecovery(recoveryEstablished)
		return
	}
	s.mutate(func(v *sessionView) { v.persistedIdentity = rec })
	adopted := s.adoptStoredID(rec)
	named := s.restoreStoredName(rec, adopted)
	switch {
	case adopted && named:
		s.setRecovery(recoveryRestored)
	default:
		s.setRecovery(recoveryDegraded)
	}
}

// adoptStoredID re-registers this connection under the internal session ID the
// durable record proves it holds, so stats, memories, collab and the mailbox see
// one continuous identity across the restart.
//
// The record's ExternalID is passed to the adoption as a FALLBACK linkage. Adopt
// prefers the predecessor's session JSON, but that file is garbage collected 24 h
// after the session ends — so before this the linkage silently vanished on any
// outage longer than the grace window, even though the identity survived it.
//
// A refusal is never converged. The previous implementation, on both the
// ID-taken and the error path, re-recorded the CURRENT (temporary) identity so
// "the next reconnect adopts correctly" — but the record it overwrote was the
// only proof of what to adopt, so the next reconnect adopted the temporary one
// instead. Leaving the record alone is what makes the retry actually retry.
func (s *connSession) adoptStoredID(rec sessionstate.Identity) bool {
	oldID := s.sessionID()
	if oldID == "" || rec.SessionID == "" {
		// An unregistered session has no identity to move, and a pre-v4 record
		// proves no predecessor. Neither is a failure worth reporting as one.
		return rec.SessionID == "" && oldID != ""
	}
	if oldID == rec.SessionID {
		return true
	}
	reg, err := session.AdoptWithExternalID(oldID, rec.SessionID, rec.ExternalID)
	if errors.Is(err, session.ErrIDTaken) {
		// Either the predecessor connSession has not finished detaching, or a
		// genuinely different live owner holds this ID. Both are conflicts, not
		// licence to mint a successor: keep the temporary ID, keep the record.
		s.log().Info("daemon: the proven session ID is held by a live session; continuing under a temporary identity",
			"proven", rec.SessionID, "using", oldID)
		return false
	}
	if err != nil {
		s.log().Warn("daemon: adopting the proven session ID failed; continuing under a temporary identity",
			"proven", rec.SessionID, "using", oldID, "err", err)
		return false
	}
	s.setSessionID(reg.ID)
	// The adopted ID is the predecessor's own, so the mailbox-inheritance
	// identity — which exists to read a predecessor's mail under its old ID — is
	// redundant: the session reads its own mail under its own ID. Clear it so the
	// session's identity is one ID everywhere (PLAN-286 step 4).
	s.mutate(func(v *sessionView) {
		if len(v.inheritedSessionIDs) == 1 && v.inheritedSessionIDs[0] == reg.ID {
			v.inheritedSessionIDs = nil
		}
	})
	if s.registry != nil {
		s.registry.rekey(oldID, reg.ID)
	}
	s.log().Info("daemon: restored the proven session ID", "id", reg.ID, "previous", oldID)
	return true
}

// restoreStoredName applies the name the durable record proves this session
// answers to. A note is addressed by name, so a reconnect that came back renamed
// would orphan every message written to it.
//
// session.Rename is the authoritative uniqueness and validation check — it runs
// inside the flock that performs the write — so this asks it rather than
// pre-checking, which would cost a second full scan of the session directory to
// reach a less reliable answer. The reservations are passed in so the check also
// sees names held by recoverable-but-absent identities; a session's own
// reservation never blocks it, which is exactly the case here.
//
// The three failure modes are distinguished because they call for different
// things:
//
//   - Taken: a live overlap. Keep the generated name, keep the record, retry on
//     the next reconnect when the predecessor has gone.
//   - Invalid: this daemon rejects the stored name outright (it predates a
//     validation rule — a session named "next" before that became the reserved
//     mailbox address). Left alone the row would fail identically on EVERY
//     reconnect and the session would come back randomly renamed each time —
//     the churn this persistence exists to prevent. This is the ONE case where
//     converging the record is the repair rather than the bug, because the
//     stored value is unusable by construction rather than by circumstance.
//   - Anything else: preserve and report.
//
// When the name is restored but the ID was not, the session inherits the
// predecessor's mailbox identity so mail still reaches it. That grant is gated
// on holding the name precisely because it is the degraded path: a session may
// read a predecessor's mail only while it is actually answering to the address
// that mail was sent to.
func (s *connSession) restoreStoredName(rec sessionstate.Identity, adopted bool) bool {
	if rec.Name == "" {
		return false
	}
	if rec.Name == s.sessionName() {
		return true
	}
	_, err := s.renameSessionClaiming(rec.Name, rec.SessionID)
	if err == nil {
		if !adopted {
			s.inheritSessionID(rec.SessionID)
		}
		return true
	}
	if errors.Is(err, session.ErrNameTaken) {
		s.log().Info("daemon: the proven session name is held by a live session; keeping the generated name",
			"proven", rec.Name, "using", s.sessionName())
		return false
	}
	if errors.Is(err, session.ErrInvalidName) {
		s.log().Warn("daemon: the proven session name is no longer valid; replacing it",
			"proven", rec.Name, "using", s.sessionName(), "err", err)
		s.persistIdentity()
		return false
	}
	s.log().Warn("daemon: restoring the proven session name failed; keeping the generated name",
		"proven", rec.Name, "using", s.sessionName(), "err", err)
	return false
}

// reservedNames returns the names held by recoverable-but-absent identities, so
// a name draw or a rename does not hand out a name a disconnected session is
// coming back to. Nil on any failure: a reservation lookup that cannot answer
// must not block a rename, because the live-session check behind it is still
// the guard that protects delivery today.
func (s *connSession) reservedNames() session.Reserved {
	return reservedNamesFrom(s.sessionState)
}

// reservedNamesClaiming is reservedNames with ownerID's own reservations
// removed — the set to check a RESTORE against.
//
// A reservation exists to hold a name for the identity that owns it, against
// everybody else. The session restoring that identity is not everybody else: it
// proved its claim with the proxy secret, and the record it is restoring is the
// very record the reservation was written from. Checking it against its own
// reservation would mean an identity could never take back the name being held
// for it — the guard defeating the case it exists to serve.
//
// Passing the owner explicitly, rather than relying on the session's live ID,
// is what makes this work on the degraded path: the ID adoption may have been
// refused, so this connection is not yet running under ownerID and would
// otherwise read as a stranger to its own reservation.
//
// Every other guard still applies. The live-session uniqueness check inside
// session.Rename is untouched, so a name a live peer actually answers to is
// still refused; only the durable reservation belonging to this same identity
// is set aside.
func (s *connSession) reservedNamesClaiming(ownerID string) session.Reserved {
	reserved := s.reservedNames()
	if ownerID == "" || len(reserved) == 0 {
		return reserved
	}
	out := make(session.Reserved, len(reserved))
	for name, owner := range reserved {
		if owner == ownerID {
			continue
		}
		out[name] = owner
	}
	return out
}

// reservedNamesFrom is reservedNames for callers that have the store but not
// yet a connSession — newConnSession draws a name before the session it would
// belong to exists. Nil-safe in both the store and the failure case.
func reservedNamesFrom(store *sessionstate.Store) session.Reserved {
	if store == nil {
		return nil
	}
	reserved, err := store.ReservedNames()
	if err != nil {
		slog.Debug("daemon: could not read name reservations", "err", err)
		return nil
	}
	return reserved
}

// setRecovery records this connection's identity-recovery outcome, for the
// initialize `_meta` snapshot and the reconnect note built from it.
func (s *connSession) setRecovery(outcome recoveryOutcome) {
	s.mutate(func(v *sessionView) { v.recovery = outcome })
}

// recovery returns the identity-recovery outcome, defaulting to unavailable for
// a connection that never reached restoreIdentity (an ordinary MCP client, which
// sends no proxy credential).
func (s *connSession) recovery() recoveryOutcome {
	if o := s.view().recovery; o != "" {
		return o
	}
	return recoveryUnavailable
}

// identityMeta builds the initialize-result `_meta` stating who this connection
// is and whether the identity was recovered — the fact the proxy needs before
// any tool runs, and the one that lets a reconnect note report rather than
// assume.
//
// It is deliberately not gated on persistence being enabled: a client with no
// durable continuity still benefits from being told its own name and ID (that is
// PLAN-425 item 1, arriving one exchange earlier), and the outcome field says
// plainly that continuity is unavailable. The proxy-session secret never
// appears here — this travels to the client.
func (s *connSession) identityMeta() map[string]any {
	id := s.sessionID()
	if id == "" {
		// An unregistered session's name was drawn without a uniqueness check
		// and is not an address. Reporting it as identity would invite the
		// client to use it as one.
		return map[string]any{identityMetaRecovery: string(s.recovery())}
	}
	return map[string]any{
		identityMetaSessionID: id,
		identityMetaName:      s.sessionName(),
		identityMetaRevision:  s.view().persistedIdentity.NameRevision,
		identityMetaRecovery:  string(s.recovery()),
	}
}

// Field names inside the identity snapshot. They are a wire contract between the
// daemon and a `plumb serve` that may be an older or newer build, so they are
// named once here and read once in serve_proxy_identity.go.
const (
	identityMetaSessionID = "session_id"
	identityMetaName      = "name"
	identityMetaRevision  = "name_revision"
	identityMetaRecovery  = "recovery"
)

// daemonInstanceID returns an opaque marker unique to this daemon PROCESS:
// stable for its lifetime, different for every start.
//
// It is derived rather than stored because both inputs are already threaded to
// every connection and neither can repeat within a process: the PID identifies
// the process on this machine, and the start time separates a reused PID from
// the earlier process that held it. That avoids a package global for a value
// whose only job is to be compared for equality across a reconnect.
func daemonInstanceID(startedAt time.Time) string {
	if startedAt.IsZero() {
		return ""
	}
	return fmt.Sprintf("%d-%d", os.Getpid(), startedAt.UnixNano())
}
