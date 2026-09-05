package session

// reserved_test.go — names held for sessions that are not live.
//
// The live-session uniqueness check is correct for every session that is
// running, and silently wrong for one that is not: a `plumb serve` outliving its
// daemon has no live record at all, so for the length of the outage its name
// belongs to nobody and the next session to draw one can take it. These tests
// pin the reservation that closes that window, and — just as importantly — that
// it never blocks the identity it is held for.

import (
	"errors"
	"strings"
	"testing"
)

func TestReserved_RefusesAnotherSessionAndAdmitsTheOwner(t *testing.T) {
	r := Reserved{"calm-stag": "id-owner"}
	if !r.taken("calm-stag", "id-other") {
		t.Error("a reserved name was offered to a different session")
	}
	if r.taken("calm-stag", "id-owner") {
		t.Error("a session was refused its OWN reservation; the reservation exists to hold the " +
			"name for exactly that session")
	}
	// Case-insensitive, matching nameTaken. Being stricter than the mailbox's
	// case-sensitive delivery can only reject a confusable name, never admit one.
	if !r.taken("CALM-STAG", "id-other") {
		t.Error("a reservation was evaded by changing case")
	}
	if r.taken("lone-dingo", "id-other") {
		t.Error("an unreserved name was refused")
	}
}

func TestReserved_EmptySetChangesNothing(t *testing.T) {
	// Every existing call path passes nil. It must behave exactly as it did
	// before reservations existed, or this feature is a behaviour change
	// everywhere rather than in the one place it belongs.
	for _, r := range []Reserved{nil, {}} {
		if r.taken("calm-stag", "id-1") {
			t.Errorf("%v refused a name with nothing reserved", r)
		}
	}
}

func TestRegisterReserved_RedrawsPastAReservedName(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	// Force every draw onto one name, then reserve it: the generated-name path
	// must route around it rather than hand it out.
	stubDraw(t, "calm-stag")

	info, err := RegisterReserved(Info{}, Reserved{"calm-stag": "id-absent"})
	if err != nil {
		t.Fatalf("RegisterReserved: %v", err)
	}
	t.Cleanup(func() { Unregister(info.ID) })
	if strings.EqualFold(info.Name, "calm-stag") {
		t.Fatalf("a new session was given the reserved name %q; the session it is held for would "+
			"come back renamed with its mail orphaned", info.Name)
	}
	// It must still be a legal, usable name rather than a bare suffix.
	if _, err := NormaliseName(info.Name); err != nil {
		t.Errorf("the redrawn name %q is not a legal session name: %v", info.Name, err)
	}
}

func TestRegisterReserved_RefusesAnExplicitlyRequestedReservedName(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	// A caller that asked for a particular name gets an error rather than a
	// substitute: silently renaming a caller's choice is how a rename becomes
	// invisible.
	_, err := RegisterReserved(Info{Name: "calm-stag"}, Reserved{"calm-stag": "id-absent"})
	if !errors.Is(err, ErrNameTaken) {
		t.Fatalf("RegisterReserved with a reserved name = %v, want ErrNameTaken", err)
	}
}

func TestRenameReserved_RefusesAReservedNameAndAllowsTheOwner(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	mine, err := Register(Info{Name: "velvet-bison"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { Unregister(mine.ID) })

	if _, err := RenameReserved(mine.ID, "calm-stag", Reserved{"calm-stag": "id-absent"}); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("rename to a name reserved for another session = %v, want ErrNameTaken", err)
	}
	// The same name, reserved for THIS session, is the recovery case and must
	// succeed — otherwise an identity could never take back the name being held
	// for it.
	got, err := RenameReserved(mine.ID, "calm-stag", Reserved{"calm-stag": mine.ID})
	if err != nil {
		t.Fatalf("a session was refused the name reserved for it: %v", err)
	}
	if got != "calm-stag" {
		t.Fatalf("rename returned %q, want calm-stag", got)
	}
}

// TestFreeName_TerminatesWhenEveryDrawIsReserved is the bound stated in
// freeName's comment, exercised rather than assumed.
//
// Reservations accumulate over a database's lifetime where live sessions do not,
// so the adjective/noun pool CAN fill — and the old pigeonhole argument ("only a
// live session can occupy a suffix") no longer covers it. If the suffix path did
// not also consult reservations this would loop forever rather than fail, which
// is why it is worth a test of its own.
func TestFreeName_TerminatesWhenEveryDrawIsReserved(t *testing.T) {
	stubDraw(t, "calm-stag")
	reserved := Reserved{"calm-stag": "id-absent"}
	for i := 2; i <= 40; i++ {
		reserved[strings.ToLower(withSuffix("calm-stag", i))] = "id-absent"
	}
	got := freeName(nil, "self", reserved)
	if reserved[strings.ToLower(got)] != "" {
		t.Fatalf("freeName returned the reserved name %q", got)
	}
	if _, err := NormaliseName(got); err != nil {
		t.Errorf("freeName returned an illegal name %q: %v", got, err)
	}
}

// TestNormaliseName_ClassifiesInvalidNames: identity recovery reacts differently
// to "this name is invalid" (replace the record — it can never succeed) and
// "this name is busy" or "the disk failed" (preserve the record and retry). The
// old code separated them by "is it ErrNameTaken?", which classified an I/O
// failure as an invalid name and would overwrite a perfectly good identity
// because a disk was briefly busy.
func TestNormaliseName_ClassifiesInvalidNames(t *testing.T) {
	for _, name := range []string{"", "next", strings.Repeat("a", MaxNameLength+1), "-lead", "trail-", "a--b", "has space", "émoji"} {
		if _, err := NormaliseName(name); !errors.Is(err, ErrInvalidName) {
			t.Errorf("NormaliseName(%q) = %v, want an ErrInvalidName", name, err)
		}
	}
	if _, err := NormaliseName("calm-stag"); err != nil {
		t.Errorf("NormaliseName rejected a legal name: %v", err)
	}
	// A busy name is emphatically NOT invalid: the two call for opposite
	// responses from the recovery path.
	if errors.Is(ErrNameTaken, ErrInvalidName) {
		t.Error("ErrNameTaken matches ErrInvalidName; the recovery path cannot then tell a " +
			"transient collision from a permanently unusable name")
	}
}
