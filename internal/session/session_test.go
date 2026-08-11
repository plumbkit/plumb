package session_test

import (
	"encoding/json"
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

// TestWriteSessionFileAtomic_NoTornReads guards that session-file writes are
// atomic: a concurrent reader (in production, the TUI refresh racing the daemon
// reaper across processes) must never observe a partially-written file, and no
// temp file may be left behind. Before the temp-file+rename change, Patch used a
// plain os.WriteFile and a reader could catch a truncated file mid-write — a
// real hazard now that List has write side effects and is called from the TUI.
func TestWriteSessionFileAtomic_NoTornReads(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	id, err := registerID(session.Info{Folder: "/tmp/x", Adapter: "gopls"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	dir, err := session.Dir()
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	path := filepath.Join(dir, id+".json")

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Reader: continuously read the file raw. With atomic writes it is always
	// either the old or the new complete file, never torn.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			data, rerr := os.ReadFile(path)
			if rerr != nil || len(data) == 0 {
				continue
			}
			var in session.Info
			if uerr := json.Unmarshal(data, &in); uerr != nil {
				t.Errorf("observed a torn session file: %v (%q)", uerr, data)
				return
			}
		}
	}()

	// Writers: many concurrent Patches to the same file.
	for i := range 50 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			session.Patch(id, func(in *session.Info) { in.Adapter = fmt.Sprintf("a%d", n) })
		}(i)
	}

	time.Sleep(50 * time.Millisecond) // let writers and the reader overlap
	close(stop)
	wg.Wait()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover temp file after atomic writes: %s", e.Name())
		}
	}
}

func TestSessionPatchesSerializeReadModifyWrite(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	id, err := registerID(session.Info{Folder: "/tmp/x", Adapter: "gopls"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	defer session.Unregister(id)

	firstEntered := make(chan struct{})
	firstRelease := make(chan struct{})
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		session.Patch(id, func(in *session.Info) {
			close(firstEntered)
			<-firstRelease
			in.ClientName = "codex"
		})
	}()

	select {
	case <-firstEntered:
	case <-time.After(time.Second):
		t.Fatal("first patch did not enter")
	}

	secondEntered := make(chan struct{})
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		session.Patch(id, func(in *session.Info) {
			close(secondEntered)
			in.ExternalID = "agent-1"
		})
	}()

	select {
	case <-secondEntered:
		t.Fatal("second patch entered while first patch held the session lock")
	case <-time.After(100 * time.Millisecond):
	}

	close(firstRelease)
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first patch did not finish")
	}
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("second patch did not finish")
	}

	dir, err := session.Dir()
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, id+".json"))
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	var got session.Info
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal session: %v", err)
	}
	if got.ClientName != "codex" || got.ExternalID != "agent-1" {
		t.Fatalf("patches lost updates: ClientName=%q ExternalID=%q", got.ClientName, got.ExternalID)
	}
}

func TestRegisterUnregister(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	id, err := registerID(session.Info{
		Language: "go",
		Folder:   "/tmp/myproject",
		Adapter:  "gopls",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty session ID")
	}

	sessions, err := session.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	s := sessions[0]
	if s.ID != id {
		t.Errorf("ID: got %q, want %q", s.ID, id)
	}
	if s.Language != "go" {
		t.Errorf("Language: got %q", s.Language)
	}
	if s.Name == "" || s.Name != strings.ToLower(s.Name) {
		t.Errorf("Name: got %q, want automatic lowercase name", s.Name)
	}
	if s.PID != os.Getpid() {
		t.Errorf("PID: got %d, want %d", s.PID, os.Getpid())
	}
	if s.StartedAt.IsZero() {
		t.Error("StartedAt is zero")
	}

	session.Unregister(id)

	sessions, err = session.List()
	if err != nil {
		t.Fatalf("List after unregister: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("expected 0 sessions after unregister, got %d", len(sessions))
	}
}

func TestGenerateNameLowercase(t *testing.T) {
	name := session.GenerateName()
	if name != strings.ToLower(name) {
		t.Fatalf("GenerateName() = %q, want lowercase", name)
	}
	if got, err := session.NormaliseName(name); err != nil || got != name {
		t.Fatalf("generated name failed validation: got %q, err %v", got, err)
	}
}

func TestNormaliseName(t *testing.T) {
	tests := []struct {
		name    string
		want    string
		wantErr bool
	}{
		{name: "build-fix", want: "build-fix"},
		{name: "Build-Fix", want: "Build-Fix"},
		{name: "BUILD-FIX", want: "BUILD-FIX"},
		{name: " Release ", want: "Release"},
		{name: "api-2026-05", want: "api-2026-05"},
		{name: "", wantErr: true},
		{name: "bad name", wantErr: true},
		{name: "bad_name", wantErr: true},
		{name: "-bad", wantErr: true},
		{name: "bad-", wantErr: true},
		{name: "bad--name", wantErr: true},
		{name: strings.Repeat("a", session.MaxNameLength+1), wantErr: true},
	}
	for _, tt := range tests {
		got, err := session.NormaliseName(tt.name)
		if tt.wantErr {
			if err == nil {
				t.Fatalf("NormaliseName(%q) returned nil error", tt.name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("NormaliseName(%q): %v", tt.name, err)
		}
		if got != tt.want {
			t.Fatalf("NormaliseName(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestRenameUpdatesSessionFile(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	id, err := registerID(session.Info{Name: "OLD-NAME"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	defer session.Unregister(id)

	got, err := session.Rename(id, "new-name")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if got != "new-name" {
		t.Fatalf("Rename returned %q, want new-name", got)
	}
	sessions, err := session.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 1 || sessions[0].Name != "new-name" {
		t.Fatalf("session name = %#v, want new-name", sessions)
	}
}

func TestList_StaleFileCleaned(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	// Write a session file with a dead PID.
	dir, _ := session.Dir()
	_ = os.MkdirAll(dir, 0o755)
	staleContent := `{"id":"stale","pid":999999999,"language":"go","folder":"/tmp","adapter":"gopls","started_at":"` +
		time.Now().Format(time.RFC3339) + `"}`
	_ = os.WriteFile(dir+"/stale.json", []byte(staleContent), 0o644)

	sessions, err := session.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("expected stale session to be filtered, got %d session(s)", len(sessions))
	}

	// Stale file is now marked ended_at (kept for grace period) rather than
	// immediately deleted, so FindEnded can still match it across restarts.
	data, readErr := os.ReadFile(dir + "/stale.json")
	if readErr != nil {
		t.Fatalf("stale session file unexpectedly removed: %v", readErr)
	}
	if !strings.Contains(string(data), "ended_at") {
		t.Error("expected ended_at to be written to stale session file")
	}
}

func TestUnregister_MarksEndedAt(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	id, err := registerID(session.Info{Language: "go", Folder: "/tmp", Adapter: "gopls"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	session.Unregister(id)

	// File must still exist (kept for grace period).
	dir, _ := session.Dir()
	data, readErr := os.ReadFile(dir + "/" + id + ".json")
	if readErr != nil {
		t.Fatalf("session file removed immediately; want kept with ended_at: %v", readErr)
	}
	if !strings.Contains(string(data), "ended_at") {
		t.Error("expected ended_at field in session file after Unregister")
	}

	// Must not appear in active List.
	sessions, listErr := session.List()
	if listErr != nil {
		t.Fatalf("List: %v", listErr)
	}
	if len(sessions) != 0 {
		t.Fatalf("expected 0 active sessions after Unregister, got %d", len(sessions))
	}
}

func TestTouch_UpdatesLastSeenAt(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	id, err := registerID(session.Info{Language: "go", Folder: "/tmp", Adapter: "gopls"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	defer session.Unregister(id)

	time.Sleep(5 * time.Millisecond)
	before := time.Now()
	session.Touch(id)

	sessions, err := session.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].LastSeenAt.Before(before) {
		t.Errorf("LastSeenAt %v not updated by Touch (before=%v)", sessions[0].LastSeenAt, before)
	}
}

// TestTouch_ConcurrentWithWriters_NoCorruption pins the lock-free Touch
// contract: hammering Touch (mtime-only) against concurrent atomic temp+rename
// writers and a List reader must never corrupt the session file nor make List
// observe a torn file. Most valuable under -race; guards against a regression
// that makes Touch write content (which would need the writer flock back).
func TestTouch_ConcurrentWithWriters_NoCorruption(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	id, err := registerID(session.Info{Language: "go", Folder: "/tmp", Adapter: "gopls"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	defer session.Unregister(id)

	const iters = 200
	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		for range iters {
			session.Touch(id)
		}
	}()
	go func() {
		defer wg.Done()
		for i := range iters {
			if _, err := session.Rename(id, fmt.Sprintf("name-%d", i)); err != nil {
				t.Errorf("Rename during concurrent Touch: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for range iters {
			if _, err := session.List(); err != nil {
				t.Errorf("List during concurrent Touch/Rename: %v", err)
				return
			}
		}
	}()
	wg.Wait()

	sessions, err := session.List()
	if err != nil {
		t.Fatalf("List after concurrency: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 surviving session after concurrent Touch/Rename, got %d", len(sessions))
	}
}

func TestFindEnded_MatchesExternalID(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	id, err := registerID(session.Info{Language: "go", Folder: "/tmp", Adapter: "gopls", Name: "BRAVE-DEER"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	session.SetExternalID(id, "agent-abc")
	session.Unregister(id)

	// FindEnded should return the ended session.
	got := session.FindEnded("agent-abc", 24*time.Hour)
	if got == nil {
		t.Fatal("FindEnded returned nil; expected a match")
	}
	if got.Name != "BRAVE-DEER" {
		t.Errorf("Name = %q, want BRAVE-DEER", got.Name)
	}

	// Unknown external ID returns nil.
	if got2 := session.FindEnded("no-such-id", 24*time.Hour); got2 != nil {
		t.Errorf("FindEnded(unknown) = %v, want nil", got2)
	}

	// Expired grace returns nil.
	if got3 := session.FindEnded("agent-abc", 0); got3 != nil {
		t.Errorf("FindEnded(grace=0) = %v, want nil", got3)
	}
}

func TestList_SortedByStartedAt(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	// Register two sessions; their StartedAt is set by Register.
	id1, _ := registerID(session.Info{Language: "go", Folder: "/a", Adapter: "gopls"})
	time.Sleep(5 * time.Millisecond) // ensure distinct timestamps
	id2, _ := registerID(session.Info{Language: "go", Folder: "/b", Adapter: "gopls"})
	defer session.Unregister(id1)
	defer session.Unregister(id2)

	sessions, err := session.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(sessions))
	}
	if !sessions[0].StartedAt.Before(sessions[1].StartedAt) {
		t.Error("sessions not sorted by StartedAt ascending")
	}
}

func TestNormalisePurpose(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"empty is valid no-op", "", "", false},
		{"whitespace trims to empty", "   ", "", false},
		{"simple tag", "deploy-fix", "deploy-fix", false},
		{"alphanumeric and hyphen", "feature-auth-2", "feature-auth-2", false},
		{"case preserved", "Deploy-Fix", "Deploy-Fix", false},
		{"surrounding whitespace trimmed", "  deploy  ", "deploy", false},
		{"max length ok", strings.Repeat("a", 32), strings.Repeat("a", 32), false},
		{"too long rejected", strings.Repeat("a", 33), "", true},
		{"space inside rejected", "deploy fix", "", true},
		{"underscore rejected", "deploy_fix", "", true},
		{"slash rejected", "deploy/fix", "", true},
		{"non-ascii rejected", "déploy", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := session.NormalisePurpose(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("NormalisePurpose(%q) = %q, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalisePurpose(%q): unexpected error %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("NormalisePurpose(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSetPurposePersists(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	id, err := registerID(session.Info{Language: "go", Folder: "/a", Adapter: "gopls"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	defer session.Unregister(id)

	session.SetPurpose(id, "deploy-fix")

	sessions, err := session.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].Purpose != "deploy-fix" {
		t.Fatalf("Purpose = %q, want deploy-fix", sessions[0].Purpose)
	}
}

// registerID registers info and returns just the session ID — the shape these
// tests want now that Register returns the completed record so its caller can
// read back the name it was assigned.
func registerID(info session.Info) (string, error) {
	reg, err := session.Register(info)
	return reg.ID, err
}

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
