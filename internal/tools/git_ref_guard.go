package tools

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/plumbkit/plumb/internal/toolerror"
)

// gitRefObservation is one session's last seen HEAD commit + branch for a
// repository. An empty branch is a detached HEAD; an empty head is an unborn
// branch (a fresh repository with no commits yet).
type gitRefObservation struct {
	head   string
	branch string
}

// String renders the observation as branch@short-sha for warnings and errors.
func (o gitRefObservation) String() string {
	head := o.head
	if len(head) > 7 {
		head = head[:7]
	}
	if head == "" {
		head = "no commits"
	}
	if o.branch == "" {
		return "detached@" + head
	}
	return o.branch + "@" + head
}

// gitRefMover attributes a repository's current ref state to the plumb session
// whose own git operation left it there.
type gitRefMover struct {
	sessionID string
	name      string
	obs       gitRefObservation // the state that session's op left behind
}

// gitRefState is the per-repository ref-movement ledger: every attached
// session's last observed HEAD+branch, plus the last plumb session whose own
// operation changed that state — the attribution the cross-session warning
// names.
//
// Concurrency: mu guards observed/mover/hasMover; lastUsedNs is atomic.
// Entries live in the process-global gitRefStates map: the daemon is a
// singleton mediating every plumb git call, so the ledger is shared across
// connections by design (the same shape as repoLocks). It is in-memory only —
// after a daemon restart the first call re-baselines, and movement the ledger
// cannot attribute is deliberately friction-free, so there is nothing worth
// persisting.
type gitRefState struct {
	mu         sync.Mutex
	observed   map[string]gitRefObservation
	mover      gitRefMover
	hasMover   bool
	lastUsedNs atomic.Int64
}

var gitRefStates sync.Map // map[string]*gitRefState, keyed by resolved repo root

func gitRefStateFor(repoRoot string) *gitRefState {
	fresh := &gitRefState{observed: make(map[string]gitRefObservation)}
	fresh.lastUsedNs.Store(time.Now().UnixNano())
	v, _ := gitRefStates.LoadOrStore(repoRoot, fresh)
	st := v.(*gitRefState)
	st.lastUsedNs.Store(time.Now().UnixNano())
	return st
}

// peerMove reports whether the repository's ref state moved since session self
// last observed it AND the move is confidently attributable to a DIFFERENT
// plumb session (the recorded mover's post-op state still matches cur).
// Movement by self, by an external git process, or by an untraceable chain
// reports moved=false — only an attributed peer move earns friction.
func (s *gitRefState) peerMove(self string, cur gitRefObservation) (prev gitRefObservation, mover gitRefMover, moved bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, seen := s.observed[self]
	if !seen || prev == cur {
		return gitRefObservation{}, gitRefMover{}, false
	}
	if !s.hasMover || s.mover.sessionID == self || s.mover.obs != cur {
		return gitRefObservation{}, gitRefMover{}, false
	}
	return prev, s.mover, true
}

// record stores sessID's fresh observation. mutating is true only when the
// session's own operation changed the ref state (pre != post), which also
// makes the session the attributable mover — an `add` that left HEAD untouched
// must not take credit for another actor's move.
func (s *gitRefState) record(sessID, name string, obs gitRefObservation, mutating bool) {
	if name == "" {
		name = sessID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observed[sessID] = obs
	if mutating {
		s.mover = gitRefMover{sessionID: sessID, name: name, obs: obs}
		s.hasMover = true
	}
}

// sweepGitRefStates evicts ledger entries idle longer than repoLockIdleExpiry;
// called from StartRepoLockSweep's ticker. Losing an entry is fail-open (the
// next call re-baselines), so — unlike repoLocks — an in-flight op needs no
// protection from eviction.
func sweepGitRefStates(now time.Time) {
	gitRefStates.Range(func(key, value any) bool {
		st := value.(*gitRefState)
		if now.Sub(time.Unix(0, st.lastUsedNs.Load())) >= repoLockIdleExpiry {
			gitRefStates.Delete(key)
		}
		return true
	})
}

// observeGitRef reads the repository's current HEAD commit and branch with two
// read-only queries. Both tolerate the usual non-error failures: a detached
// HEAD reports an empty branch, and an unborn branch (or any unresolvable
// HEAD) an empty head — the second return is false only when the HEAD commit
// could not be resolved, which expected_head treats as a mismatch (fail
// closed). Argvs are literal lists (the stagedSummary precedent) so gosec can
// inspect them.
func observeGitRef(ctx context.Context, repoRoot string) (gitRefObservation, bool) {
	var obs gitRefObservation
	branchCmd := exec.CommandContext(ctx, "git", gitNoOptionalLocks, "symbolic-ref", "--short", "-q", "HEAD")
	branchCmd.Dir = repoRoot
	if out, err := branchCmd.Output(); err == nil {
		obs.branch = strings.TrimSpace(string(out))
	}
	headCmd := exec.CommandContext(ctx, "git", gitNoOptionalLocks, "rev-parse", "--verify", "-q", "HEAD")
	headCmd.Dir = repoRoot
	out, err := headCmd.Output()
	if err != nil {
		return obs, false
	}
	obs.head = strings.TrimSpace(string(out))
	return obs, true
}

// resolveGitRev resolves rev to a full commit SHA, or ok=false when it names
// no commit in this repository.
func resolveGitRev(ctx context.Context, repoRoot, rev string) (string, bool) {
	if rev == "" || strings.HasPrefix(rev, "-") {
		return "", false
	}
	cmd := exec.CommandContext(ctx, "git", gitNoOptionalLocks, "rev-parse", "--verify", "-q", rev+"^{commit}") //nolint:gosec // G204: rev is the expected_head parameter; a dash-leading value is rejected above so it cannot become a git option, and rev-parse only reads it
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	sha := strings.TrimSpace(string(out))
	return sha, sha != ""
}

// gitRefGuard carries one git call through the cross-session ref-movement
// guard. Git.armRefGuard builds it; a nil guard is the zero-overhead path for
// a call with neither a session identity to track nor an expected_head to
// enforce.
//
// Lifecycle inside runGit: repoRoot is assigned once the repository root is
// resolved; preExec runs while the per-repo serialisation lock is held
// (write/destructive tiers), so a peer's in-flight commit cannot slip between
// the check and this operation; postExec records the post-op observation after
// a successful run.
type gitRefGuard struct {
	repoRoot     string
	sessID       string
	sessName     string
	expectedHead string
	confirm      bool
	check        bool // write/destructive tier: compare baseline + enforce expected_head
	pre          gitRefObservation
	preSet       bool
	warning      string
}

// preExec enforces expected_head and the peer-moved-HEAD escalation against
// the repository's CURRENT ref state. expected_head fails closed — an
// unresolvable HEAD or expected revision refuses the operation. The peer check
// never blocks on what it cannot attribute: movement by this session, an
// external process, or an unknown mover adds no friction.
func (g *gitRefGuard) preExec(ctx context.Context, sub string) error {
	cur, headOK := observeGitRef(ctx, g.repoRoot)
	if g.expectedHead != "" {
		if err := g.checkExpectedHead(ctx, sub, cur, headOK); err != nil {
			return err
		}
	}
	g.pre, g.preSet = cur, true
	if g.sessID == "" {
		return nil
	}
	prev, mover, moved := gitRefStateFor(g.repoRoot).peerMove(g.sessID, cur)
	if !moved {
		return nil
	}
	detail := fmt.Sprintf("HEAD/branch moved since this session last observed it (was %s, now %s — moved by plumb session %q)",
		prev, cur, mover.name)
	if !g.confirm {
		return toolerror.Wrap(
			fmt.Errorf("git %s: %s. Re-check the repository state (git log/status), then re-run with confirm: true to proceed against the new state", sub, detail),
			toolerror.KindConcurrentRefMove, toolerror.ClassPassConfirm, toolerror.Retry())
	}
	g.warning = "# plumb-warning: " + detail + ". Proceeding against the new state because confirm: true was given — verify git log/status before building on this result.\n"
	return nil
}

// checkExpectedHead enforces the caller's expected_head assertion. Both
// refusals are KindConcurrentRefMove — the guard they belong to — but their
// remediation is fix_arguments, NOT pass_confirm: confirm deliberately does not
// bypass expected_head (preExec enforces it before the confirm-aware peer
// check), so advising confirm here would send a caller round a loop that cannot
// terminate. The remedy is to pass the current HEAD or drop the assertion.
func (g *gitRefGuard) checkExpectedHead(ctx context.Context, sub string, cur gitRefObservation, headOK bool) error {
	want, ok := resolveGitRev(ctx, g.repoRoot, g.expectedHead)
	if !ok {
		return expectedHeadRefusal(fmt.Errorf("git %s: expected_head %q does not resolve to a commit in this repository — refusing to run", sub, g.expectedHead))
	}
	if !headOK || cur.head != want {
		return expectedHeadRefusal(fmt.Errorf("git %s: expected_head mismatch: HEAD is at %s, but expected_head resolved to %.7s — refusing to run. "+
			"Re-check the repository state and retry with the current HEAD, or omit expected_head", sub, cur, want))
	}
	return nil
}

func expectedHeadRefusal(err error) error {
	return toolerror.Wrap(err, toolerror.KindConcurrentRefMove, toolerror.ClassFixArguments,
		toolerror.Retry())
}

// guardRefPreExec is runGit's nil-safe entry into the ref-movement guard: it
// hands the guard the resolved repository root and runs the pre-execution
// check for the tiers that carry one. A nil guard (no session identity, no
// expected_head) is a no-op.
func guardRefPreExec(ctx context.Context, g *gitRefGuard, repoRoot, sub string) error {
	if g == nil {
		return nil
	}
	g.repoRoot = repoRoot
	if !g.check {
		return nil
	}
	return g.preExec(ctx, sub)
}

// postExec records this session's observation of the post-operation ref state.
// The session becomes the attributable mover only when its own operation
// changed the state (pre != post): a write-tier op that left HEAD untouched
// (an `add`, a no-op stash) must not take credit for another actor's move.
// Observation failures are swallowed — the ledger is advisory and the next
// call simply re-baselines. Nil-receiver safe, so runGit can call it
// unconditionally.
func (g *gitRefGuard) postExec(ctx context.Context) {
	if g == nil || g.sessID == "" {
		return
	}
	obs, _ := observeGitRef(ctx, g.repoRoot)
	mutating := g.check && g.preSet && g.pre != obs
	gitRefStateFor(g.repoRoot).record(g.sessID, g.sessName, obs, mutating)
}
