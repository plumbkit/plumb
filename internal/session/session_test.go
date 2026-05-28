package session_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golimpio/plumb/internal/session"
)

// TestWriteSessionFileAtomic_NoTornReads guards that session-file writes are
// atomic: a concurrent reader (in production, the TUI refresh racing the daemon
// reaper across processes) must never observe a partially-written file, and no
// temp file may be left behind. Before the temp-file+rename change, Patch used a
// plain os.WriteFile and a reader could catch a truncated file mid-write — a
// real hazard now that List has write side effects and is called from the TUI.
func TestWriteSessionFileAtomic_NoTornReads(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	id, err := session.Register(session.Info{Folder: "/tmp/x", Adapter: "gopls"})
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
	for i := 0; i < 50; i++ {
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
	id, err := session.Register(session.Info{Folder: "/tmp/x", Adapter: "gopls"})
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

	id, err := session.Register(session.Info{
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

	id, err := session.Register(session.Info{Name: "OLD-NAME"})
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

	id, err := session.Register(session.Info{Language: "go", Folder: "/tmp", Adapter: "gopls"})
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

	id, err := session.Register(session.Info{Language: "go", Folder: "/tmp", Adapter: "gopls"})
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

func TestFindEnded_MatchesExternalID(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	id, err := session.Register(session.Info{Language: "go", Folder: "/tmp", Adapter: "gopls", Name: "BRAVE-DEER"})
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
	id1, _ := session.Register(session.Info{Language: "go", Folder: "/a", Adapter: "gopls"})
	time.Sleep(5 * time.Millisecond) // ensure distinct timestamps
	id2, _ := session.Register(session.Info{Language: "go", Folder: "/b", Adapter: "gopls"})
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
