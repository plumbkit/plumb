package cli

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// authorityLockPathForTest is where acquireAuthority would put the lock under
// the current (seam-redirected) account, so a test can read it back.
func authorityLockPathForTest(t *testing.T) string {
	t.Helper()
	path, err := authorityPath()
	if err != nil {
		t.Fatalf("authorityPath: %v", err)
	}
	return path
}

func TestAcquireAuthority_SecondAttemptFails(t *testing.T) {
	withTempRuntime(t)
	first, err := acquireAuthority()
	if err != nil {
		t.Fatalf("first acquireAuthority: %v", err)
	}
	defer func() { _ = first.Close() }()

	if _, err := acquireAuthority(); err == nil {
		t.Fatal("a second acquireAuthority succeeded while the first was held")
	} else if !errors.Is(err, errDaemonAlreadyRunning) {
		t.Fatalf("second acquireAuthority = %v, want errDaemonAlreadyRunning", err)
	}

	// Closing the holder releases it, which is what makes crash recovery work.
	if err := first.Close(); err != nil {
		t.Fatalf("closing the authority: %v", err)
	}
	again, err := acquireAuthority()
	if err != nil {
		t.Fatalf("acquireAuthority after release: %v", err)
	}
	_ = again.Close()
}

func TestPublishAuthority_RoundTripsThroughTheHeldFD(t *testing.T) {
	withTempRuntime(t)
	f, err := acquireAuthority()
	if err != nil {
		t.Fatalf("acquireAuthority: %v", err)
	}
	defer func() { _ = f.Close() }()

	want := authorityMetadata{
		PID:        os.Getpid(),
		Version:    "1.2.3",
		Executable: "/opt/plumb/bin/plumb",
		Socket:     daemonSocketPath(),
	}
	if err := publishAuthority(f, want); err != nil {
		t.Fatalf("publishAuthority: %v", err)
	}
	got, ok := readAuthority(authorityLockPathForTest(t))
	if !ok {
		t.Fatal("readAuthority found nothing after publishing")
	}
	if got.PID != want.PID || got.Version != want.Version || got.Executable != want.Executable || got.Socket != want.Socket {
		t.Errorf("read back %+v, want %+v", got, want)
	}
	if got.Published.IsZero() {
		t.Error("Published must be filled in when the caller does not set it")
	}

	// Publishing again must overwrite, not append: a reader parses one record.
	want.Version = "2.0.0"
	if err := publishAuthority(f, want); err != nil {
		t.Fatalf("second publishAuthority: %v", err)
	}
	got, ok = readAuthority(authorityLockPathForTest(t))
	if !ok || got.Version != "2.0.0" {
		t.Errorf("after a second publish: %+v (ok=%v), want version 2.0.0", got, ok)
	}
}

func TestReadAuthority_RefusesRubbish(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "authority.lock")

	if _, ok := readAuthority(path); ok {
		t.Error("a missing file must not read as a holder")
	}
	for _, body := range []string{"", "not json", `{"pid":0}`, `{"pid":-5}`, `{`} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("writing %q: %v", body, err)
		}
		if _, ok := readAuthority(path); ok {
			t.Errorf("body %q must not read as a holder", body)
		}
	}
}

func TestAttachSocket_TrustsOnlyALiveHolderAtAPlumbSocket(t *testing.T) {
	// withShortTempRuntime, not withTempRuntime: this test BINDS a socket, and a
	// macOS t.TempDir() path exhausts the ~104-byte sun_path ceiling (bind fails
	// with EINVAL, which says nothing about length).
	withShortTempRuntime(t)
	authority := authorityLockPathForTest(t)
	if err := os.MkdirAll(filepath.Dir(authority), 0o700); err != nil {
		t.Fatalf("creating the authority directory: %v", err)
	}

	socket := daemonSocketPath()
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		t.Fatalf("creating the runtime directory: %v", err)
	}
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listening on %s: %v", socket, err)
	}
	defer func() { _ = ln.Close() }()

	live := authorityMetadata{PID: os.Getpid(), Socket: socket}
	if err := os.WriteFile(authority, authorityJSON(t, live), 0o600); err != nil {
		t.Fatalf("writing the metadata: %v", err)
	}
	if got, ok := attachSocket(authority); !ok || got != socket {
		t.Errorf("attachSocket = %q, %v; want the live socket %q", got, ok, socket)
	}

	// A dead holder is not an attach target: the record outlives the process
	// that wrote it, and dialling its socket would just fail slowly.
	dead := authorityMetadata{PID: 1 << 30, Socket: socket}
	if err := os.WriteFile(authority, authorityJSON(t, dead), 0o600); err != nil {
		t.Fatalf("writing the metadata: %v", err)
	}
	if got, ok := attachSocket(authority); ok {
		t.Errorf("attachSocket trusted a dead holder: %q", got)
	}

	// A socket outside every runtime root plumb could have chosen is refused,
	// however live it looks: metadata says where a daemon claimed to be.
	// Short enough to bind, and in a subdirectory that is NOT a runtime root:
	// the point is that a live socket elsewhere is still refused.
	elsewhereDir := filepath.Join(filepath.Dir(daemonSocketPath()), "elsewhere")
	if err := os.MkdirAll(elsewhereDir, 0o700); err != nil {
		t.Fatalf("creating %s: %v", elsewhereDir, err)
	}
	elsewhere := filepath.Join(elsewhereDir, "plumb.sock")
	other, err := net.Listen("unix", elsewhere)
	if err != nil {
		t.Fatalf("listening on %s: %v", elsewhere, err)
	}
	defer func() { _ = other.Close() }()
	stray := authorityMetadata{PID: os.Getpid(), Socket: elsewhere}
	if err := os.WriteFile(authority, authorityJSON(t, stray), 0o600); err != nil {
		t.Fatalf("writing the metadata: %v", err)
	}
	if got, ok := attachSocket(authority); ok {
		t.Errorf("attachSocket trusted a socket outside the runtime roots: %q", got)
	}
}

func TestDialAuthoritySocket_DialsTheRecordedSocket(t *testing.T) {
	withShortTempRuntime(t) // it binds a socket: see the note above
	authority := authorityLockPathForTest(t)
	if err := os.MkdirAll(filepath.Dir(authority), 0o700); err != nil {
		t.Fatalf("creating the authority directory: %v", err)
	}
	socket := daemonSocketPath()
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		t.Fatalf("creating the runtime directory: %v", err)
	}
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	defer func() { _ = ln.Close() }()
	accepted := make(chan struct{}, 1)
	go func() {
		if conn, err := ln.Accept(); err == nil {
			accepted <- struct{}{}
			_ = conn.Close()
		}
	}()

	if err := os.WriteFile(authority, authorityJSON(t, authorityMetadata{PID: os.Getpid(), Socket: socket}), 0o600); err != nil {
		t.Fatalf("writing the metadata: %v", err)
	}
	conn, ok := dialAuthoritySocket(authority)
	if !ok {
		t.Fatal("dialAuthoritySocket did not dial a recorded live socket")
	}
	_ = conn.Close()
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Error("the recorded socket was dialled but never accepted a connection")
	}

	// No metadata at all is not an error, just nothing to attach to.
	if err := os.Remove(authority); err != nil {
		t.Fatalf("removing the metadata: %v", err)
	}
	if conn, ok := dialAuthoritySocket(authority); ok {
		_ = conn.Close()
		t.Error("dialAuthoritySocket dialled with no metadata at all")
	}
}

// authorityJSON encodes metadata for a test fixture. It is named for its
// subject because this package already has a mustJSON for resume entries.
func authorityJSON(t *testing.T, m authorityMetadata) []byte {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("encoding metadata: %v", err)
	}
	return data
}
