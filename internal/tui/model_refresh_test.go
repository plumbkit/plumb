package tui

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/memory"
	"github.com/plumbkit/plumb/internal/session"
)

// fakeCtrlSocket starts a unix-socket listener that answers exactly one
// connection with resp, after reading (and discarding) the request line, then
// closes — mirroring the daemon's line-based control protocol closely enough
// for refreshDiagnostics's dial/write/ReadAll round trip. Reading the request
// before writing the response guarantees the accept goroutine never closes the
// connection ahead of the client's write, regardless of scheduling. accepted
// is closed once the connection has been served, so tests can synchronise on
// it instead of sleeping.
func fakeCtrlSocket(t *testing.T, resp string) (sockPath string, accepted <-chan struct{}) {
	t.Helper()
	// Under `make verify` TMPDIR points into the repo's .testcache, which
	// pushes t.TempDir()-based socket paths past the unix sun_path limit
	// (~104-108 bytes) and fails Listen with "bind: invalid argument" — so
	// the socket lives in a short os.MkdirTemp("/tmp", ...) dir instead
	// (same reasoning as cmd/smoke's isolated-HOME socket).
	dir, err := os.MkdirTemp("/tmp", "plumbtui")
	if err != nil {
		t.Fatalf("os.MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sockPath = filepath.Join(dir, "ctrl.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = bufio.NewReader(conn).ReadString('\n')
		_, _ = conn.Write([]byte(resp))
	}()
	return sockPath, done
}

// TestRefreshDiagnostics_SkippedWhenSectionHidden proves the gate short-circuits
// before the blocking unix-socket dial: with a control socket listening but the
// Sessions section (currentSection == 1) not active, no connection is accepted
// and lastDiagnosticsOutput stays empty.
func TestRefreshDiagnostics_SkippedWhenSectionHidden(t *testing.T) {
	sockPath, accepted := fakeCtrlSocket(t, "0 issues\n")
	m := &Model{
		currentSection: 0, // Dashboard, not Sessions
		ctrlPath:       sockPath,
		sessions:       []session.Info{{Folder: "/some/workspace"}},
	}

	m.refreshDiagnostics()

	select {
	case <-accepted:
		t.Fatal("refreshDiagnostics dialled the control socket while the Sessions section was hidden")
	case <-time.After(100 * time.Millisecond):
		// No connection within the window — the gate fired before any I/O.
	}
	if m.lastDiagnosticsOutput != "" {
		t.Errorf("lastDiagnosticsOutput = %q, want empty (no fetch attempted)", m.lastDiagnosticsOutput)
	}
}

// TestRefreshDiagnostics_FetchesWhenSectionVisible proves the existing round
// trip is unchanged when the Sessions section IS active: the gate must not
// regress the visible-section behaviour.
func TestRefreshDiagnostics_FetchesWhenSectionVisible(t *testing.T) {
	sockPath, accepted := fakeCtrlSocket(t, "3 issues\n")
	m := &Model{
		currentSection: 1, // Sessions
		ctrlPath:       sockPath,
		sessions:       []session.Info{{Folder: "/some/workspace"}},
	}

	m.refreshDiagnostics()
	<-accepted // wait for the fake server to finish serving the connection

	if want := "3 issues\n"; m.lastDiagnosticsOutput != want {
		t.Errorf("lastDiagnosticsOutput = %q, want %q", m.lastDiagnosticsOutput, want)
	}
}

// TestRefreshMemories_SkippedWhenSectionHidden proves the gate short-circuits
// before the directory walk: with a real memory file on disk but the Memory
// section (currentSection == 2) not active, neither memoryWorkspaces nor
// memories is populated.
func TestRefreshMemories_SkippedWhenSectionHidden(t *testing.T) {
	ws := t.TempDir()
	if err := memory.Write(ws, "note", "body", "desc"); err != nil {
		t.Fatal(err)
	}
	m := &Model{
		currentSection: 0, // Dashboard, not Memory
		sessions:       []session.Info{{Folder: ws}},
	}

	m.refreshMemories()

	if m.memoryWorkspaces != nil {
		t.Errorf("memoryWorkspaces = %+v, want nil (no walk attempted)", m.memoryWorkspaces)
	}
	if m.memories != nil {
		t.Errorf("memories = %+v, want nil (no walk attempted)", m.memories)
	}
}

// TestRefreshMemories_FetchesWhenSectionVisible proves the existing behaviour
// is unchanged when the Memory section IS active — mirrors
// TestMemoryWorkspaceSwitchReloadsAndInvalidates but pins the gate condition.
func TestRefreshMemories_FetchesWhenSectionVisible(t *testing.T) {
	ws := t.TempDir()
	if err := memory.Write(ws, "note", "body", "desc"); err != nil {
		t.Fatal(err)
	}
	m := &Model{
		currentSection: 2, // Memory
		sessions:       []session.Info{{Folder: ws}},
	}

	m.refreshMemories()

	if len(m.memoryWorkspaces) != 1 {
		t.Fatalf("memoryWorkspaces = %+v, want 1 entry", m.memoryWorkspaces)
	}
	if len(m.memories) != 1 || m.memories[0].Name != "note" {
		t.Fatalf("memories = %+v, want [note]", m.memories)
	}
}

// TestSelectSection_SwitchingToSessionsRefreshesDiagnosticsImmediately proves
// selectSection does not leave the Diagnostics tab stale-on-switch: because
// refreshDiagnostics is now gated on currentSection == 1, entering the section
// must trigger an immediate fetch rather than waiting for the next 2s poll.
func TestSelectSection_SwitchingToSessionsRefreshesDiagnosticsImmediately(t *testing.T) {
	sockPath, accepted := fakeCtrlSocket(t, "1 issue\n")
	m := &Model{
		currentSection: 0, // Dashboard
		ctrlPath:       sockPath,
		sessions:       []session.Info{{Folder: "/some/workspace"}},
	}

	m.selectSection(1) // Sessions
	<-accepted

	if want := "1 issue\n"; m.lastDiagnosticsOutput != want {
		t.Errorf("lastDiagnosticsOutput = %q, want %q (immediate refresh on switch-in)", m.lastDiagnosticsOutput, want)
	}
}

// TestSelectSection_SwitchingToMemoryRefreshesImmediately is the Memory-section
// analogue of the Diagnostics switch-in test above.
func TestSelectSection_SwitchingToMemoryRefreshesImmediately(t *testing.T) {
	ws := t.TempDir()
	if err := memory.Write(ws, "note", "body", "desc"); err != nil {
		t.Fatal(err)
	}
	m := &Model{
		currentSection: 0, // Dashboard
		sessions:       []session.Info{{Folder: ws}},
	}

	m.selectSection(2) // Memory

	if len(m.memories) != 1 || m.memories[0].Name != "note" {
		t.Fatalf("memories = %+v, want [note] populated immediately on switch-in", m.memories)
	}
}

// TestSelectSection_NoOpTransitionDoesNotRefetch guards the prev != idx
// transition check: re-selecting the already-active Sessions section must not
// re-dial the control socket a second time. Mirrors the existing prev != 2 /
// prev != 4 transition guards already present in selectSection.
func TestSelectSection_NoOpTransitionDoesNotRefetch(t *testing.T) {
	sockPath, accepted := fakeCtrlSocket(t, "0 issues\n")
	m := &Model{
		currentSection: 1, // already Sessions
		ctrlPath:       sockPath,
		sessions:       []session.Info{{Folder: "/some/workspace"}},
	}

	m.selectSection(1) // no-op transition: prev == idx == 1

	select {
	case <-accepted:
		t.Fatal("selectSection re-fetched diagnostics on a no-op transition into the already-active section")
	case <-time.After(100 * time.Millisecond):
		// No connection within the window — no refetch happened, as expected.
	}
}
