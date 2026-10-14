package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/daemons"
)

// Tests for the socket path as a process other than the daemon sees it
// (PLAN-529): the record the daemon publishes in the account authority's
// metadata, the state that record yields against the file at the path, and the
// doctor/`plumb debug daemons` rows and exit code built from it. The daemon-side
// hygiene these read from lives in daemon_hygiene_test.go.

// TestDaemonSocketPublication_TrustsOnlyALiveFreshRecord: the socket record is
// evidence about a running daemon only while it is both fresh and owned by a live
// pid. A stale record (a dead holder whose pid was recycled) must not make a
// deleted path read as a live daemon's fault.
func TestDaemonSocketPublication_TrustsOnlyALiveFreshRecord(t *testing.T) {
	withTempRuntime(t)
	path := authorityLockPathForTest(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("creating the authority directory: %v", err)
	}
	socket := filepath.Join(t.TempDir(), "plumb.sock")
	write := func(m authorityMetadata) {
		t.Helper()
		if err := os.WriteFile(path, authorityJSON(t, m), 0o600); err != nil {
			t.Fatalf("writing the metadata: %v", err)
		}
	}

	write(authorityMetadata{PID: os.Getpid(), Socket: socket, SocketDev: 7, SocketIno: 4242, SocketCtime: 99, Published: time.Now()})
	pub, ok := daemonSocketPublication()
	if !ok || pub.pid != os.Getpid() || pub.path != socket || pub.id.ino != 4242 || pub.id.ctime != 99 {
		t.Fatalf("a fresh record of a live holder must be trusted, change time included: %+v (ok=%v)", pub, ok)
	}

	// A record written before the change time was published carries none. It is
	// still a record of a live holder, with the weaker (device, inode) identity:
	// the reader must not treat "no change time" as a mismatch, or every healthy
	// daemon an older binary started would be reported as a replaced socket.
	write(authorityMetadata{PID: os.Getpid(), Socket: socket, SocketDev: 7, SocketIno: 4242, Published: time.Now()})
	if older, ok := daemonSocketPublication(); !ok || older.id.ctime != 0 {
		t.Fatalf("a record without a change time must still be trusted, with ctime 0: %+v (ok=%v)", older, ok)
	}

	stale := authorityMetadata{PID: os.Getpid(), Socket: socket, Published: time.Now().Add(-2 * daemonSocketPublicationFreshness)}
	write(stale)
	if _, ok := daemonSocketPublication(); ok {
		t.Error("a record nobody has refreshed was trusted as a live holder's")
	}

	write(authorityMetadata{PID: 1 << 30, Socket: socket, Published: time.Now()})
	if _, ok := daemonSocketPublication(); ok {
		t.Error("a dead holder's record was trusted")
	}

	write(authorityMetadata{PID: os.Getpid(), Published: time.Now()})
	if _, ok := daemonSocketPublication(); ok {
		t.Error("a record naming no socket was trusted")
	}
}

// TestDaemonSocketStates_ReportsAReplacementWhileTheRecordIsFresh pins the pair
// the freshness window exists for, from the reader's side.
//
// The daemon keeps the record fresh on every hygiene check, replacement or not
// (TestSocketGuard_KeepsPublishingWhileSomebodyElsesFileIsAtThePath), so a
// replaced socket is reported for as long as the daemon is alive. The second half
// shows why that refresh is load-bearing: the same path with a record nobody has
// touched for longer than the window loses its publisher, so there is no fault
// left to report and `plumb debug daemons` exits 0 — which is exactly the hole a
// daemon that only refreshed on the healthy branch left open.
func TestDaemonSocketStates_ReportsAReplacementWhileTheRecordIsFresh(t *testing.T) {
	withTempRuntime(t)
	path := filepath.Join(t.TempDir(), "plumb.sock")
	if err := os.WriteFile(path, []byte("somebody else's file"), 0o600); err != nil {
		t.Fatal(err)
	}
	report := daemons.Report{Sockets: []daemons.SocketReading{{Path: path, Answered: true}}}

	authPath := authorityLockPathForTest(t)
	if err := os.MkdirAll(filepath.Dir(authPath), 0o700); err != nil {
		t.Fatal(err)
	}
	// A socket identity that is NOT the file at the path: this daemon bound its
	// socket, and somebody replaced the path afterwards.
	published := authorityMetadata{
		PID: os.Getpid(), Socket: path, SocketDev: 1, SocketIno: 2, SocketCtime: 3,
		Published: time.Now(),
	}
	writeRecord := func(m authorityMetadata) {
		t.Helper()
		if err := os.WriteFile(authPath, authorityJSON(t, m), 0o600); err != nil {
			t.Fatalf("writing the authority record: %v", err)
		}
	}

	writeRecord(published)
	states := daemonSocketStates(report)
	if len(states) != 1 || states[0].fault() == "" {
		t.Fatalf("a fresh record of a live holder over a replaced path must be a fault, got %+v", states)
	}
	if err := debugDaemonsExit(daemons.Verdict{State: daemons.StateOK}, states); err == nil {
		t.Error("plumb debug daemons exited 0 for a replaced socket whose holder is publishing")
	}

	// The control: the same everything, with a record nobody has refreshed inside
	// the freshness window. This is the state the daemon must never be in — it is
	// what S1 found — and the reader cannot turn it into a fault without also
	// accusing every daemon whose pid was recycled.
	stale := published
	stale.Published = time.Now().Add(-2 * daemonSocketPublicationFreshness)
	writeRecord(stale)
	states = daemonSocketStates(report)
	if states[0].fault() != "" {
		t.Errorf("a stale record must not be trusted as a live holder's: %+v", states[0])
	}
	if states[0].Publisher != 0 {
		t.Errorf("a stale record published a pid: %+v", states[0])
	}
	if err := debugDaemonsExit(daemons.Verdict{State: daemons.StateOK}, states); err != nil {
		t.Errorf("the reader is expected to go quiet here (which is why the daemon must keep publishing), got %v", err)
	}
}

// TestSocketPathStateFor_ComparesThePathWithThePublication is the mapping doctor
// and `plumb debug daemons` render from: a path that still holds the published
// file is healthy, a replaced one and a missing one are faults.
func TestSocketPathStateFor_ComparesThePathWithThePublication(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plumb.sock")
	if err := os.WriteFile(path, []byte("the socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	pub := socketPublication{pid: 4242, path: path, id: identityOf(info)}

	healthy := socketPathStateFor(path, true, []int{4242}, pub, true)
	if !healthy.Exists || !healthy.IdentityMatches {
		t.Fatalf("the published file is at the path, so the state must match: %+v", healthy)
	}
	if healthy.fault() != "" || healthy.note() != "" {
		t.Errorf("a healthy path must say nothing: fault=%q note=%q", healthy.fault(), healthy.note())
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("somebody else's file"), 0o600); err != nil {
		t.Fatal(err)
	}
	replacement, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if identityOf(replacement).ino == pub.id.ino {
		t.Logf("the filesystem reused inode %d; the change time is what distinguishes the replacement", pub.id.ino)
	}
	replaced := socketPathStateFor(path, true, nil, pub, true)
	if !replaced.IdentityKnown || replaced.IdentityMatches {
		t.Fatalf("the path no longer holds the published file: published=%+v replacement=%+v state=%+v", pub.id, identityOf(replacement), replaced)
	}
	// The same inode with a different change time is a replacement too — the ext4
	// case, made deterministic here rather than left to the allocator.
	forged := pub
	forged.id.ctime++
	if st := socketPathStateFor(path, true, nil, forged, true); st.IdentityMatches {
		t.Errorf("a file whose change time differs from the published one was accepted: published=%+v state=%+v", forged.id, st)
	}
	if !strings.Contains(replaced.fault(), "replaced") {
		t.Errorf("a replaced socket must be a fault, got %q", replaced.fault())
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	gone := socketPathStateFor(path, true, nil, pub, true)
	if !strings.Contains(gone.fault(), "gone while plumb daemon 4242") {
		t.Errorf("a missing socket of a live daemon must be a fault, got %q", gone.fault())
	}

	// With no publication there is nothing to compare, so nothing is claimed —
	// even for a path that is present and owned by nobody.
	unpublished := socketPathStateFor(path, true, nil, socketPublication{}, false)
	if unpublished.fault() != "" {
		t.Errorf("a path no live daemon published must not be a fault, got %q", unpublished.fault())
	}

	// A publication names ONE path: another path must not inherit its publisher,
	// or a healthy account would be accused of a socket it never had.
	other := filepath.Join(dir, "plumb.ctrl.sock")
	unrelated := socketPathStateFor(other, true, nil, pub, true)
	if unrelated.Publisher != 0 || unrelated.fault() != "" {
		t.Errorf("the publication for %s made %s read as a live daemon's socket: %+v", path, other, unrelated)
	}
}

// TestDebugDaemonsExit_SocketFaultIsNotSuccess: exactly one daemon holding the
// authority, whose socket path has been replaced under it, is one daemon no client
// can reach — a script reading the exit status must not read that as success.
func TestDebugDaemonsExit_SocketFaultIsNotSuccess(t *testing.T) {
	// An intact socket: the file is the published one in the strongest sense.
	healthy := []socketPathState{{
		Path: "/run/plumb/plumb.sock", Exists: true, Publisher: 7,
		IdentityKnown: true, InodeMatches: true, IdentityMatches: true,
	}}
	if err := debugDaemonsExit(daemons.Verdict{State: daemons.StateOK}, healthy); err != nil {
		t.Errorf("a clean account with an intact socket must be success, got %v", err)
	}
	// A metadata change (same inode, moved change time) is a note: the daemon is
	// serving, and accusing it would have a user restart a working daemon.
	metadataMoved := []socketPathState{{
		Path: "/run/plumb/plumb.sock", Exists: true, Publisher: 7,
		IdentityKnown: true, InodeMatches: true,
	}}
	if err := debugDaemonsExit(daemons.Verdict{State: daemons.StateOK}, metadataMoved); err != nil {
		t.Errorf("a moved change time must not fail the command, got %v", err)
	}
	// A note alone (a socket file nothing serves) is information, not a fault.
	noted := []socketPathState{{Path: "/run/plumb/plumb.sock", Exists: true, Answered: true}}
	if err := debugDaemonsExit(daemons.Verdict{State: daemons.StateOK}, noted); err != nil {
		t.Errorf("a path with no live publisher must not fail the command, got %v", err)
	}
	// Gone, and replaced (a different inode): both are the daemon being
	// unreachable, and both must exit non-zero.
	gone := []socketPathState{{Path: "/run/plumb/plumb.sock", Publisher: 7, IdentityKnown: true}}
	if err := debugDaemonsExit(daemons.Verdict{State: daemons.StateOK}, gone); err == nil {
		t.Error("a live daemon whose socket path is gone must exit non-zero")
	}
	replaced := []socketPathState{{
		Path: "/run/plumb/plumb.sock", Exists: true, Publisher: 7,
		IdentityKnown: true, InodeMatches: false,
	}}
	if err := debugDaemonsExit(daemons.Verdict{State: daemons.StateOK}, replaced); err == nil {
		t.Error("a replaced socket must exit non-zero")
	}
}

// TestMetadataChangeIsNotAFaultAndAReplacementIs pins the reader's discrimination
// from both sides, and the exit code each one earns: a moved change time on the
// publisher's own inode is a note (doctor warns, `plumb debug daemons` exits 0),
// while a different inode is a replacement (a failed row and a non-zero exit).
//
// This pair is the reviewer's finding, both ways round: the first version keyed
// the fault on the change time, so an unrelated chmod on the daemon's own socket
// made a healthy, serving daemon read as unreachable.
func TestMetadataChangeIsNotAFaultAndAReplacementIs(t *testing.T) {
	withTempRuntime(t)
	path := filepath.Join(t.TempDir(), "plumb.sock")
	if err := os.WriteFile(path, []byte("the socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	authPath := authorityLockPathForTest(t)
	if err := os.MkdirAll(filepath.Dir(authPath), 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(id fileIdentity) {
		t.Helper()
		m := authorityMetadata{
			PID: os.Getpid(), Socket: path, SocketDev: id.dev, SocketIno: id.ino,
			SocketCtime: id.ctime, Published: time.Now(),
		}
		if err := os.WriteFile(authPath, authorityJSON(t, m), 0o600); err != nil {
			t.Fatalf("writing the authority record: %v", err)
		}
	}
	// The realistic Direction-1 shape: the census answered AND the publishing
	// daemon (this process, for the record's pid) is the one holding the path open
	// — a live daemon whose socket's metadata moved.
	report := daemons.Report{Sockets: []daemons.SocketReading{
		{Path: path, Answered: true, Owners: []int{os.Getpid()}},
	}}
	okVerdict := daemons.Verdict{State: daemons.StateOK}

	// Direction 1: the metadata moved on the publisher's own inode.
	moved := identityOf(info)
	moved.ctime--
	write(moved)
	states := daemonSocketStates(report)
	if len(states) != 1 || !states[0].MetadataChanged() {
		t.Fatalf("a moved change time must read as a metadata change, got %+v", states)
	}
	if states[0].fault() != "" {
		t.Errorf("a metadata change is not a fault: %q", states[0].fault())
	}
	if err := debugDaemonsExit(okVerdict, states); err != nil {
		t.Error("plumb debug daemons must not fail for a healthy daemon whose socket's metadata moved")
	}
	if rows := daemonSocketResults(states); len(rows) != 1 || !rows[0].ok || !rows[0].warn {
		t.Errorf("doctor must warn, not fail, for a metadata change: %+v", rows)
	}

	// The liveness cross-check, three ways, all with the same device and inode
	// and a moved change time:
	//  - the census did not answer, so nothing is assumed: a note;
	//  - the census answered and the holder has the path open: the change time
	//    moved on the holder's own socket, a note;
	//  - the census answered and the holder does NOT have it open: the inode was
	//    recycled after that daemon's socket went away, which is a replacement.
	movedStates := func(answered bool, owners []int) []socketPathState {
		return daemonSocketStates(daemons.Report{Sockets: []daemons.SocketReading{
			{Path: path, Answered: answered, Owners: owners},
		}})
	}
	write(moved) // the record names the same inode with the older change time
	unanswered := movedStates(false, nil)
	if unanswered[0].fault() != "" || !unanswered[0].MetadataChanged() {
		t.Errorf("an unanswered ownership question must leave a metadata change as a note: %+v", unanswered[0])
	}
	if err := debugDaemonsExit(okVerdict, unanswered); err != nil {
		t.Errorf("an unanswered ownership question must not fail the command: %v", err)
	}
	owned := movedStates(true, []int{os.Getpid()})
	if owned[0].fault() != "" || !owned[0].MetadataChanged() {
		t.Errorf("the holder still holding the path open must leave it a note: %+v", owned[0])
	}
	disowned := movedStates(true, []int{4242})
	if disowned[0].fault() == "" {
		t.Errorf("a holder that no longer has the path open must be a fault: %+v", disowned[0])
	}
	if err := debugDaemonsExit(okVerdict, disowned); err == nil {
		t.Error("a recycled inode under a live record must exit non-zero")
	}

	// Direction 2: a different inode is somebody else's socket. The inode is
	// forged so this does not depend on the allocator; the real replacement case
	// is TestSocketGuard_NeverRemovesAReplacementAndSaysSo, and
	// TestDaemonSocketStates_ReportsAReplacementWhileTheRecordIsFresh above.
	forged := identityOf(info)
	forged.ino++
	write(forged)
	states = daemonSocketStates(report)
	if len(states) != 1 || states[0].fault() == "" {
		t.Fatalf("a different inode must be a fault, got %+v", states)
	}
	if err := debugDaemonsExit(okVerdict, states); err == nil {
		t.Error("plumb debug daemons must exit non-zero for a replaced socket")
	}
	if rows := daemonSocketResults(states); len(rows) != 1 || rows[0].ok {
		t.Errorf("doctor must fail for a replaced socket: %+v", rows)
	}
}

// TestDaemonSocketResults is doctor's half: the same state becomes a failed row
// with a remedy, a warning, or nothing at all.
func TestDaemonSocketResults(t *testing.T) {
	faulted := daemonSocketResults([]socketPathState{{Path: "/run/plumb/plumb.sock", Publisher: 7, IdentityKnown: true}})
	if len(faulted) != 1 || faulted[0].ok || faulted[0].fix == "" {
		t.Fatalf("a socket fault must be one failed row with a fix, got %+v", faulted)
	}
	if !strings.Contains(faulted[0].detail, "gone while plumb daemon 7") {
		t.Errorf("the row must say what is wrong, got %q", faulted[0].detail)
	}

	noted := daemonSocketResults([]socketPathState{{Path: "/run/plumb/plumb.sock", Exists: true, Answered: true}})
	if len(noted) != 1 || !noted[0].ok || !noted[0].warn {
		t.Fatalf("a socket file nothing serves must be one warning row, got %+v", noted)
	}

	healthy := daemonSocketResults([]socketPathState{{
		Path: "/run/plumb/plumb.sock", Exists: true, Publisher: 7,
		IdentityKnown: true, InodeMatches: true, IdentityMatches: true,
	}})
	if len(healthy) != 0 {
		t.Errorf("a healthy socket must add no rows, got %+v", healthy)
	}

	// A metadata change is a WARNING row, never a failure with the "move it
	// aside and restart" remedy: the daemon is alive and serving.
	moved := daemonSocketResults([]socketPathState{{
		Path: "/run/plumb/plumb.sock", Exists: true, Publisher: 7,
		IdentityKnown: true, InodeMatches: true,
	}})
	if len(moved) != 1 || !moved[0].ok || !moved[0].warn {
		t.Fatalf("a moved change time must be one warning row, got %+v", moved)
	}
	if moved[0].fix != "" {
		t.Errorf("a warning must not carry a fix that restarts a healthy daemon, got %q", moved[0].fix)
	}
}
