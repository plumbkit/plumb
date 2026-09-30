package tools

import (
	"context"
	"path/filepath"
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

// TestRunTaskArgv pins the task-command seam (run_task, mutation_test): under a
// go.work that lists another checkout of the directory's module the command runs
// with GOWORK=off and says so, PWD names the directory it runs in (not the daemon's),
// and everything else is inherited. RunArgv — run_command's path — never does this:
// an agent's own `go work use .` must not be refused by plumb.
func TestRunTaskArgv(t *testing.T) {
	hermeticGoEnv(t)
	t.Setenv("PLUMB_RUNARGV_PROBE", "inherited")
	t.Setenv("PWD", "/") // what an auto-spawned daemon has
	root := goWorkLayout(t, map[string]string{
		"go.work":     "go 1.21\n\nuse ./main\n",
		"main/go.mod": goModMain,
		"wt/go.mod":   goModMain,
	})
	wt, main := filepath.Join(root, "wt"), filepath.Join(root, "main")
	probe := []string{"sh", "-c", `printf '%s|%s|%s' "${GOWORK-UNSET}" "$PWD" "${PLUMB_RUNARGV_PROBE-UNSET}"`}
	ctx := context.Background()

	res, err := RunTaskArgv(ctx, wt, probe, nil, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if want := "off|" + wt + "|inherited"; res.Stdout != want {
		t.Errorf("a task command in the shadowed worktree saw %q, want %q", res.Stdout, want)
	}
	if res.GoWorkOff != filepath.Join(root, "go.work") {
		t.Errorf("GoWorkOff = %q, want the go.work that was switched off", res.GoWorkOff)
	}

	res, err = RunTaskArgv(ctx, main, probe, nil, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if want := "UNSET|" + main + "|inherited"; res.Stdout != want || res.GoWorkOff != "" {
		t.Errorf("a task command in the listed checkout saw %q (GoWorkOff %q), want %q untouched", res.Stdout, res.GoWorkOff, want)
	}

	res, err = RunArgv(ctx, wt, probe, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Stdout, "UNSET|") || res.GoWorkOff != "" {
		t.Errorf("RunArgv (run_command) must never switch GOWORK off; saw %q (GoWorkOff %q)", res.Stdout, res.GoWorkOff)
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
	if !strings.Contains(res.Stdout, "lines omitted") {
		t.Error("expected a truncation marker")
	}
	// The tail survives the cap: a runner's verdict lands at the end (PLAN-441).
	if !strings.Contains(res.Stdout, "\n500\n") {
		t.Error("the last line of output was dropped by the cap")
	}
}
