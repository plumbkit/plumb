package cli

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// What it takes to call a legacy daemon COMPATIBLE (PLAN-552 MINOR-5): a version
// file says which build wrote it, not which build is serving now — a SIGKILL
// leaves it behind. Split from legacy_test.go by responsibility.

// publishLegacyDaemon publishes a legacy location's runtime files the way a live
// daemon does at listen — its pid file, then its version file — for a stand-in
// process the identity check is stubbed to accept. It returns the stand-in's pid.
func publishLegacyDaemon(t *testing.T, loc legacyLocation, version string) int {
	t.Helper()
	pid := startReapedSleep(t)
	stubDaemonIdentity(t, func(p int) bool { return p == pid })
	writeRuntimeFile(t, loc.PID, strconv.Itoa(pid))
	writeRuntimeFile(t, loc.Version, version)
	return pid
}

// startReapedSleep starts a stand-in process that is reaped the moment it exits,
// so processAlive stops naming it at once instead of seeing a zombie.
func startReapedSleep(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the stand-in process: %v", err)
	}
	reaped := make(chan struct{})
	go func() { _ = cmd.Wait(); close(reaped) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-reaped })
	return cmd.Process.Pid
}

func TestAttachLegacyDaemon_ALeftoverVersionFileIsNotCompatible(t *testing.T) {
	cases := []struct {
		name    string
		arrange func(t *testing.T, loc legacyLocation)
	}{{
		// The daemon that wrote this build's version was SIGKILLed; its pid is gone.
		name: "the publishing daemon is dead",
		arrange: func(t *testing.T, loc legacyLocation) {
			t.Helper()
			pid := publishLegacyDaemon(t, loc, Version)
			p, _ := os.FindProcess(pid)
			_ = p.Kill()
			waitGone(t, pid)
		},
	}, {
		// A later daemon from a build older than the version file published its pid
		// but never rewrote the version file the killed one left behind.
		name: "a later daemon never rewrote the version file",
		arrange: func(t *testing.T, loc legacyLocation) {
			t.Helper()
			writeRuntimeFile(t, loc.Version, Version)
			old := time.Now().Add(-time.Hour)
			if err := os.Chtimes(loc.Version, old, old); err != nil {
				t.Fatalf("ageing %s: %v", loc.Version, err)
			}
			pid := startReapedSleep(t)
			stubDaemonIdentity(t, func(p int) bool { return p == pid })
			writeRuntimeFile(t, loc.PID, strconv.Itoa(pid))
		},
	}, {
		// The pid file names a live process that is not a plumb daemon (a reused pid).
		name: "the recorded pid is not a daemon",
		arrange: func(t *testing.T, loc legacyLocation) {
			t.Helper()
			writeRuntimeFile(t, loc.PID, strconv.Itoa(startReapedSleep(t)))
			writeRuntimeFile(t, loc.Version, Version)
		},
	}, {
		name: "there is no pid file",
		arrange: func(t *testing.T, loc legacyLocation) {
			t.Helper()
			writeRuntimeFile(t, loc.Version, Version)
		},
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withShortTempRuntime(t)
			withAccountAuthority(t)
			loc := legacyLocationForTest(t)
			countingListener(t, loc.Socket)
			tc.arrange(t, loc)

			conn, err := attachLegacyDaemon()
			if conn != nil {
				_ = conn.Close()
				t.Fatal("a version file the serving daemon did not publish made it compatible: attached")
			}
			if err == nil || !strings.Contains(err.Error(), "could not confirm the daemon serving there published it") {
				t.Fatalf("the refusal must say the version file is not the serving daemon's: %v", err)
			}
		})
	}
}

// waitGone waits for a killed stand-in to be reaped.
func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("PID %d did not exit", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAttachLegacyDaemon_EqualMtimesAreCompatible pins versionIsCurrent's boundary:
// a daemon writes its pid file and then its version file microseconds apart, and
// on a filesystem with coarse timestamps (ext4, tmpfs) the two share an mtime.
// That must read as published by the live daemon, or a healthy same-build daemon
// is refused there.
func TestAttachLegacyDaemon_EqualMtimesAreCompatible(t *testing.T) {
	withShortTempRuntime(t)
	withAccountAuthority(t)
	loc := legacyLocationForTest(t)
	countingListener(t, loc.Socket)
	publishLegacyDaemon(t, loc, Version)
	same := time.Now().Add(-time.Minute).Truncate(time.Second)
	for _, p := range []string{loc.PID, loc.Version} {
		if err := os.Chtimes(p, same, same); err != nil {
			t.Fatalf("Chtimes %s: %v", p, err)
		}
	}
	conn, err := attachLegacyDaemon()
	if err != nil || conn == nil {
		t.Fatalf("a version file written in the same tick as the pid file must be compatible: conn=%v err=%v", conn, err)
	}
	_ = conn.Close()
}
