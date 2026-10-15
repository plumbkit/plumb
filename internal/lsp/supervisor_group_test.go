//go:build unix

package lsp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
)

// The tests below run a fake language server that is this test binary re-executed
// with an environment switch (the standard helper-process pattern), so they need
// neither sh nor sleep and behave the same on every Unix. The fake "leader"
// starts a "grandchild" in the same process group, as gopls does with its
// telemetry helper, and publishes the grandchild's pid so a test can check that
// it dies with the server.

const (
	fakeRoleEnv    = "PLUMB_FAKE_LSP_ROLE"
	fakeModeEnv    = "PLUMB_FAKE_LSP_MODE"
	fakePIDFileEnv = "PLUMB_FAKE_LSP_PIDFILE"
	fakeMarkerEnv  = "PLUMB_FAKE_LSP_MARKER"

	fakeModePlain      = "plain"       // leader and grandchild run until killed
	fakeModeIgnoreTerm = "ignore-term" // both ignore SIGTERM, so only SIGKILL stops them
	fakeModeTermMarker = "term-marker" // leader records SIGTERM in a marker file, then exits
	fakeModeCrash      = "crash"       // leader exits on its own once the grandchild is up

	// fakeLifetime bounds how long a helper process can outlive its test: a
	// mutant that stops killing the group must not strand processes for good.
	fakeLifetime = 2 * time.Minute
)

// TestFakeLSPHelperProcess is the fake server's entry point. Outside a helper
// invocation it is a no-op.
func TestFakeLSPHelperProcess(*testing.T) {
	role := os.Getenv(fakeRoleEnv)
	if role == "" {
		return
	}
	time.AfterFunc(fakeLifetime, func() { os.Exit(0) })
	mode := os.Getenv(fakeModeEnv)
	if mode == fakeModeIgnoreTerm {
		signal.Ignore(syscall.SIGTERM)
	}
	if role == "leader" {
		runFakeLeader(mode)
	}
	select {} // grandchild: wait to be killed
}

func runFakeLeader(mode string) {
	gc := exec.Command(os.Args[0], "-test.run=^TestFakeLSPHelperProcess$")
	gc.Env = append(os.Environ(), fakeRoleEnv+"=grandchild")
	// Hold the same pipes the server's stdio uses, as a real helper would.
	gc.Stdout, gc.Stderr = os.Stdout, os.Stderr
	if err := gc.Start(); err != nil {
		os.Exit(2)
	}
	pidFile := os.Getenv(fakePIDFileEnv)
	if err := os.WriteFile(pidFile+".tmp", []byte(strconv.Itoa(gc.Process.Pid)), 0o600); err != nil {
		os.Exit(2)
	}
	if err := os.Rename(pidFile+".tmp", pidFile); err != nil {
		os.Exit(2)
	}
	switch mode {
	case fakeModeCrash:
		os.Exit(3)
	case fakeModeTermMarker:
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM)
		<-sig
		if err := os.WriteFile(os.Getenv(fakeMarkerEnv), []byte("term"), 0o600); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
	select {}
}

// fakeLSP is a running fake server under a real Supervisor.
type fakeLSP struct {
	sup        *Supervisor
	leader     int // the server's own pid
	grandchild int // its helper, in the same process group
	marker     string
}

// startFakeLSP starts a Supervisor over a fake server and waits until the
// server has published its helper's pid. Whatever the test does, the helper is
// SIGKILLed when it finishes, so a failing test never strands one.
func startFakeLSP(ctx context.Context, t *testing.T, mode string, backoff time.Duration) *fakeLSP {
	t.Helper()
	dir := t.TempDir()
	f := &fakeLSP{marker: filepath.Join(dir, "marker")}
	pidFile := filepath.Join(dir, "grandchild.pid")
	env := append(os.Environ(),
		fakeRoleEnv+"=leader", fakeModeEnv+"="+mode,
		fakePIDFileEnv+"="+pidFile, fakeMarkerEnv+"="+f.marker)
	f.sup = NewSupervisor(os.Args[0], []string{"-test.run=^TestFakeLSPHelperProcess$"}, env, SupervisorOptions{
		BackoffBase: backoff,
		OnStart:     func(context.Context, *jsonrpc.Conn) error { return nil },
	})
	if err := f.sup.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	f.leader = f.sup.PID()
	if f.leader <= 1 {
		t.Fatalf("supervisor reports pid %d for a running server", f.leader)
	}

	deadline := time.Now().Add(15 * time.Second)
	for f.grandchild == 0 {
		if b, err := os.ReadFile(pidFile); err == nil {
			f.grandchild, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		if f.grandchild == 0 && time.Now().After(deadline) {
			t.Fatal("fake server never published its helper's pid")
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Cleanup(f.sup.Stop)
	// Registered after Stop so it runs first: whatever a failing test left
	// running is killed, which also lets a Stop that is stuck waiting return.
	t.Cleanup(func() {
		_ = syscall.Kill(-f.leader, syscall.SIGKILL)
		_ = syscall.Kill(f.leader, syscall.SIGKILL)
		_ = syscall.Kill(f.grandchild, syscall.SIGKILL)
	})
	// A crashing server is swept as soon as it exits, so only the others are
	// expected to be up by now.
	if mode != fakeModeCrash && processGone(f.grandchild) {
		t.Fatalf("helper %d is not running right after start", f.grandchild)
	}
	return f
}

// processGone reports whether pid no longer names a live process. A zombie that
// nothing has reaped yet still answers signal 0, so on Linux its state is read
// from /proc as well; an orphan's reaper (init, a container's --init) normally
// collects it within moments.
func processGone(pid int) bool {
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		return true
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	// The state letter follows the parenthesised command name.
	i := bytes.LastIndexByte(stat, ')')
	return i >= 0 && i+2 < len(stat) && (stat[i+2] == 'Z' || stat[i+2] == 'X')
}

func waitGone(t *testing.T, what string, pid int) {
	t.Helper()
	if pid <= 1 { // signal 0 to pid 0 or 1 does not mean what a liveness probe needs
		t.Fatalf("%s: refusing to probe pid %d", what, pid)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !processGone(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("%s (pid %d) is still running", what, pid)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// stopWithin runs Stop and fails the test if it does not return in d, instead
// of letting a regression hang the whole test binary.
func stopWithin(t *testing.T, sup *Supervisor, d time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	done := make(chan struct{})
	go func() {
		sup.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("Stop did not return within %s", d)
	}
	return time.Since(start)
}

// TestSupervisor_Stop_KillsWholeProcessGroup pins that stopping a server takes
// its helper with it. Before the server led its own group, ctx cancellation
// SIGKILLed only the server and the helper was reparented to init.
func TestSupervisor_Stop_KillsWholeProcessGroup(t *testing.T) {
	f := startFakeLSP(t.Context(), t, fakeModePlain, time.Hour)
	stopWithin(t, f.sup, 10*time.Second)
	waitGone(t, "language server", f.leader)
	waitGone(t, "language server helper", f.grandchild)
	if got := f.sup.State(); got != StateStopped {
		t.Errorf("state after Stop = %v, want stopped", got)
	}
}

// TestSupervisor_ContextCancel_KillsWholeProcessGroup is the same guarantee for
// the other way a server ends: the daemon cancelling the supervisor's lifetime
// context, with no Stop call.
func TestSupervisor_ContextCancel_KillsWholeProcessGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f := startFakeLSP(ctx, t, fakeModePlain, time.Hour)
	cancel()
	waitGone(t, "language server", f.leader)
	waitGone(t, "language server helper", f.grandchild)
}

// TestSupervisor_Stop_AsksBeforeKilling pins the polite half of the escalation:
// the group gets SIGTERM, which a server can act on, before anything is killed.
// The fake records the signal it receives; a SIGKILL-first stop leaves no mark.
func TestSupervisor_Stop_AsksBeforeKilling(t *testing.T) {
	f := startFakeLSP(t.Context(), t, fakeModeTermMarker, time.Hour)
	stopWithin(t, f.sup, 10*time.Second)
	if _, err := os.Stat(f.marker); err != nil {
		t.Fatalf("server never saw SIGTERM before it was stopped: %v", err)
	}
	waitGone(t, "language server helper", f.grandchild)
}

// TestSupervisor_Stop_KillsAServerThatIgnoresTerm pins the bounded half: a
// server that ignores SIGTERM is killed once the grace has run out, along with
// a helper that ignores it too, and Stop returns instead of waiting for them.
func TestSupervisor_Stop_KillsAServerThatIgnoresTerm(t *testing.T) {
	f := startFakeLSP(t.Context(), t, fakeModeIgnoreTerm, time.Hour)
	took := stopWithin(t, f.sup, groupTermGrace+10*time.Second)
	if took < groupTermGrace {
		t.Errorf("Stop returned after %s, before the %s grace the server was owed", took, groupTermGrace)
	}
	waitGone(t, "language server", f.leader)
	waitGone(t, "language server helper", f.grandchild)
}

// TestSupervisor_Crash_SweepsOrphanedHelpers pins that a server dying on its own
// does not leave its helper behind: the supervisor sweeps the group before it
// restarts the server.
func TestSupervisor_Crash_SweepsOrphanedHelpers(t *testing.T) {
	f := startFakeLSP(t.Context(), t, fakeModeCrash, time.Hour)
	waitGone(t, "language server", f.leader)
	waitGone(t, "language server helper", f.grandchild)
}

// TestKillAllGroups_LeavesNoDescendants pins the registry path the shutdown
// watchdog uses: it reaches servers it holds no Supervisor for, and leaves none
// of them or their helpers alive.
func TestKillAllGroups_LeavesNoDescendants(t *testing.T) {
	t.Cleanup(ReopenGroups)
	servers := []*fakeLSP{
		startFakeLSP(t.Context(), t, fakeModePlain, time.Hour),
		startFakeLSP(t.Context(), t, fakeModeIgnoreTerm, time.Hour),
	}
	if got := KillAllGroups(); got < len(servers) {
		t.Errorf("KillAllGroups signalled %d groups, want at least %d", got, len(servers))
	}
	for _, f := range servers {
		waitGone(t, "language server", f.leader)
		waitGone(t, "language server helper", f.grandchild)
	}
}

// TestSupervisor_RegistryTracksTheLiveGroup pins the registry's bookkeeping: a
// running server's group is in it, and a stopped server's is not, because a
// stale id could one day name an unrelated process.
func TestSupervisor_RegistryTracksTheLiveGroup(t *testing.T) {
	before := liveGroups.count()
	f := startFakeLSP(t.Context(), t, fakeModePlain, time.Hour)
	if got := liveGroups.count(); got != before+1 {
		t.Fatalf("registry holds %d groups while the server runs, want %d", got, before+1)
	}
	stopWithin(t, f.sup, 10*time.Second)
	if got := liveGroups.count(); got != before {
		t.Errorf("registry holds %d groups after Stop, want %d", got, before)
	}
}

// TestSupervisor_SpawnedServerStderrIsTheNullDevice pins the command that was
// actually started, not just the one newCommand builds: a pipe drained into
// io.Discard stays open for as long as a helper holds it, and Wait would then
// not return for a crashed server until the helper exited or WaitDelay ran out,
// delaying the sweep that is meant to kill the helper.
func TestSupervisor_SpawnedServerStderrIsTheNullDevice(t *testing.T) {
	f := startFakeLSP(t.Context(), t, fakeModePlain, time.Hour)
	f.sup.mu.RLock()
	proc := f.sup.proc
	f.sup.mu.RUnlock()
	if proc == nil {
		t.Fatal("no running process to inspect")
	}
	if proc.Stderr != nil {
		t.Errorf("started server's Stderr = %v, want nil (the null device)", proc.Stderr)
	}
}

// TestSupervisor_NewCommand_ProcessGroupPolicy pins the command's wiring
// without spawning anything: its own group, a Cancel that does not SIGKILL only
// the leader, a bounded wait, and the settings the supervisor already applied.
func TestSupervisor_NewCommand_ProcessGroupPolicy(t *testing.T) {
	env := []string{"A=1"}
	cmd := NewSupervisor("server", []string{"--stdio"}, env, SupervisorOptions{Dir: "/"}).newCommand(t.Context())

	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Errorf("SysProcAttr.Setpgid is not set: %+v", cmd.SysProcAttr)
	}
	if cmd.Cancel == nil {
		t.Error("Cancel is nil: cancellation would SIGKILL the leader alone")
	}
	if cmd.WaitDelay != groupTermGrace {
		t.Errorf("WaitDelay = %s, want %s", cmd.WaitDelay, groupTermGrace)
	}
	if cmd.Dir != "/" || len(cmd.Env) != 1 || cmd.Env[0] != "A=1" {
		t.Errorf("dir/env not applied: dir=%q env=%v", cmd.Dir, cmd.Env)
	}
	if cmd.Stderr != nil {
		t.Errorf("Stderr = %v, want nil (the null device) so no pipe outlives the server", cmd.Stderr)
	}
}

// TestKillAllGroups_LatchesLaterSpawns: once KillAllGroups has run the daemon
// is exiting, so a server spawned afterwards is killed by its spawner and the
// start fails, rather than leaving a server to outlive the daemon.
func TestKillAllGroups_LatchesLaterSpawns(t *testing.T) {
	t.Cleanup(ReopenGroups)
	KillAllGroups()
	before := liveGroups.count()
	// The refused server records its own pid, so the test can see it die rather
	// than relying on the test's context to reap it.
	pidFile := filepath.Join(t.TempDir(), "pid")
	sup := NewSupervisor("/bin/sh", []string{"-c", `echo $$ > "$0"; exec sleep 300`, pidFile}, nil, SupervisorOptions{BackoffBase: time.Hour})
	readyCh, err := sup.StartAsync(t.Context())
	if err != nil {
		t.Fatalf("StartAsync: %v", err)
	}
	defer sup.Stop()
	select {
	case err := <-readyCh:
		if err == nil || !strings.Contains(err.Error(), "shutting down") {
			t.Fatalf("start after KillAllGroups = %v, want the shutting-down refusal", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("start after KillAllGroups never reported")
	}
	if got := liveGroups.count(); got != before {
		t.Fatalf("registry holds %d groups after a refused spawn, want %d", got, before)
	}
	// Killed at once, the server usually dies before its shell writes the pid.
	// One that lived long enough to write it must be gone; one left running
	// (the spawner forgot to kill it) writes it and stays. An EMPTY file is a
	// shell killed between the redirect's truncate and the echo — it died
	// before writing, which is the pass case, not a pid to probe.
	time.Sleep(500 * time.Millisecond)
	if data, err := os.ReadFile(pidFile); err == nil {
		if pid, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil && pid > 0 {
			waitGone(t, "the server spawned after KillAllGroups", pid)
		}
	}
}

// TestSupervisor_FailedOnStartLeavesNoGroup: a server whose OnStart fails is
// killed and reaped, and its group leaves the registry — a stale id there would
// one day be signalled by KillAllGroups after the pid was recycled.
func TestSupervisor_FailedOnStartLeavesNoGroup(t *testing.T) {
	before := liveGroups.count()
	cmd, args := longLivedCommand(t)
	sup := NewSupervisor(cmd, args, nil, SupervisorOptions{
		BackoffBase: time.Hour,
		OnStart:     func(context.Context, *jsonrpc.Conn) error { return errors.New("initialize failed") },
	})
	readyCh, err := sup.StartAsync(t.Context())
	if err != nil {
		t.Fatalf("StartAsync: %v", err)
	}
	defer sup.Stop()
	select {
	case err := <-readyCh:
		if err == nil {
			t.Fatal("a failing OnStart reported success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the failed start never reported")
	}
	deadline := time.Now().Add(5 * time.Second)
	for liveGroups.count() != before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := liveGroups.count(); got != before {
		t.Fatalf("registry holds %d groups after a failed OnStart, want %d", got, before)
	}
}
