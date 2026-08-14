package tools

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// git_child.go holds how plumb RUNS the git child, as opposed to git_exec.go's
// what it runs: the environment the child is given, and the bound on waiting
// for it. Both were previously left at Go's defaults, and both defaults are
// wrong for a long-lived daemon that runs a repository's own hooks.

// gitChildSpec bundles the two settings that decide how the git child runs.
// They travel together, as one value threaded through runGit, so a call site
// cannot pick up the environment and silently miss the bound; both come from
// the same trust-gated [git] block, and neither has a meaningful default a
// caller should be inventing locally.
//
// Concurrency: immutable once built; safe to share.
type gitChildSpec struct {
	// Env is the child's environment as gitChildEnv built it. nil means inherit
	// the daemon's, byte-for-byte the behaviour before the knob existed.
	Env []string
	// WriteTimeout is [git] write_timeout. ZERO means the compiled default, not
	// "no bound": GitPolicy is constructed by hand in tests and by any consumer
	// that does not care about this knob, and an unset field resolving to an
	// unbounded child is the one outcome that must be unreachable — such a child
	// holds the per-repository lock and a drain token for as long as it lives.
	WriteTimeout time.Duration
}

// gitChildSpecFor adapts a resolved GitPolicy into the spec runGit needs.
func gitChildSpecFor(p GitPolicy) gitChildSpec {
	return gitChildSpec{Env: gitChildEnv(p.Env), WriteTimeout: p.WriteTimeout}
}

// writeTimeout resolves the bound actually applied, substituting the compiled
// default for an unset (or nonsensical) value. See the field comment.
func (s gitChildSpec) writeTimeout() time.Duration {
	if s.WriteTimeout <= 0 {
		return defaultGitWriteTimeout
	}
	return s.WriteTimeout
}

// defaultGitWriteTimeout is the compiled default for [git] write_timeout: the
// bound on an index/ref-mutating git child once it is decoupled from request
// and daemon cancellation, so a shutdown mid-commit lets git finish and release
// .git/index.lock rather than being SIGKILLed holding it.
//
// It was 2 minutes, under a comment calling that "generous enough for a slow
// pre-commit hook (go build + golangci-lint)". That claim does not survive a
// machine running several agents: such a hook queues behind a peer's run on the
// shared golangci-lint cache, and those waits have been measured well past two
// minutes on plumb's own repository. Ten minutes covers the observed contention
// with room to spare while still bounding a genuinely wedged child — and, since
// the number is now a knob, a repository whose hooks need longer has an answer
// that is not "plumb kills my commits".
const defaultGitWriteTimeout = 10 * time.Minute

// gitChildEnv builds the environment for the git child process from the
// resolved [git] env overrides.
//
// It EXTENDS the daemon's inherited environment rather than replacing it, and
// that direction is not a convenience — it is the only one that works. git
// needs PATH to find itself and its subcommands, HOME to read the user's
// ~/.gitconfig and known_hosts, and SSH_AUTH_SOCK to authenticate a fetch or
// push; a hook needs whatever toolchain the user's shell would have given it.
// A replacing knob would make every entry a complete environment the user has
// to reconstruct by hand, and getting it subtly wrong fails at the worst
// moment — mid-push, against a remote. Extending also keeps the empty case
// honest: with no overrides configured the child inherits exactly what it
// inherited before this knob existed.
//
// Returns nil when there is nothing to override, so the caller leaves cmd.Env
// nil and os/exec inherits the daemon's environment directly — byte-for-byte
// the previous behaviour, not a reconstruction of it.
//
// An override REPLACES any inherited value of the same name; that is the point
// (GOWORK=off has to beat an inherited GOWORK). Setting a name to the empty
// string sets the variable to empty. There is deliberately no way to UNSET an
// inherited variable: any sentinel meaning "unset" would collide with a
// legitimate value, and no need for one has been demonstrated.
//
// Concurrency: pure apart from reading os.Environ(); safe for concurrent use.
func gitChildEnv(overrides map[string]string) []string {
	if len(overrides) == 0 {
		return nil
	}
	env := os.Environ()
	// Deterministic order. setGitEnvVar matches names case-SENSITIVELY, which is
	// right on Linux and Darwin (both have case-sensitive environments) but not
	// on Windows, where os/exec folds case when it deduplicates cmd.Env and keeps
	// the LAST of the matching entries. Sorting the names is what makes that last
	// one a defined choice rather than map-iteration luck.
	names := make([]string, 0, len(overrides))
	for k := range overrides {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		env = setGitEnvVar(env, k, overrides[k])
	}
	return env
}

// setGitEnvVar replaces the value of key in env if present, otherwise appends
// "key=value". env is modified in place.
func setGitEnvVar(env []string, key, val string) []string {
	prefix := key + "="
	for i, e := range env {
		if strings.HasPrefix(e, prefix) {
			env[i] = prefix + val
			return env
		}
	}
	return append(env, prefix+val)
}

// gitChildWaitDelay bounds how long cmd.Wait may keep waiting on the output
// pipes once the git child itself is gone. Matches RunArgv's (cmdexec.go).
const gitChildWaitDelay = 5 * time.Second

// boundGitChildWait applies the exec hygiene a git child needs, exactly as
// RunArgv does for task commands (cmdexec.go): its own process group, a
// group-kill on cancellation, and a WaitDelay. execGitCmd is the caller, so
// every git child routed through that chokepoint gets it — which is the one
// that runs the repository's hooks and can open an editor, i.e. the only one
// with a realistic way to trigger the hazard below. The auxiliary read queries
// spawned around it (git_ref_guard.go, git_exec.go's helpers, git.go,
// git_init.go) run neither, and are left unbounded rather than plumbed for a
// case that cannot arise.
//
// Without the WaitDelay, cmd.Wait() on a command whose Stdout/Stderr are
// bytes.Buffers can block FOREVER — long past the context deadline, and past
// the death of git itself. os/exec gives such a command an os.Pipe and copies
// from it in a goroutine, and Wait waits for that copy to reach EOF; EOF comes
// only when every holder of the write end closes it. Any descendant that
// inherited it therefore pins Wait for its own lifetime. Cancelling the
// context does not help: it SIGKILLs the direct child, which is not the
// process holding the pipe.
//
// For plumb that is worse than a hung call. runGit's mutating tiers hold the
// per-repo lock and a gitWriteInflight token, both released by
// beginSerialisedGit's deferred cleanup — which never runs while Wait is
// parked. One such call wedges EVERY later non-read git op on that repository,
// from every session, and leaves the shutdown drain unable to complete. It is
// reachable today: `git rebase -i` and `git tag -a` invoke GIT_EDITOR
// unconditionally, and a pre-commit hook that backgrounds a process without
// redirecting its output does the same thing accidentally.
//
// The residual cost is small and preferable: when the delay does expire on a
// child that exited SUCCESSFULLY, Wait returns exec.ErrWaitDelay and plumb
// reports the op as failed even though git did the work (gitExitDescription
// explains that case). The alternative is the unbounded park above.
func boundGitChildWait(cmd *exec.Cmd) {
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = gitChildWaitDelay
}

// gitWaitDelayNote explains an exec.ErrWaitDelay to the caller, which otherwise
// surfaces as the bare, unactionable "exec: WaitDelay expired before I/O
// complete". git ran; something it started outlived it while still holding the
// output pipes, so plumb stopped waiting rather than park forever.
func gitWaitDelayNote() string {
	return fmt.Sprintf(
		"a process started by this command outlived git while still holding its output pipes, so plumb stopped waiting after %s "+
			"rather than block indefinitely. The git operation itself may well have completed — check the repository state before retrying. "+
			"The usual cause is a hook that backgrounds a process without redirecting its output, or an editor the command opened",
		gitChildWaitDelay)
}
