package tools

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"time"
)

// git_childrun.go holds gitChildRun, one git child from start to reported
// outcome. runGitIn (git_exec.go) builds it; git_background.go hands it to a
// finisher when the child outlives the call.

// gitChildRun is one git child from start to reported outcome. It exists so a
// child that outlives the call's foreground deadline can be handed, whole, to a
// background finisher that reports it on the same terms a foreground call would.
//
// Concurrency: owned by one goroutine at a time — the calling one, then (once
// detached) the finisher alone; the caller touches nothing after detaching.
type gitChildRun struct {
	ctx, execCtx context.Context
	repoRoot     string
	sub          string
	argv         []string
	tier         gitTier
	child        gitChildSpec
	guard        *gitRefGuard
	warning      string
	cleanup      func()
	// afterExit is the own-writes refresh (git_own_writes.go), run once the child
	// has exited and before the per-repo lock is released.
	afterExit func()
	start     time.Time
	// detachAfter is the call's foreground deadline, measured from start; zero
	// means the call waits the child out.
	detachAfter    time.Duration
	stdout, stderr bytes.Buffer
	goWorkOff      string
}

func (r *gitChildRun) mutating() bool { return r.tier == tierWrite || r.tier == tierDestructive }

// exec starts the child and waits for it — or, past the foreground deadline,
// detaches it and returns the registered background op instead of a result.
// argv is passed rather than read from r so the argv reaching exec stays a
// visible parameter (it is always the tool's own classified argv), and is
// stored on r for the failure report.
func (r *gitChildRun) exec(argv []string) (string, *gitBackgroundOp, error) {
	r.argv = argv
	cmd := exec.CommandContext(r.execCtx, "git", argv...)
	cmd.Dir = r.repoRoot
	cmd.Env = r.child.Env
	cmd.Stdout = &r.stdout
	cmd.Stderr = &r.stderr
	goWorkOff, wait, err := startGitCmd(cmd, r.mutating(), r.repoRoot)
	r.goWorkOff = goWorkOff
	if err == nil {
		var op *gitBackgroundOp
		if op, err = r.awaitOrDetach(cmd, wait); op != nil {
			return "", op, nil
		}
	}
	if err != nil {
		// git check-ignore exits 1 when NONE of the listed paths are ignored —
		// a normal "no match" result, not a failure.
		if r.sub == "check-ignore" && isExitCode(err, 1) && strings.TrimSpace(r.stderr.String()) == "" {
			out, perr := postProcessGit(r.ctx, r.repoRoot, r.sub, r.stdout.String())
			return out, nil, perr
		}
		return "", nil, r.failure(err)
	}
	r.guard.postExec(r.execCtx)
	processed, err := postProcessGit(r.ctx, r.repoRoot, r.sub, r.output())
	return r.warning + processed, nil, err
}

// output is the child's report: stdout, or stderr when stdout is empty
// (switch/push and friends report on stderr).
func (r *gitChildRun) output() string {
	if out := r.stdout.String(); strings.TrimSpace(out) != "" {
		return out
	}
	return r.stderr.String()
}
