package lsp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/procgroup"
)

// groupTermGrace is how long a server has to exit after its process group is
// sent SIGTERM (a Stop, or cancellation of the supervisor's context) before the
// leader is killed outright. It is deliberately short: the pool has already run
// the LSP shutdown handshake by the time it stops a supervisor, and the pool's
// per-entry stop budget (supStopGrace in internal/cli) is one second, so a
// longer grace here would be abandoned by the daemon and leave the escalation to
// the watchdog.
const groupTermGrace = 500 * time.Millisecond

// State is the lifecycle state of a supervised process.
type State int

const (
	StateStopped    State = iota // not yet started or cleanly stopped
	StateStarting                // process is being spawned
	StateRunning                 // process is running and conn is live
	StateRestarting              // crashed; waiting before retry
)

func (s State) String() string {
	switch s {
	case StateStopped:
		return "stopped"
	case StateStarting:
		return "starting"
	case StateRunning:
		return "running"
	case StateRestarting:
		return "restarting"
	default:
		return "unknown"
	}
}

// SupervisorOptions controls the behaviour of a Supervisor.
type SupervisorOptions struct {
	// OnStart is called each time the process (re)starts with the fresh Conn.
	// If OnStart returns an error the supervisor logs it and restarts again.
	OnStart func(ctx context.Context, conn *jsonrpc.Conn) error

	// BackoffBase is the initial restart delay (default 500ms).
	BackoffBase time.Duration

	// BackoffMax is the maximum restart delay (default 30s).
	BackoffMax time.Duration

	// Dir is the working directory for the server process. Set it to the
	// workspace root: the daemon deliberately runs from "/", so a server left to
	// inherit the daemon's cwd would resolve any relative path of its own against
	// a directory that owns no project. Empty means inherit (tests).
	Dir string
}

// errSupervisorStop is the cancellation cause Stop() sets on the loop's
// lifetime context, letting the loop tell a deliberate stop apart from an
// external cancellation (daemon shutdown, a Start caller's own ctx). Only the
// latter is reported as a first-start failure.
var errSupervisorStop = errors.New("supervisor: stopped")

// Supervisor manages the lifecycle of an LSP server subprocess.
// It monitors the process and restarts it with exponential backoff on crash.
//
// Each server leads its own process group, so stopping it (Stop, or cancelling
// the lifetime context) takes its descendants too: SIGTERM to the group, then
// SIGKILL to the leader after groupTermGrace, then SIGKILL to whatever the
// group still holds once the leader has been reaped. Without that a server's
// helper processes (gopls's telemetry child, a build tool's workers) outlive it,
// reparented to init. The same sweep runs after a server crashes, before its
// restart. A descendant that starts a group of its own (sourcekit-lsp's
// background swift-build) is outside this reach.
//
// If the daemon itself dies without running any of this, the server is reclaimed
// only by the platform: on Linux the kernel SIGKILLs the direct child
// (Pdeathsig, see procgroup.DieWithParent); on Darwin, which has no equivalent,
// the server exits only if it notices that its stdin has closed. That Darwin
// residual is why a SIGKILLed daemon can leave servers running there.
//
// Concurrency: all exported methods are safe for concurrent use.
type Supervisor struct {
	command string
	args    []string
	env     []string
	opts    SupervisorOptions

	mu    sync.RWMutex
	state State
	conn  *jsonrpc.Conn
	proc  *exec.Cmd

	cancel context.CancelCauseFunc
	wg     sync.WaitGroup
}

// NewSupervisor creates a Supervisor for the given command.
// opts.OnStart must be set before calling Start.
func NewSupervisor(command string, args, env []string, opts SupervisorOptions) *Supervisor {
	if opts.BackoffBase == 0 {
		opts.BackoffBase = 500 * time.Millisecond
	}
	if opts.BackoffMax == 0 {
		opts.BackoffMax = 30 * time.Second
	}
	return &Supervisor{
		command: command,
		args:    args,
		env:     env,
		opts:    opts,
		state:   StateStopped,
	}
}

// StartAsync spawns the process and begins the supervision loop, returning
// immediately without waiting for the first OnStart to complete. The returned
// channel receives exactly one value: nil once the process is running and the
// first OnStart has succeeded, or a non-nil error if the first spawn/OnStart
// fails (the loop does not retry a first-start failure). A first start cut
// short by a deliberate Stop delivers nil, not an error: the server did not
// fail — it was stopped — so there is no dead entry for a caller's self-heal
// to evict (the pool's hibernate/wake cycle depends on this: hibernation Stops
// a still-warming supervisor and must not lose the entry it is designed to
// keep). The channel is buffered (cap 1) so the loop's single send never
// blocks even if the caller stops reading; callers may abandon the channel at
// any time.
//
// ctx is the supervisor's lifetime and must outlive any single request — pass
// the daemon root context, never a per-tool-call context. Cancelling ctx (or
// calling Stop) stops the loop.
func (s *Supervisor) StartAsync(ctx context.Context) (<-chan error, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	s.mu.Lock()
	if s.state != StateStopped {
		s.mu.Unlock()
		cancel(nil)
		return nil, fmt.Errorf("supervisor: already running (state=%s)", s.state)
	}
	s.cancel = cancel
	s.mu.Unlock()

	readyCh := make(chan error, 1)
	s.wg.Add(1)
	go s.loop(ctx, readyCh)
	return readyCh, nil
}

// Start spawns the process and blocks until the first OnStart has succeeded or
// failed. As with StartAsync, a first start cut short by a deliberate Stop
// returns nil, not an error — the server was stopped, it did not fail.
// Cancelling ctx stops the loop and returns ctx's error; call Stop for a clean
// shutdown. Retained for synchronous callers and tests; the daemon hot path
// uses StartAsync so a slow cold-start handshake never blocks a tool call.
func (s *Supervisor) Start(ctx context.Context) error {
	readyCh, err := s.StartAsync(ctx)
	if err != nil {
		return err
	}
	s.mu.RLock()
	cancel := s.cancel
	s.mu.RUnlock()
	select {
	case err := <-readyCh:
		return err
	case <-ctx.Done():
		if cancel != nil {
			cancel(nil)
		}
		return ctx.Err()
	}
}

// Stop stops the supervisor loop and the server's whole process group: SIGTERM
// at once, SIGKILL for anything still alive after groupTermGrace. It returns
// once the leader has been reaped and its group swept, so it is bounded by
// roughly groupTermGrace. Callers should still run Client.Shutdown + Exit first;
// that handshake is what lets a well-behaved server flush its state.
func (s *Supervisor) Stop() {
	s.mu.RLock()
	cancel := s.cancel
	s.mu.RUnlock()
	if cancel != nil {
		cancel(errSupervisorStop)
	}
	s.wg.Wait()

	s.mu.Lock()
	s.state = StateStopped
	s.conn = nil
	s.proc = nil
	s.mu.Unlock()
}

// Conn returns the active connection, or nil if not running.
func (s *Supervisor) Conn() *jsonrpc.Conn {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.conn
}

// State returns the current supervisor state.
func (s *Supervisor) State() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// PID returns the OS process ID of the supervised language server, or 0 when no
// process is currently running (stopped, starting, or between restarts). Used
// to sample a child server's resource usage (see internal/monitor.ProcessRSS).
func (s *Supervisor) PID() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.proc == nil || s.proc.Process == nil {
		return 0
	}
	return s.proc.Process.Pid
}

// firstStartOutcome decides what an aborted first start reports on readyCh.
// A deliberate Stop (the loop's own cancel cause) reports nil — the server did
// not fail, it was stopped, so there is nothing for the pool's self-heal to
// evict. Anything else, including external cancellation of the supervisor's
// lifetime context, reports the error as before.
func firstStartOutcome(ctx context.Context, err error) error {
	if errors.Is(context.Cause(ctx), errSupervisorStop) {
		return nil
	}
	return err
}

// retryAfterStartFailure decides what happens after a failed start — either the
// spawn itself or OnStart. On the FIRST attempt the error is handed to readyCh
// (Start is still blocked on it) and supervision gives up, because a server that
// never came up once is a configuration problem, not a transient one. After that
// it backs off and reports that the loop should retry.
//
// Returns the next backoff and whether to continue supervising. Both start paths
// need identical handling, and keeping one copy is what holds loop's complexity
// down — it was the tree's only function over the cognitive-complexity budget.
func (s *Supervisor) retryAfterStartFailure(
	ctx context.Context, readyCh chan<- error, first bool, backoff time.Duration, err error,
) (time.Duration, bool) {
	if first {
		readyCh <- firstStartOutcome(ctx, err)
		return backoff, false
	}
	s.setState(StateRestarting)
	if !s.sleep(ctx, backoff) {
		return backoff, false
	}
	return min(backoff*2, s.opts.BackoffMax), true
}

// loop is the supervision goroutine.  It spawns the process, calls OnStart,
// waits for the process to exit, then retries with exponential backoff.
func (s *Supervisor) loop(ctx context.Context, readyCh chan<- error) {
	defer s.wg.Done()
	backoff := s.opts.BackoffBase
	first := true

	for {
		if ctx.Err() != nil {
			if first {
				readyCh <- firstStartOutcome(ctx, ctx.Err())
			}
			return
		}

		s.mu.Lock()
		s.state = StateStarting
		s.mu.Unlock()

		conn, proc, err := s.spawn(ctx)
		if err != nil {
			slog.Error("supervisor: failed to spawn", "command", s.command, "err", err)
			var retry bool
			backoff, retry = s.retryAfterStartFailure(ctx, readyCh, first, backoff,
				fmt.Errorf("supervisor: spawn %q: %w", s.command, err))
			if !retry {
				return
			}
			continue
		}

		s.mu.Lock()
		s.state = StateRunning
		s.conn = conn
		s.proc = proc
		s.mu.Unlock()

		if s.opts.OnStart != nil {
			if err := s.opts.OnStart(ctx, conn); err != nil {
				slog.Error("supervisor: OnStart failed", "err", err)
				_ = conn.Close()
				_ = procgroup.Kill(proc.Process.Pid)
				// exec.Cmd.Wait (not os.Process.Wait) so the cmd's stdin/stdout pipe
				// parent ends are closed and its IO goroutines joined, matching the
				// normal-exit path — os.Process.Wait leaks both until GC.
				_ = waitAndReap(proc)
				var retry bool
				backoff, retry = s.retryAfterStartFailure(ctx, readyCh, first, backoff,
					fmt.Errorf("supervisor: OnStart: %w", err))
				if !retry {
					return
				}
				continue
			}
		}

		if first {
			readyCh <- nil
			first = false
			backoff = s.opts.BackoffBase // reset on success
		}

		// Wait for the process to exit.
		err = waitAndReap(proc)
		if ctx.Err() != nil {
			s.setState(StateStopped)
			return
		}
		slog.Warn("supervisor: process exited unexpectedly", "command", s.command, "err", err)
		_ = conn.Close()
		s.setState(StateRestarting)
		if !s.sleep(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, s.opts.BackoffMax)
	}
}

// newCommand builds the server's command. It is the one place the process-group
// policy is applied, so a test can pin it without spawning anything.
func (s *Supervisor) newCommand(ctx context.Context) *exec.Cmd {
	// The previous justification here claimed s.command and s.args were "not from
	// user input". That was false, and it was load-bearing: until
	// config.LoadProject forced [lsp.<lang>] exec fields back to the global config,
	// a cloned repository's .plumb/config.toml could set them, and this line ran the
	// attacker's argv as the user on attach. The suppression is correct only because
	// that chokepoint now guarantees command/args/env come from the TRUSTED global
	// config; do not widen what reaches here without revisiting it.
	cmd := exec.CommandContext(ctx, s.command, s.args...) //nolint:gosec // G204: command/args are global-config-only by construction — config.LoadProject forces the project file's [lsp.<lang>] exec fields back to base
	if len(s.env) > 0 {
		cmd.Env = s.env
	}
	if s.opts.Dir != "" {
		cmd.Dir = s.opts.Dir
	}

	// Lead a process group of our own, so the signals below reach the server's
	// descendants and not just the server.
	procgroup.Set(cmd)
	procgroup.DieWithParent(cmd)
	// Cancelling ctx (Stop, daemon shutdown) asks the whole group to exit rather
	// than SIGKILLing only the leader, as exec.CommandContext does by default;
	// WaitDelay then kills the leader if the request is ignored. The sweep in
	// waitAndReap handles whatever is left once the leader is gone.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return procgroup.Terminate(cmd.Process.Pid)
	}
	cmd.WaitDelay = groupTermGrace
	// Stderr stays nil, which connects it to the null device: nothing to read and
	// nothing to block on. A pipe drained into io.Discard would instead stay open
	// for as long as any descendant holds it, and Wait would not return for a
	// crashed server until they exited (or WaitDelay ran out), delaying the sweep
	// that is meant to kill them.
	return cmd
}

// spawn starts the subprocess and returns a Conn wired to its stdio.
func (s *Supervisor) spawn(ctx context.Context) (*jsonrpc.Conn, *exec.Cmd, error) {
	cmd := s.newCommand(ctx)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("starting %q: %w", s.command, err)
	}
	liveGroups.add(cmd.Process.Pid)

	conn := jsonrpc.NewConn(stdout, stdin)
	return conn, cmd, nil
}

// waitAndReap waits for the server to exit, then SIGKILLs whatever it left in
// its process group and drops the group from the registry. The group is swept
// even when the server exited on its own: a crashed server's helpers are
// orphans nothing will ever talk to again.
func waitAndReap(proc *exec.Cmd) error {
	pgid := proc.Process.Pid
	err := proc.Wait()
	reapGroup(pgid)
	return err
}

func (s *Supervisor) setState(state State) {
	s.mu.Lock()
	s.state = state
	s.conn = nil
	s.proc = nil
	s.mu.Unlock()
}

// sleep blocks for d or until ctx is cancelled.  Returns false if cancelled.
func (s *Supervisor) sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}
