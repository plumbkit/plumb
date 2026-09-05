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
	"strings"
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
		if !s.persistIdentity() {
			// First contact whose commit did not land. The session works normally,
			// but there is no record for a reconnect to resolve — so it must not
			// advertise durable continuity it cannot deliver. Reporting
			// "established" here would have the note tell the agent its identity is
			// safe across a restart when nothing was written.
			s.log().Warn("daemon: could not record this session's identity; it will not be recoverable after a restart")
			s.setRecovery(recoveryUnavailable)
			return
		}
		s.setRecovery(recoveryEstablished)
		return
	}
	s.mutate(func(v *sessionView) { v.persistedIdentity = rec })
	adoption := s.adoptStoredID(rec)
	named := s.restoreStoredName(rec, adoption)
	// "Restored" means BOTH halves came back, and nothing weaker qualifies.
	//
	// The case worth spelling out is a legacy record (schema v3) that carries a
	// name but no session ID: the name is restored, and adoption reports
	// idAbsent because there was nothing recorded to resume. That is not a
	// restore. Counting it as one would have the reconnect note tell the agent
	// "you are still <name> (<id>)" while naming an ID it has never held, with
	// mail bound to the predecessor sitting unreachable behind the reassurance.
	if adoption == idResumed && named {
		s.setRecovery(recoveryRestored)
		return
	}
	s.setRecovery(recoveryDegraded)
}

// idOutcome is what became of the internal session ID during a restoration.
//
// It replaces a bool because "adopted" had to carry three meanings and could
// only express two: resumed, refused, and "there was nothing recorded to
// resume". The third was folded in with success — a legacy record with a name
// and no ID reported a full restore — so the reconnect note claimed an identity
// had been restored while the session ran under a brand-new ID.
type idOutcome int

const (
	// idRefused: a session ID was recorded and could not be resumed. The record
	// is preserved and a later reconnect will retry.
	idRefused idOutcome = iota
	// idResumed: this connection now IS the recorded identity.
	idResumed
	// idAbsent: the record carries no session ID (a pre-v4 row), so there is
	// nothing to resume. Not a failure, and not a success either.
	idAbsent
)

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
func (s *connSession) adoptStoredID(rec sessionstate.Identity) idOutcome {
	oldID := s.sessionID()
	switch {
	case rec.SessionID == "":
		// A pre-v4 record proves no predecessor. There is nothing to resume,
		// which the caller must not read as having resumed something.
		return idAbsent
	case oldID == "":
		// An unregistered session has no identity to move.
		return idRefused
	case oldID == rec.SessionID:
		return idResumed
	}
	reg, err := session.AdoptWithExternalID(oldID, rec.SessionID, rec.ExternalID)
	if errors.Is(err, session.ErrIDTaken) {
		// Either the predecessor connSession has not finished detaching, or a
		// genuinely different live owner holds this ID. Both are conflicts, not
		// licence to mint a successor: keep the temporary ID, keep the record.
		s.log().Info("daemon: the proven session ID is held by a live session; continuing under a temporary identity",
			"proven", rec.SessionID, "using", oldID)
		return idRefused
	}
	if err != nil {
		s.log().Warn("daemon: adopting the proven session ID failed; continuing under a temporary identity",
			"proven", rec.SessionID, "using", oldID, "err", err)
		return idRefused
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
	return idResumed
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
func (s *connSession) restoreStoredName(rec sessionstate.Identity, adoption idOutcome) bool {
	if rec.Name == "" {
		return false
	}
	if rec.Name == s.sessionName() {
		return true
	}
	_, err := s.renameSessionClaiming(rec.Name, nameClaim{sessionID: rec.SessionID, restoring: true})
	if err == nil {
		if adoption == idRefused {
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
		if adoption == idRefused {
			// The stored name can never be applied, but this connection failed to
			// resume the recorded identity — writing here would replace a PROVEN
			// session ID with a temporary one in order to repair a NAME. Leave the
			// record whole and let a reconnect that does resume it repair it.
			//
			// idAbsent is deliberately not included: a legacy row has no session ID
			// to protect, so there is nothing to lose and the repair is the whole
			// point — left alone such a row fails identically on EVERY reconnect and
			// the session comes back randomly renamed each time.
			s.log().Warn("daemon: the proven session name is no longer valid, but this connection could not resume the identity either; leaving the record intact for a later reconnect to repair",
				"proven", rec.Name, "using", s.sessionName(), "err", err)
			return false
		}
		s.log().Warn("daemon: the proven session name is no longer valid; replacing it",
			"proven", rec.Name, "using", s.sessionName(), "err", err)
		s.persistIdentity()
		return false
	}
	s.log().Warn("daemon: restoring the proven session name failed; keeping the generated name",
		"proven", rec.Name, "using", s.sessionName(), "err", err)
	return false
}

// reservedNamesFor builds the reservation set to check a name against, with the
// entries this caller is ENTITLED to removed.
//
// Two kinds of entitlement, and the second is not optional:
//
//   - ownerID: the internal session ID of the identity being restored. A
//     reservation exists to hold a name for the identity that owns it, against
//     everybody else — and the session restoring that identity is not everybody
//     else. It proved its claim with the proxy secret, and the record it is
//     restoring is the very record the reservation was written from. Passing
//     the owner explicitly, rather than reading the session's live ID, is what
//     makes this work on the degraded path, where the ID adoption was refused
//     and the connection would otherwise read as a stranger to its own
//     reservation.
//   - ownerExternalID: the caller's own external conversation ID. A `plumb
//     serve` that RESTARTS gets a fresh proxy secret and a fresh internal
//     session ID, so it can present neither — but if it re-links the same
//     conversation it is the same agent, and the name is being held for exactly
//     it. Without this, a reservation would lock a name away from its only
//     rightful claimant, and the resume-by-external-ID guarantee (PR #189)
//     would degrade into "you come back randomly renamed": the same churn this
//     persistence exists to prevent, relocated to a new trigger.
//
// An empty ownerExternalID matches nothing, so a caller that names no
// conversation is entitled to no reservation — the exclusion can only ever widen
// entitlement to someone naming the SAME conversation, never to someone naming
// none. Both empty is an ordinary rename, checked against every reservation.
//
// Every other guard still applies. The live-session uniqueness check inside
// session.Rename is untouched, so a name a live peer actually answers to is
// refused regardless of what is excluded here.
func (s *connSession) reservedNamesFor(ownerID, ownerExternalID string) session.Reserved {
	v := s.view()
	if !s.namePersistEnabled(v) {
		// The feature is off (or this is not a serve proxy). Enforcing
		// reservations here would be the worst of both worlds: nothing writes
		// records, so nothing can ever reclaim a name, and every historically
		// recorded name stays locked out forever. An opt-out has to remove the
		// cost as well as the benefit.
		return nil
	}
	return reservationsExcept(s.sessionState, ownerID, ownerExternalID)
}

// reservationsExcept reads the durable reservations and drops the ones the given
// owner is entitled to. Nil-safe in both the store and the failure case: a
// lookup that cannot answer must not block a rename, because the live-session
// check behind it is still the guard that protects delivery today.
//
// It is a full read of the identity table, taken BEFORE the session-directory
// flock the caller then enters — deliberately, so no storage query runs under
// that lock. The table holds one row per proxy session ever seen, so the scan is
// small in practice and bounded by the retention documented in Store.Prune.
func reservationsExcept(store *sessionstate.Store, ownerID, ownerExternalID string) session.Reserved {
	if store == nil {
		return nil
	}
	records, err := store.Reservations()
	if err != nil {
		slog.Debug("daemon: could not read name reservations", "err", err)
		return nil
	}
	out := make(session.Reserved, len(records))
	for _, r := range records {
		if r.SessionID == ownerID {
			continue
		}
		if ownerExternalID != "" && r.ExternalID == ownerExternalID {
			continue
		}
		out[strings.ToLower(r.Name)] = r.SessionID
	}
	return out
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
