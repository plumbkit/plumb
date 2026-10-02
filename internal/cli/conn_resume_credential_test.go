package cli

// conn_resume_credential_test.go — when the daemon mints and discloses a resume
// credential, and when it must not (design §3 "Establishment and disclosure", §7
// rows 4, 5 and 7).
//
// The rule every test here serves: only a PROVEN branch (established or restored
// under a proxy credential, with persistence on) mints. A degraded outcome never
// does — issuing one to a connection that is not provably the recorded identity
// would hand the identity-fork bug a credential of its own.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/sessionstate"
	"github.com/plumbkit/plumb/internal/sqlitex"
)

// credentialRows counts the rows of the credential table through a second handle on
// the database, so a test can see what the store holds without going through the
// code under test.
func credentialRows(t *testing.T) int {
	t.Helper()
	db, err := sqlitex.Open(sessionstate.DBPath(), sqlitex.Options{MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("opening the state database: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM resume_credential`).Scan(&n); err != nil {
		t.Fatalf("counting credentials: %v", err)
	}
	return n
}

// First contact under a proxy credential discloses once, in the initialize result;
// the daemon keeps only the hash; a restore under the same proxy credential mints the
// next generation and the previous one is superseded.
func TestResumeCredential_DisclosedOnFirstContactAndOnRestore(t *testing.T) {
	w := newIdentityWorld(t).withState()
	ws := identityRepo(t)

	a := w.conn("")
	meta := a.initialize("P1")
	c1 := disclosed(meta)
	if !resumeSecretShape.MatchString(c1) {
		t.Fatalf("first contact disclosed %q, want rsk1- and 22 base64url characters", c1)
	}
	if again := disclosed(a.srv.InitializeMeta(t.Context())); again != "" {
		t.Errorf("the credential was disclosed a second time: %q (it is disclosed once)", again)
	}
	a.start("conv-1", ws, "conv-1", nil) // links the conversation, which is how a credential is found
	if lk, err := w.ss.LookupResumeCredential("conv-1", sessionstate.HashResumeSecret(c1)); err != nil || lk.State != sessionstate.CredentialCurrent {
		t.Fatalf("the daemon does not hold the disclosed credential as current: %+v, %v", lk, err)
	}
	if got := a.s.recovery(); got != recoveryEstablished {
		t.Fatalf("precondition: first contact is %q, want established", got)
	}
	a.s.close()

	b := w.conn("")
	c2 := disclosed(b.initialize("P1"))
	if got := b.s.recovery(); got != recoveryRestored {
		t.Fatalf("precondition: the reconnect is %q, want restored", got)
	}
	if !resumeSecretShape.MatchString(c2) || c2 == c1 {
		t.Fatalf("a restore disclosed %q, want a NEW rsk1- credential (previous %q)", c2, c1)
	}
	if lk, _ := w.ss.LookupResumeCredential("conv-1", sessionstate.HashResumeSecret(c2)); lk.State != sessionstate.CredentialCurrent {
		t.Errorf("the restored connection's credential is %q, want current", lk.State)
	}
	if lk, _ := w.ss.LookupResumeCredential("conv-1", sessionstate.HashResumeSecret(c1)); lk.State != sessionstate.CredentialSuperseded {
		t.Errorf("the previous generation is %q, want superseded", lk.State)
	}
}

// A degraded recovery never learns the secret, never writes a credential row, and
// does not disturb the live one: the fork bug, in its new costume.
func TestResumeCredential_DegradedOutcomesNeverLearnTheSecret(t *testing.T) {
	t.Run("a predecessor that has not detached", func(t *testing.T) {
		w := newIdentityWorld(t).withState()
		first := w.conn("")
		c1 := disclosed(first.initialize("P1")) // stays live, so the reconnect below overlaps it
		rows := credentialRows(t)

		overlapping := w.conn("")
		meta := overlapping.initialize("P1")
		if got := overlapping.s.recovery(); got != recoveryDegraded {
			t.Fatalf("precondition: the overlap is %q, want degraded", got)
		}
		if got := disclosed(meta); got != "" {
			t.Fatalf("a degraded connection was disclosed a credential: %q", got)
		}
		if _, ok := meta[mcp.MetaResumeCredentialKey]; ok {
			t.Error("the resume-credential key is present on a degraded initialize result")
		}
		if got := credentialRows(t); got != rows {
			t.Errorf("a degraded connection changed the credential table: %d rows, want %d", got, rows)
		}
		if got := overlapping.callMeta("", "daemon_info", nil, nil).credential(); got != "" {
			t.Errorf("a later tool result disclosed a credential to a degraded connection: %q", got)
		}
		if c1 == "" {
			t.Fatal("precondition: the live predecessor was never given a credential, so there is nothing to disturb")
		}
	})

	t.Run("an unreadable identity record", func(t *testing.T) {
		w := newIdentityWorld(t).withState()
		first := w.conn("")
		first.initialize("P1")
		first.s.close()
		rows := credentialRows(t)

		restore := hideIdentityTable(t)
		blind := w.conn("")
		meta := blind.initialize("P1")
		restore()
		if got := blind.s.recovery(); got != recoveryDegraded {
			t.Fatalf("precondition: an unreadable record is %q, want degraded", got)
		}
		if got := disclosed(meta); got != "" {
			t.Fatalf("a connection that could not read its record was disclosed a credential: %q", got)
		}
		if got := credentialRows(t); got != rows {
			t.Errorf("an unreadable record changed the credential table: %d rows, want %d", got, rows)
		}
	})
}

// An ordinary MCP client (no proxy credential) and a session with persistence off
// receive nothing, exactly as before.
func TestResumeCredential_NotIssuedToClientsWithoutContinuity(t *testing.T) {
	t.Run("no proxy credential", func(t *testing.T) {
		w := newIdentityWorld(t).withState()
		c := w.conn("")
		meta := c.initialize("")
		if _, ok := meta[mcp.MetaResumeCredentialKey]; ok {
			t.Fatalf("an ordinary client's initialize result carries the key: %v", meta)
		}
		if got := credentialRows(t); got != 0 {
			t.Fatalf("an ordinary client left %d credential rows", got)
		}
		if got := c.callMeta("", "daemon_info", nil, nil).credential(); got != "" {
			t.Errorf("an ordinary client's tool result disclosed %q", got)
		}
	})

	t.Run("persist_state off, even for a proxy whose record exists", func(t *testing.T) {
		w := newIdentityWorld(t).withState()
		a := w.conn("")
		a.initialize("P1") // persistence on: a record and a credential exist for P1
		a.s.close()
		rows := credentialRows(t)

		w.cfg.Session.PersistState = false
		b := w.conn("")
		meta := b.initialize("P1")
		if got := b.s.recovery(); got != recoveryUnavailable {
			t.Fatalf("precondition: persistence off is %q, want unavailable", got)
		}
		if got := disclosed(meta); got != "" {
			t.Fatalf("a connection with persistence off was disclosed %q", got)
		}
		if got := credentialRows(t); got != rows {
			t.Errorf("a connection with persistence off wrote the credential table: %d rows, want %d", got, rows)
		}
	})
}

// C3: a connection that degraded under a restart storm and converged on the bounded
// retry is a proven restore, so it is issued a credential on the same terms. There is
// no initialize result left to carry it, so the next successful tool result does, once.
func TestResumeCredential_RetryConvergedConnectionIsDisclosedOnce(t *testing.T) {
	w := newIdentityWorld(t).withState()
	ws := identityRepo(t)
	first := w.conn("")
	first.initialize("P1")
	first.start("conv-1", ws, "conv-1", nil) // a linked record, so a credential can be found
	oldRows := credentialRows(t)

	overlapping := w.conn("")
	overlapping.s.restoreRetryBackoff = func(int) time.Duration { return time.Millisecond }
	if got := disclosed(overlapping.initialize("P1")); got != "" {
		t.Fatalf("the degraded initialize disclosed %q", got)
	}
	if overlapping.s.recovery() != recoveryDegraded {
		t.Fatal("precondition: the overlap did not degrade")
	}
	first.s.close() // the blocker detaches; the retry converges the open connection

	deadline := time.Now().Add(3 * time.Second)
	for overlapping.s.recovery() != recoveryRestored {
		if time.Now().After(deadline) {
			t.Fatal("the degraded connection never converged on the retry")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := credentialRows(t); got <= oldRows {
		t.Fatalf("convergence minted no credential (rows %d, was %d)", got, oldRows)
	}
	cred := overlapping.callMeta("", "daemon_info", nil, nil).credential()
	if !resumeSecretShape.MatchString(cred) {
		t.Fatalf("the next tool result after convergence disclosed %q, want an rsk1- credential", cred)
	}
	if lk, _ := w.ss.LookupResumeCredential("conv-1", sessionstate.HashResumeSecret(cred)); lk.State != sessionstate.CredentialCurrent {
		t.Errorf("the credential disclosed after convergence is %q at the daemon, want current", lk.State)
	}
	if again := overlapping.callMeta("", "daemon_info", nil, nil).credential(); again != "" {
		t.Errorf("the late credential was disclosed twice: %q", again)
	}
}

// The credential rides `_meta` and nowhere a model or an operator reads: not a
// packet, a tool result's text, a log line, and not the database in plaintext. The
// scan has a positive control, so its silence means something.
func TestResumeCredential_SecretNeverAppearsInTextLogsOrStorage(t *testing.T) {
	if !leakScanWorks() {
		t.Fatal("the leak scan cannot see a credential-shaped token, so its silence would prove nothing")
	}
	logs := captureLogs(t)
	fx := newCredFixture(t)

	texts := make([]string, 0, 6)
	secrets := append(make([]string, 0, 4), fx.cred)
	second := fx.replacement("P2")
	secrets = append(secrets, disclosed(second.srv.InitializeMeta(t.Context())))
	r := second.startPresenting(credConv, fx.ws, credConv, fx.cred)
	succ := r.credential()
	if !resumeSecretShape.MatchString(succ) {
		t.Fatalf("precondition: the accepted resume disclosed %q in its result _meta", succ)
	}
	secrets = append(secrets, succ)
	texts = append(texts, r.Text)
	for _, tool := range []string{"check_messages", "daemon_info", "session_start"} {
		texts = append(texts, second.callMeta(credConv, tool, nil, nil).Text)
	}
	// A stale presentation from a third process, so the refusal paths are covered.
	second.s.close()
	third := fx.replacement("P3")
	texts = append(texts, third.startPresenting(credConv, fx.ws, credConv, fx.cred).Text)

	assertNoSecret(t, "tool result text", strings.Join(texts, "\n"), secrets...)
	assertNoSecret(t, "daemon log", logs.String(), secrets...)

	db, err := sqlitex.Open(sessionstate.DBPath(), sqlitex.Options{MaxOpenConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT hash FROM resume_credential`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var dump strings.Builder
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			t.Fatal(err)
		}
		dump.WriteString(h + "\n")
	}
	assertNoSecret(t, "the stored credential column", dump.String(), secrets...)
}

// The daemon discloses into initialize `_meta` as a sibling key. An older proxy reads
// only the keys it knows, so a result carrying the new one parses exactly as before
// (design §6, mixed versions).
func TestResumeCredential_OlderProxyReadersIgnoreTheKey(t *testing.T) {
	w := newIdentityWorld(t).withState()
	c := w.conn("")
	c.s.daemonStartedAt = time.Now()
	meta := c.initialize("P1")
	if disclosed(meta) == "" {
		t.Fatal("precondition: nothing was disclosed")
	}
	frame, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"_meta": meta}})
	if err != nil {
		t.Fatal(err)
	}
	id, ok := sessionIdentityMeta(frame)
	if !ok || id.sessionID != c.s.sessionID() || id.recovery != string(recoveryEstablished) {
		t.Errorf("the identity reader no longer parses a result carrying the credential: %+v, %v", id, ok)
	}
	if got := daemonInstanceMeta(frame); got == "" || got != daemonInstanceID(c.s.daemonStartedAt) {
		t.Errorf("the daemon-instance reader returned %q", got)
	}
}
