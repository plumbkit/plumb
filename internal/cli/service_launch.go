package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/plumbkit/plumb/internal/account"
	"github.com/plumbkit/plumb/internal/paths"
	"github.com/plumbkit/plumb/internal/service"
)

// What a managed launch starts (PLAN-528).
//
// Before this, `plumb serve` started os.Executable() with the calling process's
// environment. That made the account's ONE daemon a function of whichever client
// happened to start it: a branch or worktree build became the daemon, and a
// spoofed HOME/XDG rooted its config, state and logs in temporary directories
// that the next client silently inherited.
//
// A managed launch — `plumb serve`, `plumb restart`, `plumb service install`
// starting a daemon — now starts the executable the SERVICE PROFILE records,
// with an environment built from the profile and the account database. The
// profile is never inferred: no profile means no launch, with the
// `plumb service install` instruction as the remedy.
//
// LEGACY MODE IS UNTOUCHED. Until the default flip (PLAN-518 rollout invariant
// 1) `paths.AuthorityEnabled()` is true only in a test or a container harness,
// so every shipped build takes the first branch below and behaves exactly as it
// does today.

// daemonLaunchSpec is what to start and the environment to start it in.
type daemonLaunchSpec struct {
	Executable string
	Env        []string
}

// resolveLaunchSpec resolves the launch: the profile's executable in canonical
// mode, and this binary with this environment in legacy mode.
//
// The executable is deliberately NOT symlink-resolved. The installed path is
// what the profile must keep naming — on Homebrew that is the shim in bin/,
// which follows the upgrade to the next Cellar — so resolving it here would pin
// the service to a versioned directory that the next upgrade deletes.
func resolveLaunchSpec() (daemonLaunchSpec, error) {
	if !paths.AuthorityEnabled() {
		// Legacy: this binary, this environment. os.Executable is consulted
		// ONLY here — a canonical launch must not depend on the calling binary at
		// all, not even on being able to name it.
		exe, err := os.Executable()
		if err != nil {
			return daemonLaunchSpec{}, fmt.Errorf("resolving executable path: %w", err)
		}
		return daemonLaunchSpec{Executable: exe, Env: os.Environ()}, nil
	}
	p, err := service.Load()
	if err != nil {
		return daemonLaunchSpec{}, serviceProfileError(err)
	}
	if err := checkLaunchExecutable(p.Executable); err != nil {
		return daemonLaunchSpec{}, err
	}
	u, err := account.Current()
	if err != nil {
		return daemonLaunchSpec{}, fmt.Errorf("resolving the account to launch the plumb service: %w", err)
	}
	return daemonLaunchSpec{Executable: p.Executable, Env: p.LaunchEnv(u)}, nil
}

// serviceProfileError is the remedy for both ways the profile can let a launch
// down. It is one function so the message cannot drift: every refusal names
// `plumb service install`, because that is the only thing that writes one.
func serviceProfileError(err error) error {
	if errors.Is(err, service.ErrNoProfile) {
		return fmt.Errorf("no plumb service profile: refusing to guess which binary to start — "+
			"run `plumb service install` to record the installed binary, its PATH and its directories: %w", err)
	}
	return fmt.Errorf("the plumb service profile cannot be used: run `plumb service install` to rewrite it: %w", err)
}

// checkLaunchExecutable refuses a launch whose executable is not there, or is
// not something that can be run. It is checked BEFORE the child is started, so
// the user gets the path and the remedy rather than a bare fork/exec error.
func checkLaunchExecutable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("the service profile's executable %s cannot be read (%w); reinstall it with `plumb service install`", path, err)
	}
	if info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("the service profile's executable %s is not executable; reinstall it with `plumb service install`", path)
	}
	return nil
}

// daemonCommand builds the detached daemon command: the spec's executable, the
// spec's environment plus the two launch-lock handovers, and the locks on fds 3
// (authority) and 4 (lifetime). It returns the log file the caller closes once
// the child has dup'd it.
//
// It is separate from startDaemonProcess so a test can inspect exactly what
// would be started — binary, argv, environment, handover fds — without starting
// anything.
func daemonCommand(spec daemonLaunchSpec, locks launchLocks) (*exec.Cmd, *os.File, error) {
	files := locks.files()
	if len(files) != 2 {
		// Unreachable through acquireLaunchLocks, which returns both locks or
		// none. Guarded because a nil *os.File in ExtraFiles panics os/exec, and
		// because a daemon handed half the pair would be a second authority in
		// waiting.
		return nil, nil, errors.New("starting daemon: both launch locks must be held to hand them over")
	}

	logPath := daemonLogPath()
	_ = os.MkdirAll(filepath.Dir(logPath), 0o755)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		logFile, _ = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	}

	// G204: the executable is the service profile's own recorded path, checked by
	// checkLaunchExecutable before it gets here, and the argv is literal.
	cmd := exec.Command(spec.Executable, "daemon") //nolint:gosec // profile-recorded, validated path; literal argv
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// fd 3 in the child is the authority and fd 4 the lifetime lock: the order
	// the daemon adopts them in, and the order it holds them in.
	cmd.ExtraFiles = files
	cmd.Env = append(append([]string{}, spec.Env...),
		fmt.Sprintf("%s=%d", authorityLockFDEnv, inheritedAuthorityLockFD),
		fmt.Sprintf("%s=%d", daemonLockFDEnv, inheritedDaemonLockFD),
	)
	// Never let the daemon inherit this serve proxy's working directory. The daemon
	// is a singleton shared across every connection, so an inherited cwd belongs to
	// one arbitrary client's project — and any stray relative path would resolve
	// there. Root has no project to damage. runDaemon chdirs as well, covering a
	// daemon started by any other means.
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd, logFile, nil
}

// errNoDaemonToAttach is what `plumb serve --attach-only` reports when nothing
// is running. It is deliberately not a start: the flag exists for scripts that
// must fail rather than launch.
var errNoDaemonToAttach = errors.New("no plumb daemon is running and --attach-only never starts one")

// attachOnly dials the daemon and never starts one: `plumb serve --attach-only`.
//
// It takes no lock, not even briefly. A "probe" that took the authority would
// make a concurrent `plumb serve` see it held and refuse to start a daemon —
// which is how a diagnostic turns into an outage (see lock.go's contract).
// Instead it reads the authority's metadata, which names a live holder, and
// waits only while one is there: a daemon that is still starting is worth
// waiting for, and a machine with no daemon at all fails at once rather than
// after the whole bounded wait.
func attachOnly(ctx context.Context, socketPath string) (net.Conn, error) {
	l := newDaemonLauncher(socketPath)
	deadline := time.Now().Add(l.wait)
	for {
		if conn, err := l.dial(); err == nil {
			return conn, nil
		}
		if conn := l.attachToHolder(); conn != nil {
			return conn, nil
		}
		if !authorityRecorded() {
			return nil, fmt.Errorf("%w (socket: %s); start one with `plumb serve` (without --attach-only) or `plumb restart`%s",
				errNoDaemonToAttach, socketPath, socketPathLengthHint(socketPath))
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("a plumb daemon holds %s but did not accept connections within %s (socket: %s); "+
				"if it is wedged, see %s, and `plumb stop` stops it%s",
				daemonLockPath(), l.wait, socketPath, daemonLogPath(), socketPathLengthHint(socketPath))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(l.poll):
		}
	}
}

// authorityRecorded reports whether a live process holds the account authority,
// using the metadata the holder publishes through the held fd — read-only, and
// without taking the lock itself (an unheld lock file left over from a dead
// daemon reads as no record, which is the answer we want).
func authorityRecorded() bool {
	path, err := authorityPath()
	if err != nil {
		return false
	}
	m, ok := readAuthority(path)
	if !ok || m.PID <= 0 {
		return false
	}
	return processAlive(m.PID)
}

// envWarning is one line of the divergence report: whether it is worth a
// warning, its message, and the structured detail behind it.
type envWarning struct {
	Warn  bool
	Role  string
	Msg   string
	Attrs []any
}

// clientEnvWarnings builds the divergence report: one line per global directory
// this client's environment would have resolved somewhere else. A directory that
// holds a real plumb config is a warning with the remedy that adopts it; the
// rest is information, because nothing is lost by ignoring them.
//
// It is a pure function of the environment and the profile so the report can be
// tested for what it SAYS, not only for the fact that it was logged.
func clientEnvWarnings() []envWarning {
	var out []envWarning
	for _, d := range paths.ClientEnvDivergence() {
		attrs := []any{"role", d.Role, "client_env", d.Env, "service", d.Service}
		if d.Role == string(service.RoleConfig) && regularFile(filepath.Join(d.Env, "config.toml")) {
			// The remedy names the XDG BASE, which is what `set-xdg` records —
			// not the plumb directory under it, which is what diverged.
			out = append(out, envWarning{
				Warn: true,
				Role: d.Role,
				Msg: "serve: this client's environment points plumb's config at a directory that holds a config " +
					"(" + filepath.Join(d.Env, "config.toml") + "); the service uses " + d.Service +
					" — adopt it with `plumb service set-xdg --config-home " + filepath.Dir(d.Env) + "`",
				Attrs: attrs,
			})
			continue
		}
		out = append(out, envWarning{
			Role:  d.Role,
			Msg:   "serve: ignoring this client's environment for a global directory",
			Attrs: attrs,
		})
	}
	return out
}

// logClientEnvDivergence says once, per client, that its environment resolved a
// global directory somewhere other than where the account service keeps it.
//
// It is the honest half of "a client's HOME or XDG never retargets anything": we
// do ignore them, and when ignoring them would have moved a real directory we
// say so, with the remedy. A client whose environment agrees with the service
// says nothing at all, which is every ordinary client.
func logClientEnvDivergence() {
	clientEnvWarn.Do(func() {
		for _, w := range clientEnvWarnings() {
			if w.Warn {
				slog.Warn(w.Msg, w.Attrs...)
				continue
			}
			slog.Info(w.Msg, w.Attrs...)
		}
	})
}

// clientEnvWarn makes the divergence report once per process. A serve that
// reconnects (the resilient proxy redials) must not repeat it.
var clientEnvWarn sync.Once

// regularFile reports whether path names an existing non-directory, which is
// all the divergence report needs to know before it calls a directory "a config
// that would have been dropped".
func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
