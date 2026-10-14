package cli

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/paths"
)

// Tests for PLAN-530's legacy migration and fence.
//
// The isolation helper is withShortTempRuntime, plus the account authority: with
// the authority OFF the runtime directory IS the environment's answer, so on a
// platform where the environment's answer and the cache directory coincide (all
// of macOS, and Linux with no usable $XDG_RUNTIME_DIR) there is no legacy
// location to test at all — which is itself the common case the plan calls out,
// and is covered by TestLegacyLocations_NoSecondLocationWhenTheyCoincide.
//
// What these tests are really pinning, in the order the plan asks for it:
//   - a probe takes no lock and creates nothing (a lock-taking probe makes a
//     concurrent `plumb serve` wait out its whole start deadline and fail);
//   - a compatible legacy daemon is reused, an incompatible one is refused with
//     an instruction, and the compatibility decision reads the version file of
//     the SAME directory the socket is in;
//   - every launch lock is handed to the successor and none is released;
//   - a restart stops by identity and fails closed when the exit or the release
//     cannot be confirmed.

// withAccountAuthority turns the account authority on for a test that has already
// isolated itself. It is the seam's gate (PLUMB_ACCOUNT_AUTHORITY), reachable in
// a test binary and in a `-tags=plumbtest` child with separateenv evidence, and
// it is what makes the runtime directory account-derived — hence different from
// the environment's answer an older binary would have used.
func withAccountAuthority(t *testing.T) {
	t.Helper()
	t.Setenv("PLUMB_ACCOUNT_AUTHORITY", "1")
}

// legacyLocationForTest returns this isolated account's one legacy runtime
// location and creates the directory, so a test can stand a daemon up in it.
func legacyLocationForTest(t *testing.T) legacyLocation {
	t.Helper()
	locs := legacyLocations()
	if len(locs) == 0 {
		t.Fatal("this test account has no legacy runtime location: the isolation helper must leave the environment's runtime directory different from the account's")
	}
	loc := locs[0]
	if err := os.MkdirAll(loc.Dir, 0o700); err != nil {
		t.Fatalf("creating the legacy runtime directory %s: %v", loc.Dir, err)
	}
	return loc
}

// countingListener stands a unix listener up at path and counts the connections
// it accepts, so a test can prove a connection it holds really reached that
// socket. It accepts in a goroutine and closes each connection.
func countingListener(t *testing.T, path string) *net.UnixListener {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("binding unix socket %s (%d bytes): %v", path, len(path), err)
	}
	unixLn, ok := ln.(*net.UnixListener)
	if !ok {
		t.Fatalf("unix listener has type %T", ln)
	}
	t.Cleanup(func() { _ = unixLn.Close() })
	go func() {
		for {
			conn, err := unixLn.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	return unixLn
}

// writeRuntimeFile writes one of a runtime directory's files.
func writeRuntimeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// dirEntries lists a directory's entries, for the "the probe created nothing"
// assertions: a probe that creates a lock file, a socket or a marker changes it.
func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatalf("reading %s: %v", dir, err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// --- the probe: no lock, no file -------------------------------------------------

func TestProbeLegacyDaemons_TakesNoLockAndCreatesNothing(t *testing.T) {
	withShortTempRuntime(t)
	withAccountAuthority(t)
	loc := legacyLocationForTest(t)
	countingListener(t, loc.Socket)
	writeRuntimeFile(t, loc.Version, Version)

	before := dirEntries(t, loc.Dir)
	got := probeLegacyDaemons()
	if len(got) != 1 || got[0].loc.Dir != loc.Dir {
		t.Fatalf("probe found %d legacy daemons at %v, want the one at %s", len(got), got, loc.Dir)
	}
	if !got[0].Compatible() {
		t.Fatalf("a daemon publishing this build's version must be compatible, got %q", got[0].running)
	}
	if after := dirEntries(t, loc.Dir); !reflect.DeepEqual(before, after) {
		t.Fatalf("the probe changed the legacy runtime directory:\nbefore: %v\nafter:  %v", before, after)
	}

	// THE LOCK. A probe that took it — even for an instant — makes a concurrent
	// `plumb serve` see it held and wait out its whole start deadline. So the lock
	// file must not exist afterwards, and must be free (not held) when the test
	// creates it.
	if _, err := os.Stat(loc.Lock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the probe created the legacy lifetime lock %s (stat err = %v)", loc.Lock, err)
	}
	writeRuntimeFile(t, loc.Lock, "")
	held, err := acquireLegacyLock(loc.Lock)
	if err != nil {
		t.Fatalf("the legacy lifetime lock is held after a probe: %v", err)
	}
	_ = held.File.Close()
}

func TestProbeLegacyDaemons_CreatesNothingWhenThereIsNothing(t *testing.T) {
	withShortTempRuntime(t)
	withAccountAuthority(t)
	_ = os.RemoveAll(legacyLocations()[0].Dir)

	if got := probeLegacyDaemons(); len(got) != 0 {
		t.Fatalf("probe found %v where nothing is listening", got)
	}
	if _, err := os.Stat(legacyLocations()[0].Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the probe created the legacy runtime directory %s", legacyLocations()[0].Dir)
	}
}

// --- compatible attach, and the four paths resolved together ---------------------

func TestAttachLegacyDaemon_CompatibleDaemonIsReused(t *testing.T) {
	withShortTempRuntime(t)
	withAccountAuthority(t)
	loc := legacyLocationForTest(t)
	countingListener(t, loc.Socket)
	// The version file is read from the directory the SOCKET is in. The runtime
	// directory in use is given a version file of its own that does NOT match, so a
	// probe reading the wrong one refuses a daemon it should reuse.
	writeRuntimeFile(t, daemonVersionPath(), "another-build")
	writeRuntimeFile(t, loc.Version, Version)

	conn, err := attachLegacyDaemon()
	if err != nil {
		t.Fatalf("attachLegacyDaemon: %v", err)
	}
	if conn == nil {
		t.Fatal("a compatible legacy daemon must be reused, not refused")
	}
	_ = conn.Close()
}

func TestAttachLegacyDaemon_IncompatibleDaemonIsRefusedWithInstructions(t *testing.T) {
	withShortTempRuntime(t)
	withAccountAuthority(t)
	loc := legacyLocationForTest(t)
	countingListener(t, loc.Socket)
	// The reverse of the test above, and the reason both exist: the version in the
	// runtime directory IN USE says this build, so a probe reading that file instead
	// of the daemon's own would call an incompatible daemon compatible.
	writeRuntimeFile(t, daemonVersionPath(), Version)
	writeRuntimeFile(t, loc.Version, "0.0.1-from-an-older-build")

	conn, err := attachLegacyDaemon()
	if conn != nil {
		t.Fatal("an incompatible legacy daemon must never be attached to")
	}
	if err == nil {
		t.Fatal("an incompatible legacy daemon must be refused")
	}
	for _, want := range []string{"plumb restart", "plumb service install", loc.Socket, "0.0.1-from-an-older-build"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q must name %q", err, want)
		}
	}
}

func TestAttachLegacyDaemon_NoPublishedVersionIsNotCompatible(t *testing.T) {
	withShortTempRuntime(t)
	withAccountAuthority(t)
	loc := legacyLocationForTest(t)
	countingListener(t, loc.Socket)

	conn, err := attachLegacyDaemon()
	if conn != nil || err == nil {
		t.Fatalf("a daemon that published no version cannot be confirmed compatible: conn=%v err=%v", conn, err)
	}
	if !strings.Contains(err.Error(), "published no version") {
		t.Fatalf("the refusal must say the version could not be read: %v", err)
	}
}

func TestAttachLegacyDaemon_StaleSocketIsNotADaemon(t *testing.T) {
	withShortTempRuntime(t)
	withAccountAuthority(t)
	loc := legacyLocationForTest(t)
	// A socket FILE with nobody listening: what a crashed daemon leaves behind, and
	// the state that must NOT refuse a launch — otherwise a crash would strand the
	// account until somebody removed the file by hand.
	ln, err := net.Listen("unix", loc.Socket)
	if err != nil {
		t.Fatalf("binding %s: %v", loc.Socket, err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false) //nolint:errcheck // the file is what this test is about
	_ = ln.Close()

	conn, err := attachLegacyDaemon()
	if conn != nil || err != nil {
		t.Fatalf("a socket file nobody accepts on is not a daemon: conn=%v err=%v", conn, err)
	}
}

func TestConnectOrStartDaemon_IncompatibleLegacyDaemonSpawnsNothing(t *testing.T) {
	withShortTempRuntime(t)
	withAccountAuthority(t)
	loc := legacyLocationForTest(t)
	countingListener(t, loc.Socket)
	writeRuntimeFile(t, loc.Version, "0.0.1-from-an-older-build")

	_, err := connectOrStartDaemon(context.Background(), daemonSocketPath())
	if err == nil {
		t.Fatal("serve must fail with instructions rather than start beside an incompatible legacy daemon")
	}
	if !strings.Contains(err.Error(), "plumb restart") {
		t.Fatalf("the failure must name the remedy: %v", err)
	}
	// Nothing was started: no socket in the runtime directory in use, and no legacy
	// fence either — the refusal comes BEFORE the launch decision, so not even a
	// lock file was created.
	if _, err := os.Stat(daemonSocketPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a daemon socket exists at %s: something was started beside the legacy daemon", daemonSocketPath())
	}
	if _, err := os.Stat(loc.Lock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the launcher created %s: the refusal must happen before any launch", loc.Lock)
	}
}

// --- the fence: every legacy lock, held and handed over --------------------------

// sleepScript is a stand-in for the daemon child: it ignores the "daemon"
// argument daemonCommand passes and sleeps long enough for the assertions below.
func sleepScript(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fakedaemon")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil { //nolint:gosec // a test's own script, mode 0755 is the point
		t.Fatalf("writing the stand-in child: %v", err)
	}
	return path
}

// assertLocksHeld requires every lock the launcher handed over to still be held —
// by the CHILD, through the description the parent closed its own copy of. That is
// the whole property: closing the parent's copy must not release anything.
func assertLocksHeld(t *testing.T, locks launchLocks) {
	t.Helper()
	for _, path := range lockPaths(t, locks) {
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("the handed-over lock %s is gone: %v", path, err)
		}
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		_ = f.Close()
		if err == nil {
			t.Fatalf("%s was RELEASED: the child that inherited it does not hold it", path)
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			t.Fatalf("%s: flock gave %v, want EWOULDBLOCK", path, err)
		}
	}
}

// assertLocksFree requires every lock to be reacquirable, which is how a test
// shows the fence ends with the child.
func assertLocksFree(t *testing.T, locks launchLocks) {
	t.Helper()
	for _, path := range lockPaths(t, locks) {
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("opening %s: %v", path, err)
		}
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		_ = f.Close()
		if err != nil {
			t.Fatalf("%s is still held after the holder exited: %v", path, err)
		}
	}
}

func lockPaths(t *testing.T, locks launchLocks) []string {
	t.Helper()
	authority, err := authorityPath()
	if err != nil {
		t.Fatalf("authorityPath: %v", err)
	}
	paths := make([]string, 0, 2+len(locks.Legacy))
	paths = append(paths, authority, daemonLockPath())
	for _, held := range locks.Legacy {
		paths = append(paths, held.Path)
	}
	return paths
}

func TestLaunchHandsOverEveryLockAndReleasesNone(t *testing.T) {
	withShortTempRuntime(t)
	withAccountAuthority(t)
	_ = legacyLocationForTest(t)

	locks, err := acquireLaunchLocks()
	if err != nil {
		t.Fatalf("acquireLaunchLocks: %v", err)
	}
	if len(locks.Legacy) == 0 {
		t.Fatal("this account has no legacy location, so this test would prove nothing")
	}
	if len(locks.Legacy) != len(legacyLocations()) {
		t.Fatalf("held %d legacy locks, want one per known location (%d)", len(locks.Legacy), len(legacyLocations()))
	}

	cmd, logFile, err := daemonCommand(daemonLaunchSpec{Executable: sleepScript(t), Env: os.Environ()}, locks)
	if err != nil {
		t.Fatalf("daemonCommand: %v", err)
	}
	defer func() { _ = logFile.Close() }()
	if len(cmd.ExtraFiles) != 2+len(locks.Legacy) {
		t.Fatalf("ExtraFiles = %d, want authority + lifetime + %d legacy locks", len(cmd.ExtraFiles), len(locks.Legacy))
	}
	handover, ok := envValue(cmd.Env, legacyLocksEnv)
	if !ok {
		t.Fatalf("the child was not told about the legacy locks: %s is missing", legacyLocksEnv)
	}
	for i, held := range locks.Legacy {
		want := fmt.Sprintf(`"fd":%d,"path":%q`, inheritedLegacyLockFDBase+i, held.Path)
		if !strings.Contains(handover, want) {
			t.Fatalf("handover %s does not contain %s", handover, want)
		}
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the stand-in child: %v", err)
	}
	// The launcher closes ITS copies once the child has them. flock belongs to the
	// open file description the two now share, so nothing is released.
	locks.closeAll()
	assertLocksHeld(t, locks)

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("killing the stand-in child: %v", err)
	}
	_ = cmd.Wait()
	assertLocksFree(t, locks)
}

// envValue finds NAME=value in an environment slice.
func envValue(env []string, name string) (string, bool) {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, name+"="); ok {
			return v, true
		}
	}
	return "", false
}

func TestHoldLegacyLocks_AdoptsTheHandoverAndRefusesAMismatch(t *testing.T) {
	withShortTempRuntime(t)
	withAccountAuthority(t)
	loc := legacyLocationForTest(t)

	// The launcher's copy, still open — which is what the child inherits.
	held, err := acquireLegacyLock(loc.Lock)
	if err != nil {
		t.Fatalf("acquireLegacyLock: %v", err)
	}
	defer func() { _ = held.File.Close() }()

	t.Setenv(legacyLocksEnv, fmt.Sprintf(`[{"fd":%d,"path":%q}]`, held.File.Fd(), loc.Lock))
	adopted, err := holdLegacyLocks()
	if err != nil {
		t.Fatalf("holdLegacyLocks: %v", err)
	}
	if len(adopted) != 1 || adopted[0].Path != loc.Lock {
		t.Fatalf("adopted %v, want the one handed-over lock at %s", adopted, loc.Lock)
	}
	defer closeHeldLegacyLocks(adopted)
	if _, ok := os.LookupEnv(legacyLocksEnv); ok {
		t.Fatalf("%s is still set, so the daemon's own children would inherit the fence", legacyLocksEnv)
	}

	// A path that is not what the fd names is refused, never re-acquired: falling
	// back would take a second lock at the expected path while the launcher still
	// holds the first.
	other := filepath.Join(t.TempDir(), "other.lock")
	writeRuntimeFile(t, other, "")
	otherFile, err := os.OpenFile(other, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("opening %s: %v", other, err)
	}
	defer func() { _ = otherFile.Close() }()
	t.Setenv(legacyLocksEnv, fmt.Sprintf(`[{"fd":%d,"path":%q}]`, otherFile.Fd(), loc.Lock))
	_, err = holdLegacyLocks()
	if err == nil || !strings.Contains(err.Error(), "refusing to start") {
		t.Fatalf("a mismatched handover must fail closed, got %v", err)
	}

	t.Setenv(legacyLocksEnv, "{not json")
	if _, err := holdLegacyLocks(); err == nil {
		t.Fatal("an unreadable handover must fail closed")
	}
}

func TestRefuseBesideLegacyDaemon(t *testing.T) {
	withShortTempRuntime(t)
	withAccountAuthority(t)
	loc := legacyLocationForTest(t)
	// COMPATIBLE, and still refused: a daemon has nothing to add to another daemon.
	writeRuntimeFile(t, loc.Version, Version)
	ln := countingListener(t, loc.Socket)

	err := refuseBesideLegacyDaemon(nil)
	if err == nil {
		t.Fatal("a daemon must refuse to start beside a legacy daemon, whatever the two builds are")
	}
	for _, want := range []string{"plumb restart", loc.Socket} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q must name %q", err, want)
		}
	}

	_ = ln.Close()
	_ = os.Remove(loc.Socket)
	if err := refuseBesideLegacyDaemon(nil); err != nil {
		t.Fatalf("nothing is serving there any more: %v", err)
	}
}

func TestAcquireLaunchLocks_RefusesAHeldLegacyLock(t *testing.T) {
	withShortTempRuntime(t)
	withAccountAuthority(t)
	loc := legacyLocationForTest(t)
	oldDaemon, err := acquireLegacyLock(loc.Lock)
	if err != nil {
		t.Fatalf("acquireLegacyLock: %v", err)
	}
	defer func() { _ = oldDaemon.File.Close() }()

	locks, err := acquireLaunchLocks()
	if err == nil {
		locks.closeAll()
		t.Fatal("a held legacy lifetime lock must stop the launch")
	}
	if !isLegacyLockHeld(err) {
		t.Fatalf("want the held-legacy-lock refusal, got %v", err)
	}
	if !strings.Contains(err.Error(), "plumb restart") {
		t.Fatalf("the refusal must name the remedy: %v", err)
	}
	// Nothing is left held but the stand-in's own lock: a launcher that loses must
	// leave the account's authority and the runtime lifetime lock exactly as it
	// found them.
	_ = oldDaemon.File.Close()
	freeLaunchLocks(t)
}

// --- restart: identity, and failing closed ---------------------------------------

// startSleep starts a live child the tests can name, refusable at the end.
func startSleep(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the stand-in process: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd.Process.Pid
}

func TestRestartDaemonPIDs_NamesIdentityNeverAScan(t *testing.T) {
	withShortTempRuntime(t)
	withAccountAuthority(t)
	loc := legacyLocationForTest(t)

	named := startSleep(t)
	unnamed := startSleep(t) // live, and named by nothing at all
	writeRuntimeFile(t, loc.PID, fmt.Sprintf("%d\n", named))

	got := restartDaemonPIDs()
	if !slices.Contains(got, named) {
		t.Fatalf("a live pid published in %s must be stopped, got %v", loc.PID, got)
	}
	if slices.Contains(got, unnamed) {
		t.Fatalf("restart named %d, which no identity names: a restart stops by authority or pid identity, never by a process scan", unnamed)
	}
	if slices.Contains(got, os.Getpid()) {
		t.Fatalf("restart named this process: %v", got)
	}
}

func TestRestartDaemonPIDs_IgnoresAStalePIDFile(t *testing.T) {
	withShortTempRuntime(t)
	withAccountAuthority(t)
	loc := legacyLocationForTest(t)
	writeRuntimeFile(t, loc.PID, "999999") // nobody

	if got := restartDaemonPIDs(); slices.Contains(got, 999999) {
		t.Fatalf("a pid file naming a dead process is not an identity: %v", got)
	}
}

func TestRestartDaemonPIDs_NamesTheAuthorityHolder(t *testing.T) {
	withShortTempRuntime(t)
	withAccountAuthority(t)
	holder := startSleep(t)
	authority, err := acquireAuthority()
	if err != nil {
		t.Fatalf("acquireAuthority: %v", err)
	}
	defer func() { _ = authority.Close() }()
	if err := publishAuthority(authority, authorityMetadata{PID: holder, Socket: daemonSocketPath()}); err != nil {
		t.Fatalf("publishAuthority: %v", err)
	}

	if got := restartDaemonPIDs(); !slices.Contains(got, holder) {
		t.Fatalf("the authority's recorded holder %d must be stopped, got %v", holder, got)
	}
}

// TestRestartStopsByIdentityNotByAScan pins the card's rule where it can be pinned
// for certain: `plumb restart` must not reach for the pgrep sweep at all, whatever the
// host makes of it. A behavioural test cannot carry this on every host — the sweep's
// ownership probe shells out to `ps` and `lsof`, and a host that denies either makes the
// sweep find nothing, which would let a reintroduced scan pass unnoticed. The rule is a
// property of the code, so the code is what is checked: the calls restart.go makes, and
// the command names it passes around, with comments ignored (the incident that rule comes
// from is worth citing, and it is cited in a comment).
func TestRestartStopsByIdentityNotByAScan(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "restart.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing restart.go: %v", err)
	}
	forbiddenCalls := []string{"findAllDaemonPIDs", "findAllDaemonByArgs", "commandOutput"}
	var found []string
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			if id, ok := x.Fun.(*ast.Ident); ok && slices.Contains(forbiddenCalls, id.Name) {
				found = append(found, id.Name+"()")
			}
		case *ast.BasicLit:
			if x.Kind != token.STRING {
				return true
			}
			// An EXACT command name, never a substring: "ps" appears inside half the
			// words this file uses legitimately ("stop", "maps").
			if unquoted, uerr := strconv.Unquote(x.Value); uerr == nil && (unquoted == "pgrep" || unquoted == "ps") {
				found = append(found, "the command name "+unquoted)
			}
		}
		return true
	})
	if len(found) > 0 {
		t.Fatalf("restart.go reaches for %s: a restart stops by authority or pid identity, never by a process scan", strings.Join(found, ", "))
	}
}

func TestWaitForDaemonExit_FailsClosedOnALiveDaemon(t *testing.T) {
	live := startSleep(t)
	err := waitForDaemonExit([]int{live}, time.Now())
	if err == nil {
		t.Fatal("a live daemon must fail the exit confirmation")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("PID %d", live)) {
		t.Fatalf("the refusal must name the pid: %v", err)
	}
	if !strings.Contains(err.Error(), "plumb restart") {
		t.Fatalf("the refusal must name the remedy: %v", err)
	}

	if err := waitForDaemonExit(nil, time.Now()); err != nil {
		t.Fatalf("nothing to wait for must confirm at once: %v", err)
	}
	if err := waitForDaemonExit([]int{live}, time.Now().Add(2*time.Second)); err == nil {
		t.Fatal("a daemon that never exits must not confirm")
	}
}

func TestRespawnDaemon_FailsClosedWhenALegacyLockIsHeld(t *testing.T) {
	withShortTempRuntime(t)
	withAccountAuthority(t)
	loc := legacyLocationForTest(t)
	// An older launcher's daemon: it holds the legacy lifetime lock and no
	// authority, which is the losing-the-race case the plan requires be
	// fail-closed rather than resolved by starting a successor.
	oldDaemon, err := acquireLegacyLock(loc.Lock)
	if err != nil {
		t.Fatalf("acquireLegacyLock: %v", err)
	}
	defer func() { _ = oldDaemon.File.Close() }()

	err = respawnDaemon(nil, time.Now().Add(2*time.Second))
	if err == nil {
		t.Fatal("a restart must not start a successor beside a daemon that holds a legacy lock")
	}
	if !strings.Contains(err.Error(), "older binary") {
		t.Fatalf("the refusal must say what is holding it: %v", err)
	}
	if dialDaemonOnce(daemonSocketPath()) {
		t.Fatal("a successor was started")
	}
}

func TestRespawnDaemon_FailsClosedWhileTheAuthorityIsHeld(t *testing.T) {
	withShortTempRuntime(t)
	withAccountAuthority(t)
	holder, err := acquireAuthority()
	if err != nil {
		t.Fatalf("acquireAuthority: %v", err)
	}
	defer func() { _ = holder.Close() }()

	err = respawnDaemon(nil, time.Now().Add(300*time.Millisecond))
	if err == nil {
		t.Fatal("a restart must fail closed while a daemon still holds the authority")
	}
	if !strings.Contains(err.Error(), "still held") {
		t.Fatalf("the refusal must say the release was not confirmed: %v", err)
	}
	if dialDaemonOnce(daemonSocketPath()) {
		t.Fatal("a successor was started")
	}
}

func TestRespawnDaemon_StartsSuccessorWithEveryLockInHand(t *testing.T) {
	withShortTempRuntime(t)
	withAccountAuthority(t)
	_ = legacyLocationForTest(t)

	// No daemon is running, so nothing holds the authority: the acquisition
	// confirms the release and the launcher starts under the fence. The stand-in
	// child never accepts, so the readiness wait is what ends the call — and by
	// then the successor has been started WITH every lock, which is the property.
	started := make(chan int, 1)
	l := newDaemonLauncher(daemonSocketPath())
	l.start = func(locks launchLocks) error {
		assertLocksHeld(t, locks) // the child holds them from the instant it exists
		started <- 1 + len(locks.Legacy)
		return nil
	}
	l.wait = 300 * time.Millisecond
	l.poll = 10 * time.Millisecond

	err := respawnDaemonWith(l, daemonSocketPath(), nil, time.Now().Add(2*time.Second))
	if err == nil {
		t.Fatal("a successor that never accepts must report a start timeout")
	}
	select {
	case legacy := <-started:
		if legacy != 1+len(legacyLocations()) {
			t.Fatalf("the successor was started with %d locks (authority counted), want 1 authority + %d locations",
				legacy, len(legacyLocations()))
		}
	case <-time.After(time.Second):
		t.Fatal("no successor was started at all")
	}
}

// --- the locations themselves ----------------------------------------------------

func TestLegacyLocations_NoneWhenTheEnvironmentIsTheAnswer(t *testing.T) {
	withShortTempRuntime(t)
	// The common unspoofed case, and always macOS without a spoofed HOME: the
	// runtime directory in use IS the environment's own answer, so there is no
	// second location to probe and none to fence. A plan that always probed a
	// "legacy" path would have to invent one here.
	if got := legacyLocations(); len(got) != 0 {
		t.Fatalf("legacy locations = %v, want none: the location in use is the environment's own answer", got)
	}
}

func TestLegacyLocations_TheEnvironmentsDirectoryWhenTheAccountIsTheAnswer(t *testing.T) {
	withShortTempRuntime(t)
	withAccountAuthority(t)
	locs := legacyLocations()
	if len(locs) != 1 {
		t.Fatalf("legacy locations = %v, want exactly the environment's runtime directory", locs)
	}
	if locs[0].Dir != paths.EnvRuntimeDir() {
		t.Fatalf("legacy location = %s, want the environment's %s", locs[0].Dir, paths.EnvRuntimeDir())
	}
	if locs[0].Dir == paths.RuntimeDir() {
		t.Fatalf("legacy location %s is the directory in use", locs[0].Dir)
	}
}

func TestCurrentLocation_IsOneDirectoryAndTheOneOlderBinariesKnow(t *testing.T) {
	withShortTempRuntime(t)
	// The account authority is ON, which is the only mode in which the environment's
	// runtime directory and this one are different directories. With it off they are
	// the same, and a "resolves every path together" assertion would hold for a
	// function that resolved half of them from the legacy location — which is how the
	// first version of this test let exactly that mutant through.
	withAccountAuthority(t)
	cur := currentLocation()
	// Every accessor names a path of the ONE location, which is what makes the
	// four-path resolution structural rather than a convention: a version file read
	// from another directory is not expressible.
	for name, got := range map[string]string{
		"socket":  daemonSocketPath(),
		"control": daemonCtrlSocketPath(),
		"pid":     daemonPIDPath(),
		"version": daemonVersionPath(),
		"lock":    daemonLockPath(),
	} {
		if filepath.Dir(got) != cur.Dir {
			t.Errorf("%s = %s, which is not under the runtime directory %s", name, got, cur.Dir)
		}
	}
	if cur.Socket != daemonSocketPath() || cur.Ctrl != daemonCtrlSocketPath() ||
		cur.PID != daemonPIDPath() || cur.Version != daemonVersionPath() || cur.Lock != daemonLockPath() {
		t.Fatalf("currentLocation() = %+v does not agree with the accessors", cur)
	}
	// The lifetime lock keeps the name an older binary opens, because that is the
	// file the fence is taken on.
	if filepath.Base(cur.Lock) != "plumb.daemon.lock" {
		t.Fatalf("the lifetime lock is %s, want plumb.daemon.lock", cur.Lock)
	}
}

func TestLegacyLocationAt_NamesOneDirectorysPaths(t *testing.T) {
	loc := legacyLocationAt("/runtime/plumb")
	for name, path := range map[string]string{
		"socket": loc.Socket, "control": loc.Ctrl, "pid": loc.PID, "version": loc.Version, "lock": loc.Lock,
	} {
		if filepath.Dir(path) != loc.Dir {
			t.Errorf("%s = %s, want it under %s", name, path, loc.Dir)
		}
	}
	if loc.Lock != filepath.Join(loc.Dir, "plumb.daemon.lock") {
		t.Errorf("lock = %s, want the name an older launcher opens", loc.Lock)
	}
	if got := locationOfLock(loc.Lock); got.Dir != loc.Dir || got.Socket != loc.Socket {
		t.Errorf("locationOfLock(%s) = %+v, want the location it belongs to", loc.Lock, got)
	}
}
