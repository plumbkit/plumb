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
	// The replacement is classified, not quoted: on ext4 the file may carry the
	// reused inode, so the state is a fault either way — and the assertion is
	// "this is a fault", never "this message says the word replaced".
	replaced := socketPathStateFor(path, true, nil, pub, true)
	if !replaced.IdentityKnown || replaced.IdentityMatches {
		t.Fatalf("the path no longer holds the published file: published=%+v replacement=%+v state=%+v", pub.id, identityOf(replacement), replaced)
	}
	if replaced.fault() == "" {
		t.Errorf("a replaced socket must be a fault: state=%+v", replaced)
	}
	if replaced.note() != "" {
		t.Errorf("a fault must not also be filed as a note: state=%+v", replaced)
	}
	if err := debugDaemonsExit(daemons.Verdict{State: daemons.StateOK}, []socketPathState{replaced}); err == nil {
		t.Error("a replaced socket must exit non-zero")
	}
	// The one wording requirement, kept loose on purpose: the fault names the
	// daemon it is about, so a reader knows which process to look at.
	if !strings.Contains(replaced.fault(), "4242") {
		t.Errorf("the fault must name the publishing daemon, got %q", replaced.fault())
	}
	// A forged change time on the same identity: this asserts only what the
	// FORGED pair can settle (the file is not accepted as identical). The full
	// classification of the reused-inode case — fault, not note, non-zero exit —
	// is pinned by TestSocketPathState_ThreeWayClassification, which constructs
	// the states instead of growing them, and therefore does not depend on the
	// allocator or on which file happens to be at the path.
	forged := pub
	forged.id.ctime++
	if st := socketPathStateFor(path, true, nil, forged, true); st.IdentityMatches {
		t.Errorf("a file whose change time differs from the published one was accepted: published=%+v state=%+v", forged.id, st)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	gone := socketPathStateFor(path, true, nil, pub, true)
	if gone.fault() == "" {
		t.Errorf("a missing socket of a live daemon must be a fault: %+v", gone)
	}
	if !strings.Contains(gone.fault(), "4242") {
		t.Errorf("the fault must name the publishing daemon, got %q", gone.fault())
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

// TestSocketPathState_ThreeWayClassification is the tri-state, written to the
// CLASSIFICATION rather than to any message: for each of the three cases the
// reader can see, which of fault/note is set, whether the command exits non-zero,
// and what shape of doctor row comes out.
//
// The three cases, and why each is what it is:
//
//  1. device, inode AND change time match — the daemon's own file, untouched.
//  2. device and inode match, the change time moved, and the census found the
//     publishing daemon still holding the path open — a chmod or a backup tool on
//     the daemon's own socket. SOFT: the mirror case this code exists for, and the
//     one that must never fail a healthy daemon.
//  3. device and inode match, the change time moved, and the daemon does NOT hold
//     the path open — the inode was handed back out after that socket went away.
//     HARD: this is a genuine replacement wearing the old numbers, and it is
//     exactly what the change time was added to catch (a different inode is the
//     other form of the same case).
//
// The one case that is deliberately soft is 2 without an ownership answer: the
// census could not tell, and an absence of evidence is never turned into an
// accusation. That residual is stated here so it is not mistaken for case 3.
func TestSocketPathState_ThreeWayClassification(t *testing.T) {
	const pid = 4242
	tests := []struct {
		name            string
		state           socketPathState
		wantFault       bool
		wantNote        bool
		wantMetadata    bool
		wantExitFailure bool
		wantDoctor      string // "", "warn" or "fail"
	}{
		{
			name: "1. the daemon's own socket, metadata untouched",
			state: socketPathState{
				Path: "/p/plumb.sock", Exists: true, Publisher: pid,
				IdentityKnown: true, InodeMatches: true, IdentityMatches: true,
				Answered: true, OwnedBy: []int{pid},
			},
		},
		{
			name: "2. metadata moved, the holder still holds the socket open",
			state: socketPathState{
				Path: "/p/plumb.sock", Exists: true, Publisher: pid,
				IdentityKnown: true, InodeMatches: true, IdentityMatches: false,
				Answered: true, OwnedBy: []int{pid},
			},
			wantNote: true, wantMetadata: true, wantDoctor: "warn",
		},
		{
			name: "3a. inode reused: the holder no longer holds the path open",
			state: socketPathState{
				Path: "/p/plumb.sock", Exists: true, Publisher: pid,
				IdentityKnown: true, InodeMatches: true, IdentityMatches: false,
				Answered: true, OwnedBy: []int{999},
			},
			wantFault: true, wantMetadata: true, wantExitFailure: true, wantDoctor: "fail",
		},
		{
			name: "3b. a different inode is somebody else's socket",
			state: socketPathState{
				Path: "/p/plumb.sock", Exists: true, Publisher: pid,
				IdentityKnown: true, InodeMatches: false, IdentityMatches: false,
				Answered: true, OwnedBy: []int{999},
			},
			wantFault: true, wantExitFailure: true, wantDoctor: "fail",
		},
		{
			name: "3c. inode reused, ownership unanswered: reported, not assumed",
			state: socketPathState{
				Path: "/p/plumb.sock", Exists: true, Publisher: pid,
				IdentityKnown: true, InodeMatches: true, IdentityMatches: false,
				Answered: false,
			},
			wantNote: true, wantMetadata: true, wantDoctor: "warn",
		},
		{
			name: "the daemon is gone and the path with it",
			state: socketPathState{
				Path: "/p/plumb.sock", Publisher: pid,
				IdentityKnown: true, Answered: true,
			},
			wantFault: true, wantExitFailure: true, wantDoctor: "fail",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.state.fault() != ""; got != tt.wantFault {
				t.Errorf("fault() set = %v, want %v (fault=%q note=%q)", got, tt.wantFault, tt.state.fault(), tt.state.note())
			}
			if got := tt.state.note() != ""; got != tt.wantNote {
				t.Errorf("note() set = %v, want %v (note=%q)", got, tt.wantNote, tt.state.note())
			}
			if got := tt.state.MetadataChanged(); got != tt.wantMetadata {
				t.Errorf("MetadataChanged() = %v, want %v", got, tt.wantMetadata)
			}
			err := debugDaemonsExit(daemons.Verdict{State: daemons.StateOK}, []socketPathState{tt.state})
			if got := err != nil; got != tt.wantExitFailure {
				t.Errorf("exit failure = %v, want %v", got, tt.wantExitFailure)
			}
			rows := daemonSocketResults([]socketPathState{tt.state})
			switch tt.wantDoctor {
			case "":
				if len(rows) != 0 {
					t.Errorf("doctor rows = %+v, want none", rows)
				}
			case "warn":
				if len(rows) != 1 || !rows[0].ok || !rows[0].warn || rows[0].fix != "" {
					t.Errorf("doctor rows = %+v, want one warning with no fix", rows)
				}
			case "fail":
				if len(rows) != 1 || rows[0].ok || rows[0].fix == "" {
					t.Errorf("doctor rows = %+v, want one failure with a fix", rows)
				}
			}
		})
	}
}

// TestDaemonSocketResults is doctor's half: the same state becomes a failed row
// with a remedy, a warning, or nothing at all.
func TestDaemonSocketResults(t *testing.T) {
	faulted := daemonSocketResults([]socketPathState{{Path: "/run/plumb/plumb.sock", Publisher: 4242, IdentityKnown: true}})
	if len(faulted) != 1 || faulted[0].ok || faulted[0].fix == "" {
		t.Fatalf("a socket fault must be one failed row with a fix, got %+v", faulted)
	}
	// The facts, not the sentence (PLAN-551): the row names the daemon that
	// published the path, and the path itself is the row.
	if !strings.Contains(faulted[0].detail, "4242") {
		t.Errorf("the row must name the publishing daemon (4242), got %q", faulted[0].detail)
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

// TestSocketPathState_NotesAnIdentityLessRecord pins PLAN-551 MINOR-2: a record
// with a socket and no identity — what a pre-529 daemon writes — must not silence
// the reader. It is a note, never a fault (the file may well be that daemon's
// socket), and it says so when the census found the publisher does NOT hold it.
func TestSocketPathState_NotesAnIdentityLessRecord(t *testing.T) {
	held := socketPathState{Path: "/run/plumb/plumb.sock", Exists: true, Publisher: 4242, Answered: true, OwnedBy: []int{4242}}
	notHeld := held
	notHeld.OwnedBy = []int{99}
	unanswered := held
	unanswered.Answered, unanswered.OwnedBy = false, nil

	for name, s := range map[string]socketPathState{"held": held, "not held": notHeld, "unanswered": unanswered} {
		if f := s.fault(); f != "" {
			t.Errorf("%s: an identity-less record is not evidence of a fault, got %q", name, f)
		}
		if !strings.Contains(s.note(), "4242") {
			t.Errorf("%s: the reader must say something about daemon 4242's identity-less record, got %q", name, s.note())
		}
	}
	if !strings.Contains(notHeld.note(), "does not hold it open") {
		t.Errorf("the census answered that 4242 does not hold the path; the note must say so: %q", notHeld.note())
	}
	if strings.Contains(unanswered.note(), "does not hold it open") {
		t.Errorf("the census did not answer; the note must not claim 4242 does not hold the path: %q", unanswered.note())
	}
	if strings.Contains(held.note(), "does not hold it open") {
		t.Errorf("4242 does hold the path; the note must not claim otherwise: %q", held.note())
	}
	// Control: the same record WITH an identity, matching, is silent.
	known := held
	known.IdentityKnown, known.InodeMatches, known.IdentityMatches = true, true, true
	if n := known.note(); n != "" {
		t.Errorf("a record with a matching identity must be silent, got %q", n)
	}
	rows := daemonSocketResults([]socketPathState{notHeld})
	if len(rows) != 1 || !rows[0].warn {
		t.Errorf("doctor must show the identity-less record as one warning row, got %+v", rows)
	}
}

// TestDaemonSocketPublication_KeepsALongLivedIdentityLessRecord pins the reach of
// PLAN-551's note: a pre-529 daemon publishes once and never refreshes, so its
// record ages past the freshness window within 90 s. While its pid is still a
// `plumb daemon` the record stays a publication; once the pid is not one (it was
// recycled), it does not. A record WITH an identity is unaffected: a current
// daemon refreshes, so a stale one is still dropped.
func TestDaemonSocketPublication_KeepsALongLivedIdentityLessRecord(t *testing.T) {
	withTempRuntime(t)
	path := filepath.Join(t.TempDir(), "plumb.sock")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	authPath := authorityLockPathForTest(t)
	if err := os.MkdirAll(filepath.Dir(authPath), 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-10 * time.Minute)
	write := func(m authorityMetadata) {
		t.Helper()
		if err := os.WriteFile(authPath, authorityJSON(t, m), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	isDaemon := true
	stubDaemonIdentity(t, func(pid int) bool { return pid == os.Getpid() && isDaemon })

	write(authorityMetadata{PID: os.Getpid(), Socket: path, Published: old})
	if pub, ok := daemonSocketPublication(); !ok || pub.pid != os.Getpid() {
		t.Fatalf("a ten-minute-old identity-less record of a live plumb daemon was dropped: %+v %v", pub, ok)
	}
	isDaemon = false // the pid now names somebody else
	if _, ok := daemonSocketPublication(); ok {
		t.Fatal("a stale identity-less record whose pid is no longer a plumb daemon was kept")
	}
	isDaemon = true
	write(authorityMetadata{PID: os.Getpid(), Socket: path, SocketDev: 1, SocketIno: 2, Published: old})
	if _, ok := daemonSocketPublication(); ok {
		t.Fatal("a stale record WITH an identity was kept: a current daemon refreshes, so it is gone")
	}
}
