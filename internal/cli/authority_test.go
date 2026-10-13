package cli

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/account"
	"github.com/plumbkit/plumb/internal/paths"
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

// shortTempDir makes a directory under the shortest available base. Socket tests
// need it (see withShortTempRuntime): a macOS t.TempDir() path would push the
// socket past the ~104-byte sun_path ceiling and the bind would fail with EINVAL,
// which says nothing about length.
func shortTempDir(t *testing.T, prefix string) string {
	t.Helper()
	base := "/tmp"
	if info, err := os.Stat(base); err != nil || !info.IsDir() {
		base = os.TempDir()
	}
	//nolint:usetesting // t.TempDir() is exactly what cannot be used here: its macOS path exhausts the sun_path ceiling.
	dir, err := os.MkdirTemp(base, prefix)
	if err != nil {
		t.Fatalf("creating a short temp directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// accountRuntimeDirForTest is the ACCOUNT's runtime directory under the current
// seam: the second root the attach gate used to accept, and so the one a test
// asserting "outside every runtime root" has to rule out explicitly.
func accountRuntimeDirForTest(t *testing.T) string {
	t.Helper()
	u, err := account.Current()
	if err != nil {
		t.Fatalf("account.Current: %v", err)
	}
	return u.RuntimeDir()
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

	// What the record names must be a socket of ours, and a path at all: a
	// regular file, a path that is not there, and a path relative to this
	// client's working directory are all things a stale or hand-edited record
	// could name, and none of them is a place to dial.
	plain := filepath.Join(filepath.Dir(socket), "plumb.pid")
	if err := os.WriteFile(plain, []byte("1234\n"), 0o600); err != nil {
		t.Fatalf("writing %s: %v", plain, err)
	}
	notASocket := map[string]string{
		"a regular file":              plain,
		"a path that is not there":    filepath.Join(filepath.Dir(socket), "gone.sock"),
		"a path that is not absolute": filepath.Join("run", "plumb", "plumb.sock"),
	}
	for name, path := range notASocket {
		t.Run(name, func(t *testing.T) {
			record := authorityMetadata{PID: os.Getpid(), Socket: path}
			if err := os.WriteFile(authority, authorityJSON(t, record), 0o600); err != nil {
				t.Fatalf("writing the metadata: %v", err)
			}
			if got, ok := attachSocket(authority); ok {
				t.Errorf("attachSocket trusted %s: %q", name, got)
			}
		})
	}
}

// FINDING-1 from the container probe: the daemon runs under one HOME (its socket
// in <home-a>/run/plumb) and the client under another, so the socket the authority
// metadata records is under neither the client's runtime root nor the ACCOUNT's.
// The "known runtime root" gate refused it, and the client waited out its whole
// bounded wait and exited with no daemon at all. The LOCK is the trust anchor, so
// that record is dialled on the checks that carry the safety: a live pid, a unix
// socket, and this user's ownership.
func TestAttachSocket_DialsALiveSocketOutsideEveryRuntimeRoot(t *testing.T) {
	withShortTempRuntime(t) // it binds a socket: see the note on the test above
	authority := authorityLockPathForTest(t)
	if err := os.MkdirAll(filepath.Dir(authority), 0o700); err != nil {
		t.Fatalf("creating the authority directory: %v", err)
	}

	// A second HOME's runtime directory, the shape the probe's /work/home-a has:
	// the socket lives where a daemon under ANOTHER environment put it.
	socket := filepath.Join(shortTempDir(t, "plumb-home-"), "run", "plumb", "plumb.sock")
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(socket), err)
	}
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listening on %s: %v", socket, err)
	}
	defer func() { _ = ln.Close() }()

	// Guard the premise, or this test proves nothing about the gate it pins: the
	// socket's directory is neither root.
	for _, root := range []string{paths.RuntimeDir(), accountRuntimeDirForTest(t)} {
		if dir := filepath.Dir(socket); dir == root || dir == paths.Canonical(root) {
			t.Fatalf("the socket at %s is under the runtime root %s; it must be under neither", socket, root)
		}
	}

	if err := os.WriteFile(authority, authorityJSON(t, authorityMetadata{PID: os.Getpid(), Socket: socket}), 0o600); err != nil {
		t.Fatalf("writing the metadata: %v", err)
	}
	if got, ok := attachSocket(authority); !ok || got != socket {
		t.Errorf("attachSocket = %q, %v; want the recorded socket %q though it is under neither runtime root",
			got, ok, socket)
	}
	conn, ok := dialAuthoritySocket(authority)
	if !ok || conn == nil {
		t.Fatal("dialAuthoritySocket did not dial a live recorded socket outside the runtime roots")
	}
	_ = conn.Close()
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
