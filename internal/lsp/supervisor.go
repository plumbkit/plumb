package lsp

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
)

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

// Supervisor manages the lifecycle of an LSP server subprocess.
// It monitors the process and restarts it with exponential backoff on crash.
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

	cancel context.CancelFunc
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
// fails (the loop does not retry a first-start failure). The channel is
// buffered (cap 1) so the loop's single send never blocks even if the caller
// stops reading; callers may abandon the channel at any time.
//
// ctx is the supervisor's lifetime and must outlive any single request — pass
// the daemon root context, never a per-tool-call context. Cancelling ctx (or
// calling Stop) stops the loop.
func (s *Supervisor) StartAsync(ctx context.Context) (<-chan error, error) {
	ctx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	if s.state != StateStopped {
		s.mu.Unlock()
		cancel()
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
// failed. Cancelling ctx stops the loop; call Stop for a clean shutdown.
// Retained for synchronous callers and tests; the daemon hot path uses
// StartAsync so a slow cold-start handshake never blocks a tool call.
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
			cancel()
		}
		return ctx.Err()
	}
}

// Stop cleanly stops the supervisor loop.  The supervised process is not
// explicitly killed here — callers should call Client.Shutdown + Exit first.
func (s *Supervisor) Stop() {
	s.mu.RLock()
	cancel := s.cancel
	s.mu.RUnlock()
	if cancel != nil {
		cancel()
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

// loop is the supervision goroutine.  It spawns the process, calls OnStart,
// waits for the process to exit, then retries with exponential backoff.
func (s *Supervisor) loop(ctx context.Context, readyCh chan<- error) {
	defer s.wg.Done()
	backoff := s.opts.BackoffBase
	first := true

	for {
		if ctx.Err() != nil {
			if first {
				readyCh <- ctx.Err()
			}
			return
		}

		s.mu.Lock()
		s.state = StateStarting
		s.mu.Unlock()

		conn, proc, err := s.spawn(ctx)
		if err != nil {
			slog.Error("supervisor: failed to spawn", "command", s.command, "err", err)
			if first {
				readyCh <- fmt.Errorf("supervisor: spawn %q: %w", s.command, err)
				return
			}
			s.setState(StateRestarting)
			if !s.sleep(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, s.opts.BackoffMax)
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
				_ = proc.Process.Kill()
				// exec.Cmd.Wait (not os.Process.Wait) so the cmd's stdin/stdout pipe
				// parent ends are closed and its IO goroutines joined, matching the
				// normal-exit path — os.Process.Wait leaks both until GC.
				_ = proc.Wait()
				if first {
					readyCh <- fmt.Errorf("supervisor: OnStart: %w", err)
					return
				}
				s.setState(StateRestarting)
				if !s.sleep(ctx, backoff) {
					return
				}
				backoff = min(backoff*2, s.opts.BackoffMax)
				continue
			}
		}

		if first {
			readyCh <- nil
			first = false
			backoff = s.opts.BackoffBase // reset on success
		}

		// Wait for the process to exit.
		err = proc.Wait()
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

// spawn starts the subprocess and returns a Conn wired to its stdio.
func (s *Supervisor) spawn(ctx context.Context) (*jsonrpc.Conn, *exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, s.command, s.args...) //nolint:gosec // G204: s.command and s.args are set from adapter config (gopls/pyright binary), not from user input
	if len(s.env) > 0 {
		cmd.Env = s.env
	}
	if s.opts.Dir != "" {
		cmd.Dir = s.opts.Dir
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stdout pipe: %w", err)
	}
	// Discard stderr to avoid blocking.
	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("starting %q: %w", s.command, err)
	}

	conn := jsonrpc.NewConn(stdout, stdin)
	return conn, cmd, nil
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
