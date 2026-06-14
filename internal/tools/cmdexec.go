package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// cmdexec.go is the bounded, no-shell argv executor shared by the task runner.
// It mirrors the git tool's execution hygiene (git_exec.go): captured output
// capped at 100 KiB / 200 lines, a timeout, an explicit working directory, and
// exec of an argv — never `sh -c` with interpolation — so a configured command
// cannot smuggle shell syntax.

const (
	maxTaskBytes = 100 * 1024 // 100 KiB — mirrors maxGitBytes
	maxTaskLines = 200
	// defaultTaskTimeout bounds a single task command; builds/tests can be slow
	// but never unbounded. Overridable per call.
	defaultTaskTimeout = 10 * time.Minute
)

// ExecResult is the bounded outcome of running one task command.
type ExecResult struct {
	ExitCode int    // process exit code; -1 when it timed out
	Stdout   string // captured, capped
	Stderr   string // captured, capped
	TimedOut bool
}

// RunArgv executes argv[0] with argv[1:] in workdir with NO shell, capturing
// bounded stdout/stderr under a timeout. The process is killed when ctx is
// cancelled or the timeout elapses. argv must be non-empty. A non-zero exit is
// reported in the result (not as an error); err is non-nil only when the
// command could not be started (e.g. argv[0] not on PATH).
//
// Concurrency: safe for concurrent use — each call owns its process and buffers.
func RunArgv(ctx context.Context, workdir string, argv []string, timeout time.Duration) (ExecResult, error) {
	if len(argv) == 0 {
		return ExecResult{}, fmt.Errorf("run task: empty command")
	}
	if timeout <= 0 {
		timeout = defaultTaskTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	// G204: the command is intentionally caller-supplied — running a stored task
	// command is the feature. Safety comes from the layers above: argv-only (no
	// shell), the per-workspace trust gate, and the no-metacharacter validation in
	// config.ParseTaskCommand. The argv is never built from agent free-text.
	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...) //nolint:gosec // see comment above
	cmd.Dir = workdir
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	res := ExecResult{
		Stdout:   capTaskOutput(stdout.String()),
		Stderr:   capTaskOutput(stderr.String()),
		TimedOut: errors.Is(cctx.Err(), context.DeadlineExceeded),
	}
	if res.TimedOut {
		res.ExitCode = -1
		return res, nil
	}
	var ee *exec.ExitError
	switch {
	case runErr == nil:
		res.ExitCode = 0
	case errors.As(runErr, &ee):
		res.ExitCode = ee.ExitCode()
	default:
		return res, fmt.Errorf("run %q: %w", argv[0], runErr)
	}
	return res, nil
}

// capTaskOutput bounds output to maxTaskLines lines then maxTaskBytes bytes.
func capTaskOutput(s string) string {
	s = truncateLines(s, maxTaskLines, "… (output truncated at 200 lines)")
	if len(s) > maxTaskBytes {
		s = s[:maxTaskBytes] + "\n… (output truncated at 100 KiB)"
	}
	return s
}
