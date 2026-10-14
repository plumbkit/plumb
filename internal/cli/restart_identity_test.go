package cli

import (
	"fmt"
	"os"
	"slices"
	"testing"
)

// The identity behind every pid `plumb restart` will signal. A pid read from a
// file is a claim; these tests pin what it takes to act on one, and what a reused
// pid looks like when a record outlives its daemon. Split from legacy_test.go by
// responsibility (PLAN-530 review findings).

// stubDaemonIdentity substitutes the identity check every FILE-recorded pid must
// pass. It is a package variable precisely so this is possible: a real `plumb
// daemon` cannot be started outside a container, so the POSITIVE direction — a
// claimed pid that IS named — is pinned with the check substituted, and the
// negative direction is pinned against the real function (the tests below).
//
// Not for use in a parallel test: it mutates package state.
func stubDaemonIdentity(t *testing.T, fn func(pid int) bool) {
	t.Helper()
	old := daemonIdentityCheck
	daemonIdentityCheck = fn
	t.Cleanup(func() { daemonIdentityCheck = old })
}

// TestDaemonIdentityCheck_RefusesThisTestProcess pins that the real check is the
// one wired in and that it is not vacuous: this process is alive, is this user's,
// and is not a `plumb daemon`.
func TestDaemonIdentityCheck_RefusesThisTestProcess(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Fatal("this process should be alive")
	}
	if daemonIdentityCheck(os.Getpid()) {
		t.Fatalf("the test binary (pid %d) reads as a plumb daemon: the identity check is vacuous", os.Getpid())
	}
}

// TestRestartDaemonPIDs_RefusesAPIDFileThatIsNotADaemon pins the identity half of
// the pid-file claim with the REAL check: a pid file whose pid is alive but is not
// a `plumb daemon` — what a reused pid looks like after a crash — is not stopped.
func TestRestartDaemonPIDs_RefusesAPIDFileThatIsNotADaemon(t *testing.T) {
	withShortTempRuntime(t)
	withAccountAuthority(t)
	loc := legacyLocationForTest(t)

	innocent := startSleep(t)
	if daemonIdentityCheck(innocent) {
		t.Fatalf("this test needs a process that is not a plumb daemon; pid %d reads as one", innocent)
	}
	writeRuntimeFile(t, loc.PID, fmt.Sprintf("%d\n", innocent))

	if got := restartDaemonPIDs(); slices.Contains(got, innocent) {
		t.Fatalf("restart named PID %d from %s: it is alive but is not a plumb daemon, and the kernel reuses pids", innocent, loc.PID)
	}
}

// TestRestartDaemonPIDs_IgnoresAStaleAuthorityRecord is the BLOCKING finding of the
// independent review of #661, pinned.
//
// The authority lock file is never unlinked, so after a crash, a `plumb stop` or a
// reboot it still names the pid the last holder published — while the lock itself is
// free and the kernel may already have handed that number to somebody else. A
// restart that reads the record as an identity would SIGTERM and then SIGKILL that
// stranger.
func TestRestartDaemonPIDs_IgnoresAStaleAuthorityRecord(t *testing.T) {
	withShortTempRuntime(t)
	withAccountAuthority(t)

	innocent := startSleep(t)
	path, err := authorityPath()
	if err != nil {
		t.Fatalf("authorityPath: %v", err)
	}
	f, err := acquireAuthority()
	if err != nil {
		t.Fatalf("acquireAuthority: %v", err)
	}
	if err := publishAuthority(f, authorityMetadata{PID: innocent, Socket: daemonSocketPath()}); err != nil {
		t.Fatalf("publishAuthority: %v", err)
	}
	_ = f.Close() // the holder is gone: the record stays, the lock is free

	if authorityHeld() {
		t.Fatal("nothing holds the authority here, so the probe must say so")
	}
	if got := restartDaemonPIDs(); slices.Contains(got, innocent) {
		t.Fatalf("restart named PID %d from a STALE authority record (%s): the lock is free and pids are reused", innocent, path)
	}
}

// TestRestartDaemonPIDs_RefusesAnAuthorityRecordThatIsNotADaemon pins the identity
// half of the authority claim while the lock IS held: a live holder whose record
// names a process that is not a `plumb daemon` is not signalled.
func TestRestartDaemonPIDs_RefusesAnAuthorityRecordThatIsNotADaemon(t *testing.T) {
	withShortTempRuntime(t)
	withAccountAuthority(t)

	innocent := startSleep(t)
	authority, err := acquireAuthority()
	if err != nil {
		t.Fatalf("acquireAuthority: %v", err)
	}
	defer func() { _ = authority.Close() }()
	if err := publishAuthority(authority, authorityMetadata{PID: innocent, Socket: daemonSocketPath()}); err != nil {
		t.Fatalf("publishAuthority: %v", err)
	}
	if !authorityHeld() {
		t.Fatal("this test holds the authority; the probe must see that")
	}

	if got := restartDaemonPIDs(); slices.Contains(got, innocent) {
		t.Fatalf("restart named PID %d: the record is current, but the process is not a plumb daemon", innocent)
	}
}
