//go:build darwin

package daemons

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLsofAttributesARealSocket is the test that would have caught the -b flag.
//
// The census's whole answer rests on this one external command, and a flag that
// makes lsof return nothing does not fail loudly — it makes every daemon look
// like it holds no authority, which is a false VIOLATION against a healthy
// machine. So this binds a real unix socket at a short path and requires the
// listing to name THIS process.
func TestLsofAttributesARealSocket(t *testing.T) {
	dir := shortTempDir(t)
	socket := filepath.Join(dir, "plumb.sock")
	liveSocket(t, socket)

	listing := newLsofListing([]string{socket})
	pids, err := listing.openers(socket)
	if err != nil {
		t.Fatalf("openers(%s): %v", socket, err)
	}
	me := os.Getpid()
	for _, pid := range pids {
		if pid == me {
			return
		}
	}
	t.Fatalf("lsof did not attribute %s to this process (pids %v); a census built on this would report a healthy daemon as a violation", socket, pids)
}

// TestLsofStatusErrorIsAnError pins the rule that an lsof which reports a status
// error has NOT answered. lsof exits 1 both for "nothing has any of these files
// open" and for "I could not read this path", and reading the second as the
// first accuses a healthy daemon of losing its authority: the census reports a
// violation and hands the operator `plumb stop`.
//
// The trigger is a real one, never a mock: lsof is asked about a symlink to a
// socket in a directory it cannot read. Measured on macOS, lsof 4.91: exit 1,
// nothing on stdout, and "lsof: status error on …/plumb.sock: Permission
// denied" on stderr. The symlink is what keeps the case reachable at all — the
// path must pass the census's own Lstat, and a path inside the unreadable
// directory does not.
func TestLsofStatusErrorIsAnError(t *testing.T) {
	dir := shortTempDir(t)
	_, socket := unreadableSocketDir(t, dir)
	link := filepath.Join(dir, "plumb-link.sock")
	if err := os.Symlink(socket, link); err != nil {
		t.Fatalf("symlinking %s: %v", link, err)
	}

	listing := newLsofListing([]string{link})
	pids, err := listing.openers(link)
	if err == nil {
		t.Fatalf("openers(%s) = %v with no error: a failed lsof must never be read as the answer %q",
			link, pids, "nobody has it open")
	}
	if len(pids) != 0 {
		t.Errorf("openers(%s) = %v alongside its error, want no pids", link, pids)
	}
	if !strings.Contains(err.Error(), "plumb.sock") {
		t.Errorf("error = %v, want lsof's own complaint in it: that text is the whole difference between this and an answer", err)
	}
}

// TestLsofUnstattablePathIsAnError pins the other half of the Lstat filter. A
// path that is not there has no openers; a path whose existence could not even
// be ESTABLISHED is a different thing, and reading it as "nobody has it open" is
// the same failure-as-answer that made a healthy daemon a violation. The trigger
// is the review's own: a socket inside a directory made unreadable (measured:
// Lstat fails with EACCES there), and this must come back as an error the census
// reports as unknown.
func TestLsofUnstattablePathIsAnError(t *testing.T) {
	dir := shortTempDir(t)
	_, socket := unreadableSocketDir(t, dir)

	listing := newLsofListing([]string{socket})
	pids, err := listing.openers(socket)
	if err == nil {
		t.Fatalf("openers(%s) = %v with no error: a path the census cannot stat is a gap, never the answer %q",
			socket, pids, "nobody has it open")
	}
	if len(pids) != 0 {
		t.Errorf("openers(%s) = %v alongside its error, want no pids", socket, pids)
	}
}

// TestLsofMissingPathIsNoOpenersAndNoError pins the rule that a path which is not
// there is not a question for lsof at all: nothing can have it open, so the
// census answers "no openers" without provoking the status error that a missing
// path produces (measured: exit 1, nothing on stdout, "lsof: status error …
// No such file or directory"). Asking anyway would turn an ordinary idle account
// — its lock file never created, its socket unlinked — into an unknown.
//
// The second path is what gives the first its teeth: an existing file nobody has
// open is lsof's own clean "exit 1, nothing listed, nothing complained about",
// so this test fails the moment the missing path is passed through.
func TestLsofMissingPathIsNoOpenersAndNoError(t *testing.T) {
	dir := shortTempDir(t)
	missing := filepath.Join(dir, "plumb.authority.lock")
	idle := filepath.Join(dir, "idle.lock")
	if err := os.WriteFile(idle, nil, 0o600); err != nil {
		t.Fatalf("creating %s: %v", idle, err)
	}

	listing := newLsofListing([]string{missing, idle})
	pids, err := listing.openers(missing)
	if err != nil {
		t.Fatalf("openers(%s) = %v, want no error: a path that is not there has no openers by definition", missing, err)
	}
	if len(pids) != 0 {
		t.Errorf("openers(%s) = %v, want none", missing, pids)
	}
}

// TestLsofPartialOutputIsStillUsed pins the other half of the stderr rule: lsof
// stops at the path it could not read but still prints the ones it could
// (measured: a live socket beside an unreadable symlink IS named, with the
// status error on stderr), so a complaint that came WITH output is not an error.
// Discarding that listing would throw away real attribution.
//
// The path it could not read is the mirror: a listing that complained does not
// answer "nobody" for the paths it never listed, because it may not have looked
// at them at all.
func TestLsofPartialOutputIsStillUsed(t *testing.T) {
	dir := shortTempDir(t)
	_, unreachable := unreadableSocketDir(t, dir)
	link := filepath.Join(dir, "plumb-link.sock")
	if err := os.Symlink(unreachable, link); err != nil {
		t.Fatalf("symlinking %s: %v", link, err)
	}
	socket := filepath.Join(dir, "open.sock")
	liveSocket(t, socket)

	listing := newLsofListing([]string{link, socket})
	pids, err := listing.openers(socket)
	if err != nil {
		t.Fatalf("openers(%s) = %v, want the partial listing used rather than turned into an error", socket, err)
	}
	found := false
	for _, pid := range pids {
		found = found || pid == os.Getpid()
	}
	if !found {
		t.Fatalf("lsof did not attribute %s to this process (pids %v) although it listed it", socket, pids)
	}
	if pids, err := listing.openers(link); err == nil {
		t.Errorf("openers(%s) = %v with no error, want an error: the listing complained, so it does not answer for the path it could not read", link, pids)
	}
}

// shortTempDir is a temp directory short enough to hold a unix socket: a macOS
// t.TempDir() path exhausts the ~104-byte sun_path ceiling and the bind fails
// with EINVAL, which says nothing about length.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "plumb-sock-") //nolint:usetesting // the socket path must stay under the sun_path ceiling
	if err != nil {
		t.Fatalf("creating a short temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// liveSocket binds a unix socket at path so lsof has something real to
// attribute, and closes it when the test ends.
func liveSocket(t *testing.T, path string) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listening on %s: %v", path, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
}

// unreadableSocketDir makes dir/protected, binds a live socket inside it, and
// then makes the directory deny search, returning both paths: the socket is a
// path the census cannot stat, and a symlink to it is a path that stats but that
// lsof cannot read. Both provoke a real failure without mocking the helper.
func unreadableSocketDir(t *testing.T, dir string) (protected, socket string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory with mode 000 regardless, so neither Lstat nor lsof would fail")
	}
	protected = filepath.Join(dir, "protected")
	if err := os.Mkdir(protected, 0o700); err != nil {
		t.Fatalf("creating %s: %v", protected, err)
	}
	socket = filepath.Join(protected, "plumb.sock")
	liveSocket(t, socket)
	if err := os.Chmod(protected, 0o000); err != nil {
		t.Fatalf("making %s unreadable: %v", protected, err)
	}
	// Restore before the temp-dir cleanup descends into it (cleanups run last
	// registered first).
	t.Cleanup(func() { _ = os.Chmod(protected, 0o700) })
	return protected, socket
}

// TestHelperCommandsAreBounded is the other half of the timeout: the bound must
// actually be applied, and a helper that does not answer in time must come back
// as a deadline error (which the census turns into an unknown reason) rather than
// hanging the caller.
func TestHelperCommandsAreBounded(t *testing.T) {
	old := helperTimeout
	helperTimeout = time.Nanosecond
	t.Cleanup(func() { helperTimeout = old })

	if _, err := psCommandLines(); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("psCommandLines with a 1ns bound = %v, want a deadline error", err)
	}

	// The path must EXIST, or lsof is never asked about it: a path that is not
	// there has no openers by definition, so the census answers that itself and
	// this assertion would pass or fail for a reason that has nothing to do with
	// the bound.
	existing := filepath.Join(shortTempDir(t), "idle.lock")
	if err := os.WriteFile(existing, nil, 0o600); err != nil {
		t.Fatalf("creating %s: %v", existing, err)
	}
	listing := newLsofListing([]string{existing})
	if _, err := listing.openers(existing); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("lsof with a 1ns bound = %v, want a deadline error", err)
	}
}

// TestPS_TimeoutIsAnUnknownReason follows the deadline one step further: the
// probe must report an unreadable process table rather than answering "no
// daemons" from a helper that never ran. A clean answer here would be the exact
// fail-open the census exists to prevent.
func TestPS_TimeoutIsAnUnknownReason(t *testing.T) {
	old := helperTimeout
	helperTimeout = time.Nanosecond
	t.Cleanup(func() { helperTimeout = old })

	found, unknown := osProcesses(os.Getuid())
	if len(found) != 0 {
		t.Errorf("a timed-out ps produced %d daemons: %+v", len(found), found)
	}
	if len(unknown) == 0 {
		t.Fatal("a timed-out ps produced no unknown reason, so the census would read as clean")
	}
	if !strings.Contains(strings.Join(unknown, " "), "the process table could not be read") {
		t.Errorf("unknown reasons = %v, want one saying the process table could not be read", unknown)
	}
}

// TestProcArgsOverridesThePSLine is the test for darwin's two-step rule: ps is
// only a prefilter, and the process's own arguments are what decide. The
// stand-in is a shell whose command line contains the words `plumb daemon` —
// exactly what ps cannot distinguish from a real daemon — and the kernel's argv
// for it must refuse it.
//
// It starts a shell, never a daemon, so it is safe on a developer's machine.
func TestProcArgsOverridesThePSLine(t *testing.T) {
	// Two commands, so the shell does not replace itself with the last one: the
	// argv has to keep the words that make ps offer it as a candidate.
	cmd := exec.Command("/bin/sh", "-c", "sleep 5; true # plumb daemon")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the stand-in shell: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		lines, err := psCommandLines()
		if err != nil {
			t.Fatalf("psCommandLines: %v", err)
		}
		if line, ok := lines[cmd.Process.Pid]; ok {
			_, command, _ := strings.Cut(line, " ")
			// ps must offer this process as worth confirming, or the test would
			// pass without exercising the rule it exists for.
			if !psLineMentionsPlumb(command) {
				t.Fatalf("ps did not show the stand-in as mentioning plumb: %q", command)
			}
			_, args, err := procArgs(cmd.Process.Pid)
			if err != nil {
				t.Fatalf("procArgs for pid %d: %v", cmd.Process.Pid, err)
			}
			if isPlumbDaemonArgv(args) {
				t.Fatalf("the kernel's argv %q was accepted as a daemon; ps must never be the verdict", args)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the stand-in shell never appeared in ps")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
