package session_test

// Tests for session-name uniqueness: the rules that make a name safe to use as
// a mailbox address. Split from session_test.go, which covers the registry's
// general lifecycle (atomic writes, listing, liveness, purpose, client info).

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/session"
)

// TestRename_RefusesNameHeldByLiveSession is the guard that makes a session name
// usable as an address at all. collab_rows.addressee stores the name string and
// ClaimNotes' atomic claim hands each message to whichever session asks first,
// so two live sessions under one name do not duplicate a message — they
// silently misdeliver it, and the intended recipient never learns it existed.
func TestRename_RefusesNameHeldByLiveSession(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	holder, err := registerID(session.Info{Name: "reviewer"})
	if err != nil {
		t.Fatalf("Register holder: %v", err)
	}
	defer session.Unregister(holder)
	other, err := registerID(session.Info{Name: "coder"})
	if err != nil {
		t.Fatalf("Register other: %v", err)
	}
	defer session.Unregister(other)

	if _, err := session.Rename(other, "reviewer"); !errors.Is(err, session.ErrNameTaken) {
		t.Fatalf("Rename onto a live peer's name = %v, want ErrNameTaken", err)
	}
	if _, err := session.Rename(other, "REVIEWER"); !errors.Is(err, session.ErrNameTaken) {
		t.Fatalf("Rename onto a case variant of a live name = %v, want ErrNameTaken", err)
	}

	// A refused rename must leave the session as it was.
	live, err := session.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, info := range live {
		if info.ID == other && info.Name != "coder" {
			t.Fatalf("refused rename still changed the name to %q", info.Name)
		}
	}
}

// TestRename_AllowsOwnName: restoreName re-applies the persisted name on every
// reconnect to refresh its TTL, so a self-match counted as a collision would
// turn that no-op into an error exactly when the session is most fragile.
func TestRename_AllowsOwnName(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	id, err := registerID(session.Info{Name: "steady-heron"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	defer session.Unregister(id)

	if _, err := session.Rename(id, "steady-heron"); err != nil {
		t.Fatalf("Rename to own name: %v", err)
	}
	// Re-casing your own name is a rename, not a collision with yourself.
	got, err := session.Rename(id, "Steady-Heron")
	if err != nil {
		t.Fatalf("Rename to a case variant of own name: %v", err)
	}
	if got != "Steady-Heron" {
		t.Fatalf("Rename returned %q, want Steady-Heron", got)
	}
}

// TestRename_EndedSessionDoesNotReserveItsName: Unregister marks EndedAt and
// keeps the file for the FindEnded grace window, but an ended session is not an
// addressee — holding its name for a day would drain the pool on a busy daemon.
func TestRename_EndedSessionDoesNotReserveItsName(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	gone, err := registerID(session.Info{Name: "retired-fox"})
	if err != nil {
		t.Fatalf("Register gone: %v", err)
	}
	session.Unregister(gone)

	live, err := registerID(session.Info{Name: "active-bee"})
	if err != nil {
		t.Fatalf("Register live: %v", err)
	}
	defer session.Unregister(live)

	if _, err := session.Rename(live, "retired-fox"); err != nil {
		t.Fatalf("Rename onto an ended session's name = %v, want success", err)
	}
}

// TestRegister_AssignsAFreeUsableName. Register returning the completed record
// is the point of the signature: newConnSession needs the assigned name for the
// session view, and having the caller pre-generate one is what allowed the
// unchecked draw in the first place.
func TestRegister_AssignsAFreeUsableName(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	reg, err := session.Register(session.Info{})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	defer session.Unregister(reg.ID)

	if reg.ID == "" {
		t.Error("Register returned an empty ID")
	}
	if reg.Name == "" {
		t.Fatal("Register returned an empty Name; the caller has nothing to display")
	}
	if _, err := session.NormaliseName(reg.Name); err != nil {
		t.Errorf("assigned name %q does not survive NormaliseName: %v", reg.Name, err)
	}
}

// TestRegister_RefusesACallerSuppliedNameThatIsTaken. Only a generated name is
// disambiguated with a suffix; a caller that named itself is told no, because
// silently handing back a different name would leave it addressing peers under
// an identity it does not actually have.
func TestRegister_RefusesACallerSuppliedNameThatIsTaken(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	first, err := registerID(session.Info{Name: "twin"})
	if err != nil {
		t.Fatalf("Register first: %v", err)
	}
	defer session.Unregister(first)

	if _, err := session.Register(session.Info{Name: "twin"}); !errors.Is(err, session.ErrNameTaken) {
		t.Fatalf("Register with a taken name = %v, want ErrNameTaken", err)
	}
}

// TestNormaliseName_RejectsTheReservedNextAddress. "next" is the mailbox's
// next-arrival sentinel and leave_note resolves it before any session lookup,
// so a session holding that name could never be addressed while shadowing the
// broadcast address for everyone else.
func TestNormaliseName_RejectsTheReservedNextAddress(t *testing.T) {
	for _, name := range []string{"next", "Next", "NEXT"} {
		if _, err := session.NormaliseName(name); err == nil {
			t.Errorf("NormaliseName(%q) = nil, want the reserved-name refusal", name)
		}
	}
	// Only the exact word is reserved.
	if _, err := session.NormaliseName("next-gen"); err != nil {
		t.Errorf("NormaliseName(\"next-gen\") = %v, want nil", err)
	}
}

// TestRename_ConcurrentClaimsOfOneName pins that the check and the write share
// one lock. Checking before taking the flock would let every goroutine see the
// name free and all of them write it — which is the pre-existing bug, just
// harder to hit. Mutation check: moving the nameTaken call outside
// withSessionDirLock makes this fail.
func TestRename_ConcurrentClaimsOfOneName(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	const contenders = 8

	ids := make([]string, contenders)
	for i := range ids {
		id, err := registerID(session.Info{Name: fmt.Sprintf("starter-%d", i)})
		if err != nil {
			t.Fatalf("Register %d: %v", i, err)
		}
		ids[i] = id
		defer session.Unregister(id)
	}

	var (
		won   atomic.Int32
		wg    sync.WaitGroup
		start = make(chan struct{})
	)
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			<-start
			if _, err := session.Rename(id, "contested"); err == nil {
				won.Add(1)
			}
		}(id)
	}
	close(start)
	wg.Wait()

	if got := won.Load(); got != 1 {
		t.Fatalf("%d sessions claimed the name, want exactly 1", got)
	}

	live, err := session.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	holders := 0
	for _, info := range live {
		if info.Name == "contested" {
			holders++
		}
	}
	if holders != 1 {
		t.Fatalf("%d live sessions answer to 'contested', want 1", holders)
	}
}

// TestRegister_RefusesAnInvalidCallerSuppliedName. Register checking uniqueness
// but not VALIDITY would make it the one door into the registry that bypasses
// NormaliseName — it would store the reserved "next" as a live address, and a
// name carrying a newline or colon, which internal/tools/git.go relies on being
// impossible to keep the Plumb-Session commit trailer single-line.
func TestRegister_RefusesAnInvalidCallerSuppliedName(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	for _, name := range []string{
		"next", "NEXT", // the reserved mailbox address
		"has space", "trailing-", "-leading", "double--hyphen",
		"colon:name", "new\nline", // the git-trailer invariant
		strings.Repeat("x", session.MaxNameLength+1),
	} {
		if _, err := session.Register(session.Info{Name: name}); err == nil {
			t.Errorf("Register(%q) succeeded; want a validation error", name)
		}
	}

	// Nothing invalid reached the registry.
	live, err := session.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("%d sessions registered despite invalid names: %+v", len(live), live)
	}
}

// TestRegister_ConcurrentDrawsAreUnique is the Register-side twin of
// TestRename_ConcurrentClaimsOfOneName. With the draw forced to collide every
// time, the suffix path runs under real contention; if freeName ran outside the
// flock every contender would see the same set free and pick the same name.
func TestRegister_ConcurrentDrawsAreUnique(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	session.SetGenerateNameForTest(t, func() string { return "always-collides" })
	const contenders = 12

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		names []string
		start = make(chan struct{})
	)
	for range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			reg, err := session.Register(session.Info{})
			if err != nil {
				t.Errorf("Register: %v", err)
				return
			}
			mu.Lock()
			names = append(names, reg.Name)
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	seen := make(map[string]bool, len(names))
	for _, n := range names {
		if seen[n] {
			t.Errorf("duplicate name assigned: %q", n)
		}
		seen[n] = true
		if _, err := session.NormaliseName(n); err != nil {
			t.Errorf("assigned name %q is not valid: %v", n, err)
		}
	}
	if len(names) != contenders {
		t.Fatalf("%d sessions registered, want %d", len(names), contenders)
	}
}

// TestRegister_SuffixesPastALiveCollision pins that Register actually WIRES
// freeName up — freeName is unit-tested as a pure function, but nothing else
// asserts the disambiguated name reaches the session file.
func TestRegister_SuffixesPastALiveCollision(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	session.SetGenerateNameForTest(t, func() string { return "amber-antelope" })

	first, err := session.Register(session.Info{})
	if err != nil {
		t.Fatalf("Register first: %v", err)
	}
	defer session.Unregister(first.ID)
	if first.Name != "amber-antelope" {
		t.Fatalf("first name = %q, want amber-antelope", first.Name)
	}

	second, err := session.Register(session.Info{})
	if err != nil {
		t.Fatalf("Register second: %v", err)
	}
	defer session.Unregister(second.ID)
	if second.Name != "amber-antelope-2" {
		t.Fatalf("second name = %q, want amber-antelope-2", second.Name)
	}

	// And it is what was persisted, not just what was returned.
	live, err := session.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := map[string]bool{}
	for _, info := range live {
		got[info.Name] = true
	}
	if !got["amber-antelope"] || !got["amber-antelope-2"] {
		t.Fatalf("session files hold %v, want both amber-antelope and amber-antelope-2", got)
	}
}

// TestRename_DoesNotDeleteTheFileItReads. Rename calls listLocked under the same
// lock, and listLocked prunes ended session files past the 24h grace. Scanning
// BEFORE reading meant Rename could delete the very file it was about to read,
// then fail with a self-inflicted ENOENT reported as "reading session file" —
// an error it caused rather than found. Reading first makes the outcome
// independent of how long ago some unrelated cleanup threshold passed.
//
// Mutation check: swapping the read and the scan back makes this fail.
func TestRename_DoesNotDeleteTheFileItReads(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	dir, err := session.Dir()
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// This session is itself ended well past the grace window, so the prune
	// inside listLocked targets the file Rename needs.
	const id = "expired-session"
	blob := fmt.Sprintf(`{"id":%q,"name":"long-gone","pid":%d,"started_at":%q,"ended_at":%q}`,
		id, os.Getpid(),
		time.Now().Add(-48*time.Hour).Format(time.RFC3339Nano),
		time.Now().Add(-25*time.Hour).Format(time.RFC3339Nano))
	if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(blob), 0o600); err != nil {
		t.Fatalf("seed expired session: %v", err)
	}

	got, err := session.Rename(id, "renamed-anyway")
	if err != nil {
		t.Fatalf("Rename of a session pending prune: %v", err)
	}
	if got != "renamed-anyway" {
		t.Fatalf("Rename returned %q, want renamed-anyway", got)
	}
}

// TestPatch_CannotChangeTheName. Patch is the third path that writes a session
// file. It takes the flock, so it cannot race — but it wrote whatever the
// callback produced, so it could set a name past both the validation and the
// uniqueness check. A write primitive that can set an address is a door around
// the guard, not a second guard.
func TestPatch_CannotChangeTheName(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	alpha, err := registerID(session.Info{Name: "alpha"})
	if err != nil {
		t.Fatalf("Register alpha: %v", err)
	}
	defer session.Unregister(alpha)
	beta, err := registerID(session.Info{Name: "beta"})
	if err != nil {
		t.Fatalf("Register beta: %v", err)
	}
	defer session.Unregister(beta)

	session.Patch(beta, func(in *session.Info) {
		in.Name = "alpha"    // must be discarded
		in.Purpose = "probe" // must still apply
	})

	live, err := session.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	named := 0
	for _, info := range live {
		if info.Name == "alpha" {
			named++
		}
		if info.ID == beta {
			if info.Name != "beta" {
				t.Errorf("Patch changed the name to %q; Rename owns that field", info.Name)
			}
			if info.Purpose != "probe" {
				t.Errorf("Patch dropped an unrelated field: Purpose = %q", info.Purpose)
			}
		}
	}
	if named != 1 {
		t.Fatalf("%d live sessions answer to \"alpha\", want 1", named)
	}
}
