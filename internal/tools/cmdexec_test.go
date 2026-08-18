package tools

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRunArgv_Success(t *testing.T) {
	res, err := RunArgv(context.Background(), "", []string{"echo", "hello"}, time.Minute)
	if err != nil {
		t.Fatalf("RunArgv: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "hello") {
		t.Errorf("got exit=%d stdout=%q", res.ExitCode, res.Stdout)
	}
}

func TestRunArgv_NonZeroExit(t *testing.T) {
	res, err := RunArgv(context.Background(), "", []string{"false"}, time.Minute)
	if err != nil {
		t.Fatalf("RunArgv: %v", err)
	}
	if res.ExitCode == 0 {
		t.Error("expected a non-zero exit code from `false`")
	}
}

func TestRunArgv_EmptyArgv(t *testing.T) {
	if _, err := RunArgv(context.Background(), "", nil, time.Minute); err == nil {
		t.Error("expected an error for an empty argv")
	}
}

func TestRunArgv_NotFound(t *testing.T) {
	if _, err := RunArgv(context.Background(), "", []string{"plumb-no-such-binary-xyz"}, time.Minute); err == nil {
		t.Error("expected an error when the binary is not on PATH")
	}
}

func TestRunArgv_Timeout(t *testing.T) {
	res, err := RunArgv(context.Background(), "", []string{"sleep", "5"}, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("RunArgv: %v", err)
	}
	if !res.TimedOut {
		t.Error("expected TimedOut=true for a command exceeding the timeout")
	}
}

func TestRunArgv_Cancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	res, err := RunArgv(ctx, "", []string{"sleep", "5"}, time.Minute)
	if err != nil {
		t.Fatalf("RunArgv: %v", err)
	}
	if !res.Cancelled {
		t.Error("expected Cancelled=true for a command whose context was cancelled")
	}
	if res.TimedOut {
		t.Error("expected TimedOut=false when the context is cancelled, not expired")
	}
	if res.ExitCode != -1 {
		t.Errorf("expected ExitCode=-1, got %d", res.ExitCode)
	}
}

func TestRunArgv_OutputCapped(t *testing.T) {
	res, err := RunArgv(context.Background(), "", []string{"seq", "1", "500"}, time.Minute)
	if err != nil {
		t.Fatalf("RunArgv: %v", err)
	}
	lines := strings.Count(res.Stdout, "\n")
	if lines > maxTaskLines+1 {
		t.Errorf("output not capped: %d lines", lines)
	}
	if !strings.Contains(res.Stdout, "truncated") {
		t.Error("expected a truncation marker")
	}
}
