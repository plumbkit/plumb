package cli

// conn_resume_accept_test.go — presenting a resume credential (design §3
// "Acceptance", §5 lifecycle, §7 rows 1, 2, 3, 5, 6 and 8).
//
// A presentation is the ONLY new authority, and it is equivalent to the proxy
// credential: a hash match escalates a name resume into a full restore of the
// internal session ID, the mail bound to it and the thread seats. Nothing a client
// merely CLAIMS (a conversation id, a stamp, a name) is an authority, so the negative
// tests here are the load-bearing ones.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/sessionstate"
	"github.com/plumbkit/plumb/internal/sqlitex"
)

// credState is what the daemon holds for a secret under the conversation.
func (f *credFixture) credState(secret string) sessionstate.CredentialState {
	f.w.t.Helper()
	lk, err := f.w.ss.LookupResumeCredential(credConv, sessionstate.HashResumeSecret(secret))
	if err != nil {
		f.w.t.Fatalf("lookup: %v", err)
	}
	return lk.State
}

// Row 1: the replacement presents the stored credential at session_start and gets
// the SAME internal session ID, name, mailbox binding and thread seat back, is told
// so, and is handed the successor in the same response.
func TestResume_FullRestoreAcrossAServeReplacement(t *testing.T) {
	for _, agent := range []string{credConv, ""} {
		name := "stamped as the conversation"
		if agent == "" {
			name = "an unstamped call"
		}
		t.Run(name, func(t *testing.T) {
			fx := newCredFixture(t)
			second := fx.replacement("P2")
			if second.s.sessionID() == fx.id {
				t.Fatal("precondition: the replacement already holds the old ID")
			}
			r := second.startPresenting(agent, fx.ws, credConv, fx.cred)

			if got := second.s.sessionID(); got != fx.id {
				t.Errorf("session ID = %q, want the predecessor's %q", got, fx.id)
			}
			if got := second.s.sessionName(); got != fx.name {
				t.Errorf("name = %q, want %q", got, fx.name)
			}
			if got := second.s.recovery(); got != recoveryRestored {
				t.Errorf("recovery = %q, want restored", got)
			}
			if line := sessionLine(r.Text); !strings.Contains(line, "identity restored") {
				t.Errorf("the packet does not report the full restore: %q", line)
			}
			succ := r.credential()
			if !resumeSecretShape.MatchString(succ) || succ == fx.cred {
				t.Fatalf("the response disclosed %q, want a NEW rsk1- successor", succ)
			}
			if got := fx.credState(fx.cred); got != sessionstate.CredentialSuperseded {
				t.Errorf("the presented generation is %q, want superseded (rotated)", got)
			}
			if got := fx.credState(succ); got != sessionstate.CredentialCurrent {
				t.Errorf("the successor is %q at the daemon, want current", got)
			}
			rec, ok, err := fx.w.ss.LoadIdentity("P2")
			if err != nil || !ok || rec.SessionID != fx.id || rec.Name != fx.name || rec.ExternalID != credConv {
				t.Errorf("the identity is not re-recorded under the replacement's proxy credential: %+v (%v, %v)", rec, ok, err)
			}
			// session_start's own packet delivers what is unread, so look in both.
			if out, _ := second.call(agent, "check_messages", nil); !strings.Contains(r.Text+out, credBoundNote) {
				t.Errorf("mail bound to the predecessor's ID did not come back:\n%s\n%s", r.Text, out)
			}
			reply, isErr := second.call(agent, "leave_note", map[string]any{"conversation_id": fx.threadID, "body": "back"})
			if isErr || strings.Contains(reply, "not one of yours") {
				t.Errorf("the thread seat did not come back: %q", reply)
			}
		})
	}
}

// The control that makes row 1 mean something: with no credential presented the
// replacement is exactly what it is today, a name resume that forks the identity.
func TestResume_WithoutACredentialStaysNameOnly(t *testing.T) {
	fx := newCredFixture(t)
	second := fx.replacement("P2")
	r := second.startPresenting("", fx.ws, credConv, "")

	if second.s.sessionID() == fx.id {
		t.Fatal("a conversation id alone restored the predecessor's session ID (decision D1)")
	}
	if got := second.s.sessionName(); got != fx.name {
		t.Errorf("the name-only resume no longer returns the name: %q, want %q", got, fx.name)
	}
	if out, _ := second.call("", "check_messages", nil); strings.Contains(r.Text+out, credBoundNote) {
		t.Errorf("a name resume read mail bound to the predecessor:\n%s\n%s", r.Text, out)
	}
	if got := r.credential(); got != "" {
		t.Errorf("a call that presented nothing was disclosed %q", got)
	}
	if got := fx.credState(fx.cred); got != sessionstate.CredentialCurrent {
		t.Errorf("a call that presented nothing moved the credential to %q", got)
	}
}

// A presentation proves nothing where it is not the proxy-credentialed owner of a
// FRESH identity presenting for the conversation it named; the call then behaves
// exactly as it does today and the credential is untouched.
func TestResume_PresentationIsInertWhereItProvesNothing(t *testing.T) {
	cases := []struct {
		name           string
		restoresItself bool
		run            func(fx *credFixture) (*identityConn, frameResult)
	}{
		{"an ordinary client with no proxy credential", false, func(fx *credFixture) (*identityConn, frameResult) {
			c := fx.w.conn("")
			return c, c.startPresenting("", fx.ws, credConv, fx.cred)
		}},
		{"a connection with persistence off", false, func(fx *credFixture) (*identityConn, frameResult) {
			fx.w.cfg.Session.PersistState = false
			c := fx.w.conn("")
			c.initialize("P9")
			return c, c.startPresenting("", fx.ws, credConv, fx.cred)
		}},
		{"a subagent of the conversation", false, func(fx *credFixture) (*identityConn, frameResult) {
			c := fx.replacement("P2")
			sub := credConv + "/agent-1"
			return c, c.startPresenting(sub, fx.ws, sub, fx.cred)
		}},
		{"another conversation presenting this one's credential", false, func(fx *credFixture) (*identityConn, frameResult) {
			c := fx.replacement("P2")
			return c, c.startPresenting("conv-other", fx.ws, "conv-other", fx.cred)
		}},
		{"a connection that already restored its own identity", true, func(fx *credFixture) (*identityConn, frameResult) {
			c := fx.w.conn("")
			c.initialize(fx.proxy) // the same serve process: the proxy credential restores it
			return c, c.startPresenting("", fx.ws, credConv, fx.cred)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := newCredFixture(t)
			conn, r := c.run(fx)
			if got := r.credential(); got != "" {
				t.Errorf("an inert presentation was disclosed a successor: %q", got)
			}
			if conn.s.recovery() == recoveryRestored && conn.s.sessionID() != fx.id {
				t.Errorf("recovery reports restored under a different ID")
			}
			if conn.s.recovery() != recoveryRestored && conn.s.sessionID() == fx.id {
				t.Errorf("%s adopted the predecessor's session ID", c.name)
			}
			// A connection restored under the SAME proxy credential was legitimately issued
			// the next generation at initialize, which supersedes the old one by design;
			// what a presentation there must not do is rotate (it was disclosed nothing).
			if got := fx.credState(fx.cred); got == sessionstate.CredentialRevoked || (got == sessionstate.CredentialSuperseded && !c.restoresItself) {
				t.Errorf("%s consumed the credential: %q", c.name, got)
			}
		})
	}
}

// Row 2: after one accepted resume moves the generation, the superseded one is
// refused, the fallback is today's name-only resume, no ID is adopted, and the
// refusal is loud at Warn without ever printing the secret.
func TestResume_ReplayAfterRotationIsRefusedAndLoud(t *testing.T) {
	logs := captureLogs(t)
	fx := newCredFixture(t)
	second := fx.replacement("P2")
	succ := second.startPresenting("", fx.ws, credConv, fx.cred).credential()
	second.s.close()

	third := fx.replacement("P3")
	r := third.startPresenting("", fx.ws, credConv, fx.cred) // the superseded generation
	if got := third.s.sessionID(); got == fx.id {
		t.Fatal("a superseded credential adopted the predecessor's session ID")
	}
	if got := third.s.sessionName(); got != fx.name {
		t.Errorf("the fallback is not the name-only resume: name %q, want %q", got, fx.name)
	}
	if got := third.s.recovery(); got != recoveryEstablished {
		t.Errorf("recovery = %q, want the name-only outcome (established)", got)
	}
	if !strings.Contains(r.Text, "superseded") {
		t.Errorf("the claimant is not told its credential was superseded:\n%s", r.Text)
	}
	if got := r.credential(); got != "" {
		t.Errorf("a refused replay was disclosed %q", got)
	}
	var warned bool
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, "superseded") && strings.Contains(line, credConv) {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no Warn line names the replay and the conversation:\n%s", logs.String())
	}
	assertNoSecret(t, "daemon log", logs.String(), fx.cred, succ)
	if rec, _, _ := fx.w.ss.LoadIdentity("P2"); rec.SessionID != fx.id {
		t.Errorf("the replay disturbed the durable record: %+v", rec)
	}

	// Replays are answered, not punished: they never count toward revocation.
	for range 5 {
		third.startPresenting("", fx.ws, credConv, fx.cred)
	}
	if got := fx.credState(succ); got != sessionstate.CredentialCurrent {
		t.Errorf("replays revoked the live successor: %q", got)
	}
}

// Row 3: two claimants of one current secret, in an interleaved order the test
// forces (the second has looked the credential up, then the first completes). Exactly
// one adoption lands; the loser is told it was superseded, keeps running under a
// temporary identity, and persisted nothing.
func TestResume_TwoClaimantsOfOneSecretAreFenced(t *testing.T) {
	fx := newCredFixture(t)
	a, b := fx.replacement("PA"), fx.replacement("PB")
	var winner frameResult
	b.s.resumeSeams.afterLookup = func() {
		b.s.resumeSeams.afterLookup = nil
		winner = a.startPresenting("", fx.ws, credConv, fx.cred)
	}
	loser := b.startPresenting("", fx.ws, credConv, fx.cred)

	if got := a.s.sessionID(); got != fx.id {
		t.Fatalf("the first claimant holds %q, want the predecessor's %q", got, fx.id)
	}
	if got := b.s.sessionID(); got == fx.id {
		t.Fatal("both claimants adopted the predecessor's session ID")
	}
	if !resumeSecretShape.MatchString(winner.credential()) {
		t.Errorf("the winner was not handed a successor: %q", winner.credential())
	}
	if got := loser.credential(); got != "" {
		t.Errorf("the loser was handed a successor: %q", got)
	}
	if !strings.Contains(loser.Text, "superseded") {
		t.Errorf("the loser is not told it was superseded:\n%s", loser.Text)
	}
	if got := b.s.recovery(); got != recoverySuperseded {
		t.Errorf("the loser's recovery = %q, want superseded", got)
	}
	if rec, _, _ := fx.w.ss.LoadIdentity("PB"); rec.ExternalID != "" || rec.SessionID != b.s.sessionID() {
		t.Errorf("the loser persisted beyond its own temporary identity: %+v", rec)
	}
	if b.s.persistIdentity() {
		t.Error("a superseded connection can still write the durable record (it runs under the degraded rules)")
	}
	if rec, _, _ := fx.w.ss.LoadIdentity("PA"); rec.SessionID != fx.id || rec.ExternalID != credConv {
		t.Errorf("the winner's record is not the predecessor's identity: %+v", rec)
	}
	// Told, not killed: the loser's connection keeps serving.
	if r := b.callMeta("", "daemon_info", nil, nil); r.IsError {
		t.Errorf("the loser's connection was closed or broken: %s", r.Text)
	}
	if later := b.callMeta("", "session_start", nil, nil).Text; !strings.Contains(later, "same resume credential first") {
		t.Errorf("a later session_start does not keep telling the loser:\n%s", later)
	}
}

// The interleaving the old order lost. The winner has adopted the predecessor's ID and
// recorded it, and has not yet issued its successor, when a second claimant of the same
// secret arrives. The arbitration is the FIRST step of a resume, so by then the
// generation has already moved: the second claimant is told it was superseded and
// adopts and restores nothing. It cannot tell this from a replay moments later, and is
// handled as one (a name-only resume under its own temporary identity). Adopt, write,
// then arbitrate let it see "current", be refused the ID, and be told nothing.
func TestResume_ALoserBetweenTheWinnersRestoreAndRotationIsToldSuperseded(t *testing.T) {
	fx := newCredFixture(t)
	a, b := fx.replacement("PA"), fx.replacement("PB")
	var loser frameResult
	var seamRan bool
	a.s.resumeSeams.afterRestore = func() {
		a.s.resumeSeams.afterRestore, seamRan = nil, true
		if got := a.s.sessionID(); got != fx.id {
			t.Errorf("seam: the winner holds %q, want the predecessor's %q; the interleaving is not the one under test", got, fx.id)
		}
		loser = b.startPresenting("", fx.ws, credConv, fx.cred)
	}
	winner := a.startPresenting("", fx.ws, credConv, fx.cred)

	if !seamRan {
		t.Fatal("the seam never ran, so the interleaving was not exercised")
	}
	if !strings.Contains(loser.Text, "superseded") {
		t.Errorf("the loser is not told it was superseded:\n%s", loser.Text)
	}
	if got := loser.credential(); got != "" {
		t.Errorf("the loser was handed a successor: %q", got)
	}
	if got := b.s.recovery(); got == recoveryRestored {
		t.Error("the loser reports a restored identity")
	}
	if got := b.s.sessionID(); got == fx.id {
		t.Error("the loser adopted the predecessor's session ID")
	}
	if rec, _, _ := fx.w.ss.LoadIdentity("PB"); rec.SessionID != b.s.sessionID() {
		t.Errorf("the loser's durable record is not its own temporary identity: %+v", rec)
	}
	if !resumeSecretShape.MatchString(winner.credential()) {
		t.Errorf("the winner was not handed a successor: %q", winner.credential())
	}
	if got := a.s.sessionID(); got != fx.id {
		t.Errorf("the winner holds %q, want the predecessor's %q", got, fx.id)
	}
}

// A claimant that wins the arbitration and then cannot finish gives the generation
// back, so the legitimate claimant's credential is valid for the retry. Two ways it
// can fail after the claim: the name is held elsewhere, and the successor cannot be
// recorded.
func TestResume_AWinnerThatCannotFinishGivesTheGenerationBack(t *testing.T) {
	t.Run("the name is held by a live session", func(t *testing.T) {
		fx := newCredFixture(t)
		holder, err := session.Register(session.Info{ID: "name-holder", Name: fx.name})
		if err != nil {
			t.Fatalf("occupying the proven name: %v", err)
		}
		t.Cleanup(func() { session.Unregister(holder.ID) })

		second := fx.replacement("P2")
		r := second.startPresenting("", fx.ws, credConv, fx.cred)
		if got := second.s.recovery(); got != recoveryDegraded {
			t.Fatalf("recovery = %q, want degraded: the name could not be applied", got)
		}
		if got := r.credential(); got != "" {
			t.Errorf("a restore that could not complete disclosed a successor: %q", got)
		}
		if got := fx.credState(fx.cred); got != sessionstate.CredentialCurrent {
			t.Errorf("a degraded restore left the credential %q, want it given back (current)", got)
		}
	})
	t.Run("the successor cannot be recorded", func(t *testing.T) {
		fx := newCredFixture(t)
		second := fx.replacement("P2")
		second.s.resumeSeams.afterRestore = func() {
			// The claimant's identity row vanishes, so there is nothing to hang a successor off.
			db, err := sqlitex.Open(sessionstate.DBPath(), sqlitex.Options{MaxOpenConns: 1})
			if err != nil {
				t.Fatalf("opening the state database: %v", err)
			}
			defer db.Close()
			if _, err := db.Exec(`DELETE FROM session_names WHERE proxy_session_id='P2'`); err != nil {
				t.Fatalf("removing the claimant's row: %v", err)
			}
		}
		r := second.startPresenting("", fx.ws, credConv, fx.cred)
		if got := second.s.sessionID(); got != fx.id {
			t.Fatalf("the restore itself did not complete: %q, want %q", got, fx.id)
		}
		if got := r.credential(); got != "" {
			t.Errorf("a successor that could not be recorded was disclosed: %q", got)
		}
		if got := fx.credState(fx.cred); got != sessionstate.CredentialCurrent {
			t.Errorf("an unrecorded successor left the presented credential %q, want it given back (current)", got)
		}
	})
}

// The recovery gate: only a FRESH, established connection may present. A connection
// that is degraded is running under a stand-in, and one that is already restored holds
// a proven identity of its own; a presentation from either must not move it onto
// the predecessor's session ID, whatever the credential is worth.
func TestResume_OnlyAFreshEstablishedConnectionMayPresent(t *testing.T) {
	cases := []struct {
		name string
		prep func(fx *credFixture) (c *identityConn, want recoveryOutcome)
	}{
		{"a degraded connection", func(fx *credFixture) (*identityConn, recoveryOutcome) {
			holder := fx.replacement("P2") // live under P2, so the next P2 connection overlaps it
			_ = holder
			c := fx.w.conn("")
			c.initialize("P2")
			return c, recoveryDegraded
		}},
		{"a connection that already restored its own identity", func(fx *credFixture) (*identityConn, recoveryOutcome) {
			first := fx.replacement("P2")
			first.startPresenting("", fx.ws, credConv, "") // a name-only resume links P2's record to the conversation
			first.s.close()
			c := fx.w.conn("")
			c.initialize("P2")
			return c, recoveryRestored
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newCredFixture(t)
			c, want := tc.prep(fx)
			if got := c.s.recovery(); got != want {
				t.Fatalf("precondition: recovery = %q, want %q", got, want)
			}
			own := c.s.sessionID()
			r := c.startPresenting("", fx.ws, credConv, fx.cred)

			if got := c.s.sessionID(); got == fx.id {
				t.Errorf("%s was moved onto the predecessor's session ID", tc.name)
			} else if got != own {
				t.Errorf("%s changed its own session ID from %q to %q", tc.name, own, got)
			}
			if got := c.s.recovery(); got != want {
				t.Errorf("recovery moved from %q to %q", want, got)
			}
			if got := r.credential(); got != "" {
				t.Errorf("an ineligible presentation was disclosed a successor: %q", got)
			}
			if got := fx.credState(fx.cred); got != sessionstate.CredentialCurrent {
				t.Errorf("an ineligible presentation moved the credential to %q", got)
			}
		})
	}
}

// The linkage gate: a connection already linked to ANOTHER conversation may not
// present for this one. Its linkage is somebody else's, and a restore here would hand
// it the predecessor's identity while the record still answers to the other
// conversation.
func TestResume_AConnectionLinkedToAnotherConversationMayNotPresent(t *testing.T) {
	fx := newCredFixture(t)
	c := fx.replacement("P2")
	c.startPresenting("", fx.ws, "conv-other", "") // an unstamped call: the connection is now linked to conv-other
	if got := c.s.externalID(); got != "conv-other" {
		t.Fatalf("precondition: the connection is linked to %q, want conv-other", got)
	}
	own := c.s.sessionID()
	r := c.startPresenting("", fx.ws, credConv, fx.cred)

	if got := c.s.sessionID(); got == fx.id {
		t.Error("a connection linked to another conversation was restored onto the predecessor's session ID")
	} else if got != own {
		t.Errorf("the connection's own session ID changed from %q to %q", own, got)
	}
	if got := r.credential(); got != "" {
		t.Errorf("an ineligible presentation was disclosed a successor: %q", got)
	}
	if got := fx.credState(fx.cred); got != sessionstate.CredentialCurrent {
		t.Errorf("an ineligible presentation moved the credential to %q", got)
	}
}

// Row 5: a degraded acceptance consumes no generation. The ID is held by a live
// session, so the restore is refused; the legitimate claimant's credential is
// untouched and the same presentation lands once the overlap clears.
func TestResume_ADegradedAcceptanceConsumesNoGeneration(t *testing.T) {
	fx := newCredFixture(t)
	holder, err := session.Register(session.Info{ID: fx.id, Name: "id-holder"})
	if err != nil {
		t.Fatalf("occupying the proven ID: %v", err)
	}
	t.Cleanup(func() { session.Unregister(holder.ID) })

	second := fx.replacement("P2")
	rows := credentialRows(t)
	r := second.startPresenting("", fx.ws, credConv, fx.cred)
	if second.s.sessionID() == fx.id {
		t.Fatal("the ID was adopted despite a live holder; the test is not exercising the refusal")
	}
	if got := r.credential(); got != "" {
		t.Errorf("a refused acceptance disclosed a successor: %q", got)
	}
	if got := fx.credState(fx.cred); got != sessionstate.CredentialCurrent {
		t.Errorf("a refused acceptance moved the credential to %q; the generation must not be consumed", got)
	}
	if got := second.s.recovery(); got == recoverySuperseded {
		t.Error("a degraded attempt was mistaken for a superseded one")
	}
	if got := credentialRows(t); got != rows {
		t.Errorf("a refused acceptance wrote the credential table: %d rows, want %d", got, rows)
	}

	session.Unregister(holder.ID)
	r = second.startPresenting("", fx.ws, credConv, fx.cred)
	if got := second.s.sessionID(); got != fx.id {
		t.Fatalf("the retry after the overlap cleared holds %q, want the predecessor's %q", got, fx.id)
	}
	if !resumeSecretShape.MatchString(r.credential()) {
		t.Errorf("the retry was not handed a successor: %q", r.credential())
	}
}

// Row 6: a chain of replacements, each restoring the full identity and rotating the
// credential. A real ID-bound message is written to each hop while it is live and read
// by the next, and the one written to the last is read at the far end. The leak scan
// runs over the whole chain.
func TestResume_RestartChainKeepsTheFullIdentityAtEveryHop(t *testing.T) {
	logs := captureLogs(t)
	fx := newCredFixture(t)
	const hops = 4
	cred := fx.cred
	secrets := []string{cred}
	var texts []string
	awaiting := credBoundNote // written to the previous hop's session ID while it was live
	var far *identityConn
	for hop := 1; hop <= hops; hop++ {
		c := fx.replacement(fmt.Sprintf("P%d", hop+1))
		r := c.startPresenting("", fx.ws, credConv, cred)
		if got := c.s.sessionID(); got != fx.id {
			t.Fatalf("hop %d: session ID %q, want %q", hop, got, fx.id)
		}
		if got := c.s.sessionName(); got != fx.name {
			t.Fatalf("hop %d: name %q, want %q", hop, got, fx.name)
		}
		if !strings.Contains(r.Text, awaiting) {
			t.Fatalf("hop %d: the message bound to the previous hop's ID did not arrive:\n%s", hop, r.Text)
		}
		succ := r.credential()
		if !resumeSecretShape.MatchString(succ) || succ == cred {
			t.Fatalf("hop %d: successor %q (previous %q)", hop, succ, cred)
		}
		if got := fx.credState(cred); got != sessionstate.CredentialSuperseded {
			t.Fatalf("hop %d: the consumed generation is %q, want superseded", hop, got)
		}
		secrets = append(secrets, succ)
		texts = append(texts, r.Text)
		cred, far = succ, c
		awaiting = fmt.Sprintf("bound note written during hop %d", hop)
		if out, isErr := fx.peer.call("", "leave_note", map[string]any{"to": fx.name, "body": awaiting}); isErr {
			t.Fatalf("hop %d: peer leave_note: %s", hop, out)
		}
		if hop < hops {
			c.s.close() // the serve is replaced again
		}
	}
	out, _ := far.call("", "check_messages", nil)
	texts = append(texts, out)
	if !strings.Contains(out, awaiting) {
		t.Errorf("the message bound to the far end's ID is not readable there: %q", out)
	}
	reply, isErr := far.call("", "leave_note", map[string]any{"conversation_id": fx.threadID, "body": "far end"})
	if isErr || strings.Contains(reply, "not one of yours") {
		t.Errorf("the thread seat did not survive %d replacements: %q", hops, reply)
	}
	assertNoSecret(t, "chain tool output", strings.Join(texts, "\n"), secrets...)
	assertNoSecret(t, "chain daemon log", logs.String(), secrets...)
}

// Row 8: replacing the record's external linkage under a proxy credential revokes the
// credential with it, so a later replacement presenting the stored credential is
// refused with "revoked" and falls back to the name.
func TestResume_ReplacingTheLinkageRevokesTheCredential(t *testing.T) {
	w := newIdentityWorld(t).withState()
	ws := identityRepo(t)
	first := w.conn("")
	cred := disclosed(first.initialize("P1"))
	first.start("conv-A", ws, "conv-A", nil)
	id := first.s.sessionID()
	// An unstamped session_start naming another conversation is how a client with no
	// per-call identity hands its connection to the next one: the linkage is REPLACED.
	first.start("", ws, "conv-B", nil)
	first.s.close()

	second := w.conn("")
	second.initialize("P2")
	r := second.startPresenting("", ws, "conv-A", cred)
	if second.s.sessionID() == id {
		t.Fatal("a revoked credential adopted the session ID")
	}
	if !strings.Contains(r.Text, "revoked") {
		t.Errorf("the claimant is not told its credential was revoked:\n%s", r.Text)
	}
	if got := second.s.recovery(); got != recoveryEstablished {
		t.Errorf("recovery = %q, want the name-only outcome (established)", got)
	}
}

// Failed ownership: junk presented against a conversation that has a credential
// counts, the third revokes it and the log says why; a conversation with no record
// is never counted.
func TestResume_ThreeUnmatchedPresentationsRevokeTheCredential(t *testing.T) {
	const junk = "rsk1-AAAAAAAAAAAAAAAAAAAAAA"
	t.Run("a conversation with a credential", func(t *testing.T) {
		logs := captureLogs(t)
		fx := newCredFixture(t)
		attacker := fx.replacement("PX")
		for i := 1; i <= 2; i++ {
			attacker.startPresenting("", fx.ws, credConv, junk)
			if got := fx.credState(fx.cred); got != sessionstate.CredentialCurrent {
				t.Fatalf("after %d junk presentations the credential is %q, want still current", i, got)
			}
		}
		attacker.startPresenting("", fx.ws, credConv, junk)
		if got := fx.credState(fx.cred); got != sessionstate.CredentialRevoked {
			t.Fatalf("after three junk presentations the credential is %q, want revoked", got)
		}
		if !strings.Contains(logs.String(), "revoked") || !strings.Contains(logs.String(), credConv) {
			t.Errorf("the revocation is not logged with the conversation:\n%s", logs.String())
		}
		legit := fx.replacement("PL")
		r := legit.startPresenting("", fx.ws, credConv, fx.cred)
		if legit.s.sessionID() == fx.id || !strings.Contains(r.Text, "revoked") {
			t.Errorf("the revoked credential was honoured, or its holder was not told:\n%s", r.Text)
		}
	})
	t.Run("a conversation with no record", func(t *testing.T) {
		fx := newCredFixture(t)
		attacker := fx.replacement("PX")
		for range 6 {
			attacker.startPresenting("", fx.ws, "conv-nobody", junk)
		}
		if got := fx.credState(fx.cred); got != sessionstate.CredentialCurrent {
			t.Errorf("presentations for a conversation with no record revoked another's credential: %q", got)
		}
	})
}
