package lsp

import (
	"context"
	"errors"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
)

// longLivedCommand returns a command + args for a process that stays alive
// without speaking LSP, so a test can drive readiness from the OnStart hook
// rather than from the subprocess. Skips the test if no such binary is present.
func longLivedCommand(t *testing.T) (string, []string) {
	t.Helper()
	if path, err := exec.LookPath("sleep"); err == nil {
		return path, []string{"30"}
	}
	t.Skip("no 'sleep' binary on PATH")
	return "", nil
}

// missingCommand is a path that cannot exist, so spawn fails immediately.
const missingCommand = "/nonexistent/plumb-supervisor-test-binary"

func waitForState(t *testing.T, sup *Supervisor, want State, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if sup.State() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("supervisor state = %v, want %v after %s", sup.State(), want, timeout)
}

func TestSupervisor_StartAsync_ReadyThenStop(t *testing.T) {
	cmd, args := longLivedCommand(t)
	sup := NewSupervisor(cmd, args, nil, SupervisorOptions{
		OnStart: func(context.Context, *jsonrpc.Conn) error { return nil },
	})

	readyCh, err := sup.StartAsync(context.Background())
	if err != nil {
		t.Fatalf("StartAsync: %v", err)
	}

	select {
	case e := <-readyCh:
		if e != nil {
			t.Fatalf("ready err: %v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for readiness")
	}
	if got := sup.State(); got != StateRunning {
		t.Fatalf("state = %v, want running", got)
	}

	sup.Stop()
	if got := sup.State(); got != StateStopped {
		t.Fatalf("state after Stop = %v, want stopped", got)
	}
}

func TestSupervisor_StartAsync_SpawnFailureNoRetry(t *testing.T) {
	var onStartCalls atomic.Int32
	sup := NewSupervisor(missingCommand, nil, nil, SupervisorOptions{
		BackoffBase: 10 * time.Millisecond,
		OnStart: func(context.Context, *jsonrpc.Conn) error {
			onStartCalls.Add(1)
			return nil
		},
	})

	readyCh, err := sup.StartAsync(context.Background())
	if err != nil {
		t.Fatalf("StartAsync: %v", err)
	}
	defer sup.Stop()

	select {
	case e := <-readyCh:
		if e == nil {
			t.Fatal("expected spawn error, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for spawn failure")
	}

	// A first-start failure must not retry: OnStart is never reached (spawn
	// failed before it) and the loop has returned rather than looping on backoff.
	time.Sleep(100 * time.Millisecond)
	if n := onStartCalls.Load(); n != 0 {
		t.Fatalf("OnStart called %d times after spawn failure; want 0 (no retry)", n)
	}
}

// TestSupervisor_StartAsync_SlowOnStartFailure_SendsToAbandonedChan pins the
// contract the pool's late-failure drain depends on: when the FIRST OnStart
// fails AFTER the caller has abandoned readyCh (the pool's awaitReady returned
// at its grace), the buffered send still lands and is readable, and the loop
// does not retry. The pool reads exactly this value to evict the dead entry.
func TestSupervisor_StartAsync_SlowOnStartFailure_SendsToAbandonedChan(t *testing.T) {
	cmd, args := longLivedCommand(t)
	release := make(chan struct{})
	var onStartCalls atomic.Int32
	sup := NewSupervisor(cmd, args, nil, SupervisorOptions{
		BackoffBase: 10 * time.Millisecond,
		OnStart: func(context.Context, *jsonrpc.Conn) error {
			onStartCalls.Add(1)
			<-release
			return errors.New("initialize failed")
		},
	})

	readyCh, err := sup.StartAsync(context.Background())
	if err != nil {
		t.Fatalf("StartAsync: %v", err)
	}
	defer sup.Stop()

	// Simulate the pool abandoning readyCh at its grace, then the OnStart failing
	// slowly afterwards.
	close(release)
	select {
	case e := <-readyCh:
		if e == nil {
			t.Fatal("expected the late OnStart failure on readyCh, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("late OnStart failure was never delivered to the abandoned readyCh")
	}

	// A first-start failure must not retry: OnStart runs exactly once.
	time.Sleep(100 * time.Millisecond)
	if n := onStartCalls.Load(); n != 1 {
		t.Fatalf("OnStart called %d times after a first-start failure; want 1 (no retry)", n)
	}
}

func TestSupervisor_StartAsync_SlowOnStart(t *testing.T) {
	cmd, args := longLivedCommand(t)
	release := make(chan struct{})
	sup := NewSupervisor(cmd, args, nil, SupervisorOptions{
		OnStart: func(context.Context, *jsonrpc.Conn) error {
			<-release
			return nil
		},
	})

	readyCh, err := sup.StartAsync(context.Background())
	if err != nil {
		t.Fatalf("StartAsync: %v", err)
	}
	defer sup.Stop()

	// Not ready while OnStart is still running.
	select {
	case <-readyCh:
		t.Fatal("became ready before OnStart returned")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	select {
	case e := <-readyCh:
		if e != nil {
			t.Fatalf("ready err: %v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out after releasing OnStart")
	}
}

// TestSupervisor_StartAsync_AbandonedReadyChan verifies the loop's single
// buffered send into readyCh never blocks the supervisor even when the caller
// never reads the channel — the pool relies on this after its grace window.
func TestSupervisor_StartAsync_AbandonedReadyChan(t *testing.T) {
	cmd, args := longLivedCommand(t)
	sup := NewSupervisor(cmd, args, nil, SupervisorOptions{
		OnStart: func(context.Context, *jsonrpc.Conn) error { return nil },
	})

	if _, err := sup.StartAsync(context.Background()); err != nil { // ignore readyCh
		t.Fatalf("StartAsync: %v", err)
	}
	defer sup.Stop()

	waitForState(t, sup, StateRunning, 2*time.Second)
}

func TestSupervisor_Start_Sync(t *testing.T) {
	cmd, args := longLivedCommand(t)
	sup := NewSupervisor(cmd, args, nil, SupervisorOptions{
		OnStart: func(context.Context, *jsonrpc.Conn) error { return nil },
	})

	if err := sup.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sup.Stop()
	if got := sup.State(); got != StateRunning {
		t.Fatalf("state = %v, want running", got)
	}
}

func TestSupervisor_Start_Sync_Failure(t *testing.T) {
	sup := NewSupervisor(missingCommand, nil, nil, SupervisorOptions{
		BackoffBase: 10 * time.Millisecond,
	})
	if err := sup.Start(context.Background()); err == nil {
		sup.Stop()
		t.Fatal("expected error from Start on a missing binary")
	}
}
