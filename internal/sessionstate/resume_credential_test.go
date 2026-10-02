package sessionstate

// resume_credential_test.go — the store-side rules of the resume credential
// (docs/identity-resume-credential-design.md §3 and §5): generation, hash-only
// storage, the one-current-generation shape, the conditional-UPDATE fencing,
// replay accounting, the failed-ownership counter, and revocation as a
// consequence of a linkage write.

import (
	"database/sql"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// seedIdentity records an identity row linked to a conversation, the shape a
// credential hangs off.
func seedIdentity(t *testing.T, s *Store, proxy, external string) {
	t.Helper()
	if err := s.SaveIdentity(proxy, Identity{Name: "name-" + proxy, SessionID: "id-" + proxy, ExternalID: external}); err != nil {
		t.Fatalf("SaveIdentity(%s): %v", proxy, err)
	}
}

// mintFor mints a credential for proxy and returns its secret and generation.
func mintFor(t *testing.T, s *Store, proxy string) (secret string, gen int64) {
	t.Helper()
	secret, err := NewResumeSecret()
	if err != nil {
		t.Fatalf("NewResumeSecret: %v", err)
	}
	gen, err = s.MintResumeCredential(proxy, HashResumeSecret(secret))
	if err != nil {
		t.Fatalf("MintResumeCredential(%s): %v", proxy, err)
	}
	return secret, gen
}

func lookup(t *testing.T, s *Store, external, secret string) CredentialLookup {
	t.Helper()
	lk, err := s.LookupResumeCredential(external, HashResumeSecret(secret))
	if err != nil {
		t.Fatalf("LookupResumeCredential: %v", err)
	}
	return lk
}

var secretShape = regexp.MustCompile(`^rsk1-[A-Za-z0-9_-]{22}$`)

// The secret is `rsk1-` plus 22 base64url characters (128 bits, unpadded), fresh on
// every call. Nothing is a parameter, which is how "never derived" is enforced.
func TestNewResumeSecret_ShapeAndFreshness(t *testing.T) {
	seen := map[string]bool{}
	for range 500 {
		sec, err := NewResumeSecret()
		if err != nil {
			t.Fatal(err)
		}
		if !secretShape.MatchString(sec) {
			t.Fatalf("secret %q does not match rsk1-<22 base64url characters>", sec)
		}
		if seen[sec] {
			t.Fatalf("secret %q was generated twice", sec)
		}
		seen[sec] = true
	}
	a, b := HashResumeSecret("rsk1-aaaaaaaaaaaaaaaaaaaaaa"), HashResumeSecret("rsk1-aaaaaaaaaaaaaaaaaaaaab")
	if a == b || len(a) != 64 {
		t.Fatalf("hashes %q / %q: want distinct 64-character SHA-256 hex", a, b)
	}
}

// Only a hash is stored: a dump of the database discloses nothing a client could
// present.
func TestMint_StoresOnlyTheHash(t *testing.T) {
	s := newTestStore(t)
	seedIdentity(t, s, "P1", "conv-X")
	secret, _ := mintFor(t, s, "P1")

	rows, err := s.db.Query(`SELECT proxy_session_id, hash, state FROM resume_credential`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var p, h, st string
		if err := rows.Scan(&p, &h, &st); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(p+h+st, secret) || strings.Contains(h, "rsk1-") {
			t.Fatalf("the plaintext secret is in the table: %q", h)
		}
		if h != HashResumeSecret(secret) {
			t.Fatalf("stored %q, want the SHA-256 of the secret", h)
		}
	}
}

// A credential hangs off an identity record. Minting for a proxy that has none would
// leave a credential nothing can ever resolve (and is the shape an unproven
// connection would produce).
func TestMint_RequiresAnIdentityRecord(t *testing.T) {
	s := newTestStore(t)
	secret, err := NewResumeSecret()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MintResumeCredential("no-such-proxy", HashResumeSecret(secret)); err == nil {
		t.Fatal("minted a credential for a proxy session with no identity record")
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM resume_credential`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows after a refused mint = %d (%v), want 0", n, err)
	}
}

// One generation is live per identity; the immediately previous one is retained so
// a replay can be recognised; anything older is pruned.
func TestMint_OneCurrentThePreviousRetainedOlderPruned(t *testing.T) {
	s := newTestStore(t)
	seedIdentity(t, s, "P1", "conv-X")
	sec1, g1 := mintFor(t, s, "P1")
	sec2, g2 := mintFor(t, s, "P1")
	sec3, g3 := mintFor(t, s, "P1")
	if g1 != 1 || g2 != 2 || g3 != 3 {
		t.Fatalf("generations = %d, %d, %d, want 1, 2, 3", g1, g2, g3)
	}
	if got := lookup(t, s, "conv-X", sec3).State; got != CredentialCurrent {
		t.Errorf("newest generation is %q, want current", got)
	}
	if got := lookup(t, s, "conv-X", sec2).State; got != CredentialSuperseded {
		t.Errorf("previous generation is %q, want superseded (retained for replay detection)", got)
	}
	if got := lookup(t, s, "conv-X", sec1).State; got != "" {
		t.Errorf("a generation two back is %q, want pruned", got)
	}
}

func TestLookup_ScopedToTheConversationAndNeverWildcard(t *testing.T) {
	s := newTestStore(t)
	seedIdentity(t, s, "P1", "conv-X")
	seedIdentity(t, s, "P2", "conv-Y")
	secX, _ := mintFor(t, s, "P1")

	if got := lookup(t, s, "conv-X", secX).State; got != CredentialCurrent {
		t.Fatalf("the credential does not resolve for its own conversation: %q", got)
	}
	if lk := lookup(t, s, "conv-Y", secX); lk.State != "" {
		t.Errorf("conv-X's credential resolved for conv-Y: %+v", lk)
	}
	if lk := lookup(t, s, "", secX); lk.State != "" || lk.HasCurrent {
		t.Errorf("a blank conversation matched: %+v", lk)
	}
	// A conversation with no record at all.
	if lk := lookup(t, s, "conv-none", secX); lk.State != "" || lk.HasCurrent {
		t.Errorf("a conversation with no record reports %+v", lk)
	}
}

// Fencing: N claimants present the same current secret; exactly one conditional
// UPDATE lands.
func TestClaim_ExactlyOneConcurrentClaimantWins(t *testing.T) {
	s := newTestStore(t)
	seedIdentity(t, s, "P1", "conv-X")
	secret, gen := mintFor(t, s, "P1")
	const claimants = 16

	var wins atomic.Int32
	var wg sync.WaitGroup
	var start sync.WaitGroup
	start.Add(1)
	for i := range claimants {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start.Wait()
			won, consumed, err := s.ClaimResumeCredential("conv-X", HashResumeSecret(secret))
			if err != nil {
				t.Errorf("claimant %d: %v", i, err)
				return
			}
			if won {
				wins.Add(1)
				if consumed != gen {
					t.Errorf("consumed generation = %d, want %d", consumed, gen)
				}
			}
		}()
	}
	start.Done()
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d claimants won the claim, want exactly 1", wins.Load())
	}
	if got := lookup(t, s, "conv-X", secret).State; got != CredentialSuperseded {
		t.Errorf("the consumed generation is %q, want superseded", got)
	}
}

// A rotation touches the presented credential's own row and no other. Two identities
// hold a credential for one conversation: consuming one leaves the other current. The
// old behaviour superseded every current credential of the conversation, so a
// connection that merely CLAIMED a victim's conversation (an unrefused relink) could
// present its own credential and revoke the victim's in one shot.
func TestRotate_TouchesOnlyThePresentedCredentialsRow(t *testing.T) {
	s := newTestStore(t)
	seedIdentity(t, s, "P1", "conv-X")
	seedIdentity(t, s, "P2", "conv-X") // a second identity holding a credential for the same conversation
	seedIdentity(t, s, "P3", "")       // the claimant, linked once the winner is recorded
	seedIdentity(t, s, "Q1", "conv-Z") // another conversation entirely
	sec1, _ := mintFor(t, s, "P1")
	sec2, _ := mintFor(t, s, "P2")
	secZ, _ := mintFor(t, s, "Q1")
	if err := s.SaveIdentity("P3", Identity{Name: "name-P3", SessionID: "id-P3", ExternalID: "conv-X"}); err != nil {
		t.Fatal(err)
	}

	won, consumed, err := s.ClaimResumeCredential("conv-X", HashResumeSecret(sec1))
	if err != nil || !won {
		t.Fatalf("claim = (%v, %v), want a win", won, err)
	}
	succ, _ := NewResumeSecret()
	gen, err := s.IssueResumeSuccessor("P3", HashResumeSecret(succ), consumed)
	if err != nil {
		t.Fatalf("IssueResumeSuccessor: %v", err)
	}
	if gen <= consumed {
		t.Errorf("successor generation = %d, want it past the consumed %d", gen, consumed)
	}
	if got := lookup(t, s, "conv-X", succ).State; got != CredentialCurrent {
		t.Errorf("successor is %q, want current", got)
	}
	if got := lookup(t, s, "conv-X", sec1).State; got != CredentialSuperseded {
		t.Errorf("presented generation is %q, want superseded", got)
	}
	if got := lookup(t, s, "conv-X", sec2).State; got != CredentialCurrent {
		t.Errorf("the OTHER identity's credential for the same conversation is %q, want untouched (current)", got)
	}
	if got := lookup(t, s, "conv-Z", secZ).State; got != CredentialCurrent {
		t.Errorf("another conversation's credential is %q, want untouched (current)", got)
	}
}

// Only a CURRENT generation may be claimed, and a refused claim changes nothing.
func TestClaim_RefusesAStaleOrMisdirectedSecretAndChangesNothing(t *testing.T) {
	s := newTestStore(t)
	seedIdentity(t, s, "P1", "conv-X")
	old, _ := mintFor(t, s, "P1")
	cur, _ := mintFor(t, s, "P1") // old is now superseded

	won, _, err := s.ClaimResumeCredential("conv-X", HashResumeSecret(old))
	if err != nil || won {
		t.Fatalf("claiming a superseded generation = (%v, %v), want refused", won, err)
	}
	if got := lookup(t, s, "conv-X", cur).State; got != CredentialCurrent {
		t.Errorf("a refused claim moved the current generation to %q", got)
	}
	// A secret presented against the wrong conversation does not claim either.
	won, _, _ = s.ClaimResumeCredential("conv-OTHER", HashResumeSecret(cur))
	if won {
		t.Error("claimed a credential on behalf of a conversation it is not linked to")
	}
	if got := lookup(t, s, "conv-X", cur).State; got != CredentialCurrent {
		t.Errorf("a misdirected claim moved the current generation to %q", got)
	}
}

// A claimant whose restore could not complete gives the generation back, and only a
// generation that is still exactly where its claim left it: a revoked one stays
// revoked, and one overtaken by a newer generation of the same identity stays
// superseded.
func TestRelease_GivesBackOnlyAGenerationThatIsStillWhereTheClaimLeftIt(t *testing.T) {
	t.Run("a claimed generation goes back to current", func(t *testing.T) {
		s := newTestStore(t)
		seedIdentity(t, s, "P1", "conv-X")
		sec, _ := mintFor(t, s, "P1")
		if won, _, err := s.ClaimResumeCredential("conv-X", HashResumeSecret(sec)); err != nil || !won {
			t.Fatalf("claim = (%v, %v)", won, err)
		}
		released, err := s.ReleaseResumeCredential(HashResumeSecret(sec))
		if err != nil || !released {
			t.Fatalf("release = (%v, %v), want it given back", released, err)
		}
		if got := lookup(t, s, "conv-X", sec).State; got != CredentialCurrent {
			t.Errorf("a released generation is %q, want current", got)
		}
		if won, _, _ := s.ClaimResumeCredential("conv-X", HashResumeSecret(sec)); !won {
			t.Error("a released generation cannot be claimed again for the retry")
		}
	})
	t.Run("a current generation is not touched", func(t *testing.T) {
		s := newTestStore(t)
		seedIdentity(t, s, "P1", "conv-X")
		sec, _ := mintFor(t, s, "P1")
		if released, err := s.ReleaseResumeCredential(HashResumeSecret(sec)); err != nil || released {
			t.Errorf("release of a current generation = (%v, %v), want a no-op", released, err)
		}
	})
	t.Run("a generation overtaken by a newer one of the same identity stays superseded", func(t *testing.T) {
		s := newTestStore(t)
		seedIdentity(t, s, "P1", "conv-X")
		sec, _ := mintFor(t, s, "P1")
		if won, _, _ := s.ClaimResumeCredential("conv-X", HashResumeSecret(sec)); !won {
			t.Fatal("claim lost")
		}
		newer, _ := mintFor(t, s, "P1") // the identity's own serve restored meanwhile
		if released, _ := s.ReleaseResumeCredential(HashResumeSecret(sec)); released {
			t.Error("a superseded generation was resurrected beside a newer current one")
		}
		if got := lookup(t, s, "conv-X", newer).State; got != CredentialCurrent {
			t.Errorf("the newer generation is %q, want current", got)
		}
	})
	t.Run("a revoked generation stays revoked", func(t *testing.T) {
		s := newTestStore(t)
		seedIdentity(t, s, "P1", "conv-X")
		sec, _ := mintFor(t, s, "P1")
		if won, _, _ := s.ClaimResumeCredential("conv-X", HashResumeSecret(sec)); !won {
			t.Fatal("claim lost")
		}
		if err := s.SaveIdentity("P1", Identity{Name: "name-P1", SessionID: "id-P1", ExternalID: "conv-Y"}); err != nil {
			t.Fatal(err) // replacing the linkage revokes the identity's credentials
		}
		if released, _ := s.ReleaseResumeCredential(HashResumeSecret(sec)); released {
			t.Error("a revoked generation was given back")
		}
		if got := lookup(t, s, "conv-X", sec).State; got != CredentialRevoked {
			t.Errorf("the generation is %q, want revoked", got)
		}
	})
}

// A presented secret matching nothing for a conversation that has a current
// credential counts; the third failure revokes. Superseded matches are answered, not
// counted, and a conversation with no record is never counted.
func TestFailedPresentations_RevokeAtThreeAndOnlyThose(t *testing.T) {
	s := newTestStore(t)
	seedIdentity(t, s, "P1", "conv-X")
	secret, _ := mintFor(t, s, "P1")

	if counted, revoked, err := s.NoteFailedResumePresentation("conv-no-record"); err != nil || counted || revoked {
		t.Fatalf("a conversation with no record counted: (%v, %v, %v)", counted, revoked, err)
	}
	for i := 1; i <= 2; i++ {
		counted, revoked, err := s.NoteFailedResumePresentation("conv-X")
		if err != nil || !counted || revoked {
			t.Fatalf("failure %d = (%v, %v, %v), want counted and not yet revoked", i, counted, revoked, err)
		}
		if got := lookup(t, s, "conv-X", secret).State; got != CredentialCurrent {
			t.Fatalf("the credential is %q after %d failures, want still current", got, i)
		}
	}
	counted, revoked, err := s.NoteFailedResumePresentation("conv-X")
	if err != nil || !counted || !revoked {
		t.Fatalf("third failure = (%v, %v, %v), want revoked", counted, revoked, err)
	}
	if got := lookup(t, s, "conv-X", secret).State; got != CredentialRevoked {
		t.Fatalf("after three failures the credential is %q, want revoked", got)
	}
	// Nothing current remains, so a further junk presentation counts for nothing.
	if counted, _, _ := s.NoteFailedResumePresentation("conv-X"); counted {
		t.Error("a presentation against a conversation with no current credential counted")
	}
}

// Detach: replacing the record's external linkage revokes the credential with it,
// and nothing else about a save does.
func TestSaveIdentity_ReplacingTheLinkageRevokes(t *testing.T) {
	s := newTestStore(t)
	seedIdentity(t, s, "P1", "conv-X")
	old, _ := mintFor(t, s, "P1")
	cur, _ := mintFor(t, s, "P1")

	// Neither a re-save of the same linkage nor a blank one (which never clears a
	// known linkage) touches the credential.
	for _, ext := range []string{"conv-X", ""} {
		if err := s.SaveIdentity("P1", Identity{Name: "name-P1", SessionID: "id-P1", ExternalID: ext}); err != nil {
			t.Fatal(err)
		}
		if got := lookup(t, s, "conv-X", cur).State; got != CredentialCurrent {
			t.Fatalf("a save with linkage %q left the credential %q, want current", ext, got)
		}
	}

	if err := s.SaveIdentity("P1", Identity{Name: "name-P1", SessionID: "id-P1", ExternalID: "conv-Y"}); err != nil {
		t.Fatal(err)
	}
	// The row now answers to conv-Y, and its credentials died with the old linkage:
	// a holder presenting either secret, for either conversation, is told "revoked".
	// The row can no longer be found through conv-X, which is why a revoked secret
	// resolves on its hash alone.
	for _, ext := range []string{"conv-X", "conv-Y"} {
		for name, sec := range map[string]string{"previous": old, "current": cur} {
			if got := lookup(t, s, ext, sec).State; got != CredentialRevoked {
				t.Errorf("after the linkage moved, the %s secret for %s is %q, want revoked", name, ext, got)
			}
		}
	}
}

// A first-ever linkage (blank to known) is how every identity acquires one and must
// not revoke the credential minted at initialize, before session_start linked it.
func TestSaveIdentity_FirstLinkageKeepsTheCredential(t *testing.T) {
	s := newTestStore(t)
	seedIdentity(t, s, "P1", "")
	secret, _ := mintFor(t, s, "P1")
	if err := s.SaveIdentity("P1", Identity{Name: "name-P1", SessionID: "id-P1", ExternalID: "conv-X"}); err != nil {
		t.Fatal(err)
	}
	if got := lookup(t, s, "conv-X", secret).State; got != CredentialCurrent {
		t.Fatalf("linking a conversation for the first time left the credential %q, want current", got)
	}
}

// No TTL: the age sweep never touches a credential.
func TestCredentials_AreNeverPrunedByAge(t *testing.T) {
	s := newTestStore(t)
	seedIdentity(t, s, "P1", "conv-X")
	secret, _ := mintFor(t, s, "P1")
	if err := s.BackdateSession("P1", time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	if err := s.Prune(time.Now().Add(24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := lookup(t, s, "conv-X", secret).State; got != CredentialCurrent {
		t.Fatalf("an aged credential is %q after the sweep, want current", got)
	}
}

// An unreadable credential table degrades to "no credential recorded": lookups and
// mints fail closed, and the identity record is untouched and still writable.
func TestBrokenCredentialTable_FailsClosedAndLeavesIdentityAlone(t *testing.T) {
	s := newTestStore(t)
	seedIdentity(t, s, "P1", "conv-X")
	secret, _ := mintFor(t, s, "P1")
	if _, err := s.db.Exec(`DROP TABLE resume_credential`); err != nil {
		t.Fatal(err)
	}
	if lk, err := s.LookupResumeCredential("conv-X", HashResumeSecret(secret)); err == nil && lk.State == CredentialCurrent {
		t.Fatal("a lookup against a missing table reported a current credential")
	}
	if _, err := s.MintResumeCredential("P1", HashResumeSecret(secret)); err == nil {
		t.Fatal("minted into a missing table")
	}
	// Linkage replacement must still write the identity (the revoke is best effort
	// here precisely because the table cannot be read).
	if err := s.SaveIdentity("P1", Identity{Name: "name-P1", SessionID: "id-P1", ExternalID: "conv-Y"}); err != nil {
		t.Fatalf("SaveIdentity failed because the credential table is broken: %v", err)
	}
	if rec, ok, err := s.LoadIdentity("P1"); err != nil || !ok || rec.ExternalID != "conv-Y" {
		t.Fatalf("identity after the save = (%+v, %v, %v)", rec, ok, err)
	}
}

// The v11 step creates the credential table, keeps every identity row, and is
// idempotent in its own right: a database whose step already ran but whose version
// was never stamped (a crash between them) opens cleanly instead of failing on the
// existing table.
func TestMigrateV10ToV11_AddsTheCredentialTable(t *testing.T) {
	for _, keepTable := range []bool{false, true} {
		name := "a v10 database"
		if keepTable {
			name = "a v10 database whose step already created the table"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			s, err := openAt(path)
			if err != nil {
				t.Fatal(err)
			}
			seedIdentity(t, s, "legacy", "conv-X")
			s.Close()
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			stmts := []string{`PRAGMA user_version = 10`}
			if !keepTable {
				stmts = append(stmts, `DROP TABLE resume_credential`)
			}
			for _, q := range stmts {
				if _, err := db.Exec(q); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
			}
			db.Close()

			for i := range 2 { // the second open is the idempotence check
				s, err := openAt(path)
				if err != nil {
					t.Fatalf("open #%d: %v", i+1, err)
				}
				if got := userVersion(t, s); got != SchemaVersion || SchemaVersion != 11 {
					t.Fatalf("user_version = %d, SchemaVersion = %d, want 11", got, SchemaVersion)
				}
				if !tableExists(t, s, "resume_credential") {
					t.Fatalf("open #%d: the credential table is missing", i+1)
				}
				if rec, ok, _ := s.LoadIdentity("legacy"); !ok || rec.ExternalID != "conv-X" {
					t.Fatalf("open #%d: the pre-v11 identity row did not survive: %+v", i+1, rec)
				}
				// A pre-existing identity is credential-less until it is re-established.
				if lk := lookup(t, s, "conv-X", "rsk1-aaaaaaaaaaaaaaaaaaaaaa"); lk.State != "" || lk.HasCurrent {
					t.Fatalf("open #%d: a legacy identity reports a credential: %+v", i+1, lk)
				}
				s.Close()
			}
		})
	}
}
