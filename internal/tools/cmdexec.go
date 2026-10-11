package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// cmdexec.go is the bounded, no-shell argv executor shared by the task runner.
// It mirrors the git tool's execution hygiene (git_exec.go): captured output
// capped at 100 KiB / 200 lines (head, tail and failure lines kept — see
// task_output.go), a timeout, an explicit working directory, and
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
	ExitCode  int    // process exit code; -1 when it was killed by a signal (timeout or cancellation)
	Stdout    string // captured, capped
	Stderr    string // captured, capped
	TimedOut  bool
	Cancelled bool // the request context was cancelled, not a genuine non-zero exit
	// GoWorkOff is the go.work RunTaskArgv switched off for this command with
	// GOWORK=off (git_gowork.go), or "" when the environment was left alone. A
	// failure report names it: a command can fail BECAUSE of the switch.
	GoWorkOff string
}

// RunArgv executes argv[0] with argv[1:] in workdir with NO shell, capturing
// bounded stdout/stderr under a timeout. The process is killed when ctx is
// cancelled or the timeout elapses. argv must be non-empty. A non-zero exit is
// reported in the result (not as an error); err is non-nil only when the
// command could not be started (e.g. argv[0] not on PATH).
//
// Concurrency: safe for concurrent use — each call owns its process and buffers.
func RunArgv(ctx context.Context, workdir string, argv []string, timeout time.Duration) (ExecResult, error) {
	return runArgv(ctx, workdir, argv, nil, nil, timeout, false)
}

// RunTaskArgv is RunArgv for a stored [tasks.<lang>] command (run_task,
// mutation_test): the child inherits the daemon's environment, except that
// applyAutoGoWork may add GOWORK=off — the decision the git tool makes for a
// repository's hooks, made here for the repository's configured commands, keyed on
// workdir (git_gowork.go says when, and why run_command is not included).
// res.GoWorkOff names the go.work that was switched off.
//
// env is the command's [tasks.<lang>] env plus any per-call override, already
// expanded (KEY=VALUE). Each entry replaces an inherited one of that name, and
// they are in place BEFORE the GOWORK decision looks at the environment, so an
// explicit GOWORK there is used as is — the rule an inherited GOWORK already
// follows — and a GOENV or PATH set there informs the decision the way it will
// inform go.
//
// unset names variables REMOVED from the child's environment once env is applied,
// so it removes an inherited entry and a configured one alike — the shape
// run_task's `unset` argument needs, because the configured entry is the one a
// caller most often wants gone (GOTMPDIR). It too is applied BEFORE the GOWORK
// decision, so unsetting GOWORK leaves that switch free to decide, exactly as
// never setting it does.
//
// Concurrency: as RunArgv.
func RunTaskArgv(ctx context.Context, workdir string, argv, env, unset []string, timeout time.Duration) (ExecResult, error) {
	return runArgv(ctx, workdir, argv, env, unset, timeout, true)
}

func runArgv(ctx context.Context, workdir string, argv, env, unset []string, timeout time.Duration, autoGoWork bool) (ExecResult, error) {
	if len(argv) == 0 {
		return ExecResult{}, errors.New("run task: empty command")
	}
	// Before exec, not after: os/exec reports a missing cmd.Dir as the BINARY
	// missing (see WorkingDirError).
	if err := checkWorkingDir(workdir); err != nil {
		return ExecResult{}, err
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
	if len(env) > 0 || len(unset) > 0 {
		// cmd.Environ(), not os.Environ(): for a nil Env it is the inherited
		// environment plus PWD=<workdir>, which os/exec adds only while Env is nil.
		// Sets first, removals second: a removal has to beat an entry env just
		// applied, which is the whole point of run_task's `unset`.
		child := cmd.Environ()
		for _, kv := range env {
			k, v, _ := strings.Cut(kv, "=")
			child = withEnvVar(child, k, v)
		}
		cmd.Env = withoutEnvVars(child, unset)
	}
	goWorkOff := ""
	if autoGoWork {
		goWorkOff = applyAutoGoWork(cmd)
	}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Run in its own process group and, on timeout/cancel, SIGKILL the whole group
	// so a build/test that forked children (or a shell that backgrounded one) does
	// not leave orphans. WaitDelay bounds the wait if a descendant keeps the output
	// pipes open after the group is signalled.
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = 5 * time.Second

	runErr := cmd.Run()
	ctxErr := cctx.Err()
	res := ExecResult{
		Stdout:    capTaskOutput(stdout.String()),
		Stderr:    capTaskOutput(stderr.String()),
		TimedOut:  errors.Is(ctxErr, context.DeadlineExceeded),
		Cancelled: errors.Is(ctxErr, context.Canceled),
		GoWorkOff: goWorkOff,
	}
	if res.TimedOut || res.Cancelled {
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

// networkLabel renders the sandbox network state for a tool reply header.
func networkLabel(denied bool) string {
	if denied {
		return "off"
	}
	return "on"
}

// withoutEnvVars returns env without every entry naming one of keys. Every
// entry, not the first: os/exec keeps the LAST duplicate, so removing one and
// leaving another would leave the variable set. A nil keys (the mutation_test
// and CLI paths) returns env unchanged.
func withoutEnvVars(env []string, keys []string) []string {
	if len(keys) == 0 {
		return env
	}
	drop := make(map[string]bool, len(keys))
	for _, k := range keys {
		drop[k] = true
	}
	out := make([]string, 0, len(env))
	for _, e := range env {
		if k, _, _ := strings.Cut(e, "="); drop[k] {
			continue
		}
		out = append(out, e)
	}
	return out
}
