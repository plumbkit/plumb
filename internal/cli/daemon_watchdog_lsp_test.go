//go:build unix

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
)

func pidGone(pid int) bool {
	return pid > 1 && errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

// waitPIDGone polls for pid to disappear for up to d.
func waitPIDGone(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for !pidGone(pid) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return true
}

// TestArmShutdownWatchdog_KillsLanguageServersBeforeExit pins what the watchdog
// owes a wedged shutdown: the language servers and their helpers are dead by
// the time it exits, because os.Exit runs no cleanup and the servers would
// otherwise outlive the daemon.
//
// The exit callback runs on the watchdog's own goroutine right after the kill
// hook, and it waits there for the processes to go. A hook that ran after exit
// (or never) would find them still alive, since nothing else is killing them.
func TestArmShutdownWatchdog_KillsLanguageServersBeforeExit(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "helper.pid")
	// A server (sh) with a helper (sleep) in its group, as gopls has its
	// telemetry child; the server waits on it.
	script := `sleep 300 & echo $! > "$1.tmp" && mv "$1.tmp" "$1"; wait`
	sup := lsp.NewSupervisor("/bin/sh", []string{"-c", script, "sh", pidFile}, nil, lsp.SupervisorOptions{
		BackoffBase: time.Hour,
		OnStart:     func(context.Context, *jsonrpc.Conn) error { return nil },
	})
	if err := sup.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(sup.Stop)
	server := sup.PID()

	helper := 0
	for deadline := time.Now().Add(10 * time.Second); helper == 0; time.Sleep(5 * time.Millisecond) {
		if b, err := os.ReadFile(pidFile); err == nil {
			helper, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		} else if time.Now().After(deadline) {
			t.Fatal("the helper never published its pid")
		}
	}
	t.Cleanup(func() { _ = syscall.Kill(helper, syscall.SIGKILL) })
	if pidGone(helper) || pidGone(server) {
		t.Fatalf("server %d or helper %d is not running before the watchdog fires", server, helper)
	}

	type outcome struct{ serverGone, helperGone bool }
	exited := make(chan outcome, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	armShutdownWatchdog(ctx, 20*time.Millisecond, func(int) {
		exited <- outcome{waitPIDGone(server, 5*time.Second), waitPIDGone(helper, 5*time.Second)}
	}, killLSPGroups)
	t.Cleanup(lsp.ReopenGroups)
	cancel()

	select {
	case got := <-exited:
		if !got.serverGone {
			t.Errorf("language server %d was still running when the watchdog exited", server)
		}
		if !got.helperGone {
			t.Errorf("language server helper %d was still running when the watchdog exited", helper)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("watchdog never exited")
	}
}

// TestArmShutdownWatchdog_RunsTheKillHookBeforeExit pins the order directly,
// without processes: the hook first, then exit.
func TestArmShutdownWatchdog_RunsTheKillHookBeforeExit(t *testing.T) {
	events := make(chan string, 2)
	ctx, cancel := context.WithCancel(t.Context())
	armShutdownWatchdog(ctx, 10*time.Millisecond,
		func(int) { events <- "exit" },
		func() { events <- "kill" })
	cancel()

	for _, want := range []string{"kill", "exit"} {
		select {
		case got := <-events:
			if got != want {
				t.Fatalf("event = %q, want %q (the kill hook must run before exit)", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %q", want)
		}
	}
}
