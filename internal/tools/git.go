package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/plumbkit/plumb/internal/collab"
)

var gitSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "subcommand": {
      "type": "string",
      "description": "Git subcommand. Read: diff, log, show, blame, status, shortlog, check-ignore, branch/tag/stash listing. Write: add, commit, switch, mv, merge, branch/tag create, stash push/pop. Destructive: reset, clean, checkout, restore, rebase, revert, cherry-pick, merge --abort/--quit, branch/tag delete, stash drop. Network: push, fetch, pull."
    },
    "args": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Arguments passed to git, e.g. [\"--oneline\",\"-10\"] for log or [\"--staged\"] for diff. Not for add or commit."
    },
    "files": {
      "type": "array",
      "items": {"type": "string"},
      "description": "add: paths to stage (-A semantics). commit: commit ONLY these tracked paths, ignoring other staged changes; omit for the whole index. No globs."
    },
    "message": {
      "type": "string",
      "description": "Commit message (-m); pre-commit hooks always run."
    },
    "repo": {
      "type": "string",
      "description": "A path inside the target repository (default: the attached workspace; refused if none, never the daemon's directory). For a submodule, pass a path inside it; run from the superproject, git records only its pointer."
    },
    "confirm": {
      "type": "boolean",
      "description": "Required for destructive and network subcommands, and to override the cross-session HEAD guard."
    },
    "expected_head": {
      "type": "string",
      "description": "Revision HEAD must resolve to, or a write, destructive or network op is refused."
    }
  },
  "required": ["subcommand"],
  "additionalProperties": false
}`)

// Git runs git through a single tiered interface: read subcommands always run;
// write, destructive, and network subcommands are gated by the resolved
// GitPolicy. The subcommand always leads the argv, so global flags supplied in
// args cannot reconfigure git; there is no shell.
//
// The tool is split across files by concern: tier classification + the global
// flag denylist live in git_classify.go; the gating policy and push protection
// in git_policy.go; argv assembly, execution, and output formatting in
// git_exec.go. This file holds the MCP Tool surface and request orchestration.
//
// Concurrency: Execute is safe for concurrent use. sessID/sessNameFn/sessNameForFn
// are set once at registration (WithSession/WithSessionNameFor); the
// cross-session ledger itself lives in the process-global gitRefStates map
// (git_ref_guard.go).
type Git struct {
	deps          WriteDeps
	policy        GitPolicyFn
	sessID        func() string
	sessNameFn    func() string
	sessNameForFn func(ctx context.Context) string
	// Peer repo-intent warning wiring (git_intent_warn.go), all nil-safe and
	// consulted lazily per call: unwired means no warning is ever computed.
	// hintBudgetBytes is the [collab] hint_budget_bytes snapshot the rendered
	// warning is clamped to, matching every other injected peer-signal block.
	intentsOn       func() bool
	collabStore     func() *collab.Store
	hintBudgetBytes func() int
	// projectGit is the session's captured project [git] request (nil-safe),
	// read only to explain a tier refusal (git_untrusted_note.go).
	projectGit func() ProjectGitStatus
}

func NewGit(deps WriteDeps, policy GitPolicyFn) *Git {
	return &Git{deps: deps, policy: policy}
}

// WithSession wires the connection's session identity for the cross-session
// ref-movement guard (git_ref_guard.go). Returns the receiver for chaining.
// Without it the ledger is untracked and only expected_head is enforced.
func (t *Git) WithSession(id func() string, name func() string) *Git {
	t.sessID = id
	t.sessNameFn = name
	return t
}

// WithSessionNameFor wires a per-call session name resolver for multi-agent
// connections.
func (t *Git) WithSessionNameFor(fn func(ctx context.Context) string) *Git {
	t.sessNameForFn = fn
	return t
}

// WithPeerIntents wires the repo-level peer-intent warning
// (git_intent_warn.go): before a repo-state verb runs, live peer intents
// covering the repository are surfaced in the response as an advisory warning.
// on is the [collab] intents snapshot; store opens the workspace's collab.db
// ONLY when it already exists (a git call never creates one); hintBudgetBytes
// is the [collab] hint_budget_bytes snapshot the rendered warning is clamped
// to, the same budget every other injected peer-signal block shares. All
// three are nil-safe: unwired means no warnings. Returns the receiver for
// chaining.
func (t *Git) WithPeerIntents(on func() bool, store func() *collab.Store, hintBudgetBytes func() int) *Git {
	t.intentsOn = on
	t.collabStore = store
	t.hintBudgetBytes = hintBudgetBytes
	return t
}

// WithProjectPolicy wires the session's captured project-config [git] request
// — the same snapshot session_start's git-policy notice reads — so a tier
// refusal can say when an untrusted project config is why the tier is off.
// Returns the receiver for chaining.
func (t *Git) WithProjectPolicy(fn func() ProjectGitStatus) *Git {
	t.projectGit = fn
	return t
}

func (t *Git) projectGitStatus() ProjectGitStatus {
	if t.projectGit == nil {
		return ProjectGitStatus{}
	}
	return t.projectGit()
}

func (t *Git) Name() string                 { return "git" }
func (t *Git) InputSchema() json.RawMessage { return gitSchema }
func (t *Git) Description() string {
	return "Policy-gated git, no shell. Reads always run. Writes need [git] allow_writes (default on). Destructive subcommands need allow_destructive and confirm:true; push, fetch and pull need allow_push and confirm:true. Force-pushing a protected branch or using an ad-hoc remote is always refused. add (-A) and commit (message, optional files) are typed; others take args. A write is refused if a DIFFERENT session moved HEAD since you looked (confirm:true overrides). Details: the plumb-git skill."
}

type gitToolArgs struct {
	Subcommand   string   `json:"subcommand"`
	Args         []string `json:"args"`
	Files        []string `json:"files"`
	Message      string   `json:"message"`
	Repo         string   `json:"repo"`
	Confirm      bool     `json:"confirm"`
	ExpectedHead string   `json:"expected_head"`
}

func (a gitToolArgs) validate() error {
	if strings.TrimSpace(a.Subcommand) == "" {
		return errors.New("git: subcommand is required")
	}
	return nil
}

func (t *Git) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	a, err := parseGitArgs(raw)
	if err != nil {
		return "", err
	}
	if err := a.validate(); err != nil {
		return "", err
	}
	// Accept `git switch -c/-C` by rewriting to --create/--force-create before the
	// global-flag denylist (which otherwise refuses the colliding -c/-C).
	newArgs, switchNote := normaliseSwitchCreate(a.Subcommand, a.Args)
	a.Args = newArgs
	tier, err := classifyGitCall(a)
	if err != nil {
		return "", err
	}
	tier = t.refineTier(ctx, a, tier) // git_ref_reset.go: creating a NEW ref is a write
	policy := t.resolvePolicy()
	if err := t.gate(tier, policy, a.Confirm); err != nil {
		return "", err
	}
	if err := checkPushProtection(a, policy, tier); err != nil {
		return "", err
	}
	if tier != tierRead && !t.deps.limiter(ctx).Allow() {
		return "", rateLimitError("git", t.deps.limiter(ctx))
	}
	a.Repo, err = t.defaultRepo(ctx, a.Repo)
	if err != nil {
		return "", err
	}
	if err := t.checkBoundary(ctx, a); err != nil {
		return "", err
	}
	return t.runGitCommand(ctx, a, tier, switchNote, t.commitTrailerToken(ctx, policy, a.Subcommand), gitChildSpecFor(policy))
}

// gate applies the tier policy (gateGit), adding the untrusted-project note
// when an untrusted project config is why the tier is off.
func (t *Git) gate(tier gitTier, policy GitPolicy, confirm bool) error {
	err := gateGit(tier, policy, confirm)
	if err == nil {
		return nil
	}
	if note := untrustedTierNote(t.projectGitStatus(), tier, policy); note != "" {
		// A note means the tier's switch is off, so err is the disabled refusal.
		return policyDisabled(err.Error() + note)
	}
	return err
}

// commitTrailerToken returns the `Plumb-Session: <session-name>` trailer to
// stamp on this call's commit, or "" when the call is not a commit, the [git]
// commit_trailer knob is off (the default), the connection has no session
// name to attribute, or the name fails the newline/colon guard below. The
// trailer only ever ADDS metadata to a commit that was
// going to happen anyway — it never blocks a commit on its own account. But
// the token this returns feeds straight into a `--trailer` argument
// (buildGitArgv), and `git commit --trailer` is itself gated on the git
// binary: the flag does not exist before git 2.32 (June 2021), so turning
// commit_trailer on against an older git makes EVERY commit issued through
// this tool fail with "error: unknown option 'trailer'" — plumb runs no
// runtime version probe to catch that ahead of time. See the git ≥ 2.32
// requirement on the [git] commit_trailer row in docs/configuration.md.
func (t *Git) commitTrailerToken(ctx context.Context, p GitPolicy, sub string) string {
	if sub != "commit" || !p.CommitTrailer {
		return ""
	}
	// The per-call resolver's answer is final, an empty one included. Falling back
	// to the connection's name when it said "" stamped a subagent's commit with its
	// PARENT's name — the one thing a trailer exists to get right — so an agent that
	// has no name of its own signs nothing rather than signing as someone else
	// (#556). Only a connection that wired no per-call resolver uses the fallback.
	var name string
	switch {
	case t.sessNameForFn != nil:
		name = strings.TrimSpace(t.sessNameForFn(ctx))
	case t.sessNameFn != nil:
		name = strings.TrimSpace(t.sessNameFn())
	}
	if name == "" {
		return ""
	}
	// Defence in depth: session.NormaliseName restricts a stored session name
	// to [A-Za-z0-9-]+ (max 25 chars, no leading/trailing/consecutive
	// hyphens), and generated names come from a fixed adjective-noun word
	// list, so neither can carry a newline or colon today. This callsite has
	// no way to see that invariant hold, only to trust it — so guard here
	// too rather than build a malformed or multi-line trailer if it ever
	// loosens: skip the trailer instead of emitting one.
	if strings.ContainsAny(name, "\n:") {
		return ""
	}
	return "Plumb-Session: " + name
}

// runGitCommand builds argv (filtering unmatched "add" paths, see
// resolveAddArgv), runs it, and assembles the final output. Split out of
// Execute purely to keep Execute's own cyclomatic complexity within the
// project's gocyclo-15 contract — the request-orchestration steps above
// (parsing, tier classification, gating, boundary checks) stay in Execute;
// this is the argv-to-output tail.
//
// runGit serialises every non-read tier; a read (status/log/diff) must never
// queue behind a slow commit.
func (t *Git) runGitCommand(ctx context.Context, a gitToolArgs, tier gitTier, switchNote, trailer string, child gitChildSpec) (string, error) {
	argv, err := buildGitArgv(a, trailer)
	if err != nil {
		return "", err
	}
	// `git add -A -- <files>` hard-fails the WHOLE command the instant ANY
	// listed pathspec is unmatched (see resolveAddArgv), so a typo'd path must
	// be filtered out before staging, not merely warned about after the fact.
	var warning string
	if a.Subcommand == "add" {
		var shortCircuit string
		var done bool
		if argv, warning, shortCircuit, done, err = t.resolveAddArgv(ctx, a, argv); err != nil {
			return "", err
		}
		if done {
			return switchNote + shortCircuit, nil
		}
	}
	guard := t.armRefGuard(a, tier)
	out, err := runGit(ctx, a.Repo, a.Subcommand, argv, tier, guard, t.peerIntentWarnFn(a.Subcommand, tier), child, t.deps.writes(ctx), t.sessionKey())
	if err != nil {
		return "", err
	}
	if guard != nil && guard.warning != "" {
		return guard.warning + switchNote + out + warning, nil
	}
	return switchNote + out + warning, nil
}

// sessionKey is the calling session's identity for the once-per-session report
// of a finished background git op (git_background.go); "" when unwired, which
// makes every identity-less caller one reader.
func (t *Git) sessionKey() string {
	if t.sessID == nil {
		return ""
	}
	return t.sessID()
}

// armRefGuard builds the per-call ref-movement guard (git_ref_guard.go). It
// returns nil — the zero-overhead path — only when the call has neither a
// session identity to track nor an expected_head to enforce. The guard checks
// the write, destructive, and network tiers before they run — network included
// because a force-push is the highest blast-radius operation the tool mediates
// — and records this session's HEAD/branch observation after every successful
// call (reads included), which is what keeps single-session use friction-free:
// a session's own moves are always its latest observation.
func (t *Git) armRefGuard(a gitToolArgs, tier gitTier) *gitRefGuard {
	sessID := ""
	if t.sessID != nil {
		sessID = t.sessID()
	}
	if sessID == "" && a.ExpectedHead == "" {
		return nil
	}
	g := &gitRefGuard{
		sessID:       sessID,
		expectedHead: a.ExpectedHead,
		confirm:      a.Confirm,
		check:        tier == tierWrite || tier == tierDestructive || tier == tierNetwork,
	}
	if t.sessNameFn != nil {
		g.sessName = t.sessNameFn()
	}
	return g
}

// resolveAddArgv adjusts argv for the "add" subcommand to exclude any
// unmatched (typo'd) paths (see partitionAddPaths), and computes the warning
// to append to the eventual output. `git add -A -- <files>` hard-fails the
// WHOLE command (exit 128, "did not match any files") the instant ANY listed
// pathspec matches neither a working-tree entry nor an index entry — even
// under -A, and even mixed with otherwise-valid paths (verified against git
// 2.55: unmatched + valid together still aborts with nothing staged at all)
// — so the unmatched paths must never reach the real git add call.
//
// When every requested path is unmatched there is nothing left to stage:
// done is true and shortCircuit is the complete result to return directly,
// skipping the git invocation entirely (running `git add -A --` with an
// empty pathspec list would change meaning completely — it stages the WHOLE
// working tree, not nothing).
func (t *Git) resolveAddArgv(ctx context.Context, a gitToolArgs, argv []string) (newArgv []string, warning, shortCircuit string, done bool, err error) {
	valid, unmatched := t.partitionAddPaths(ctx, a)
	if len(unmatched) == 0 {
		return argv, "", "", false, nil
	}
	warning = fmt.Sprintf(
		"\n\nwarning: no working-tree or index entry for: %s (skipped — check for a typo)",
		strings.Join(unmatched, ", "),
	)
	if len(valid) == 0 {
		return nil, warning, "nothing staged" + warning, true, nil
	}
	filtered := a
	filtered.Files = valid
	newArgv, err = buildGitArgv(filtered, "")
	if err != nil {
		return nil, "", "", false, err
	}
	return newArgv, warning, "", false, nil
}

// defaultRepo resolves the repo argument against the pinned workspace: an empty
// repo becomes the workspace root, and a RELATIVE repo (a bare filename such as
// "README.md", or "sub/dir") is anchored to the workspace like every other
// path-bearing argument. Left relative it would reach checkBoundary, which
// canonicalises through filepath.Abs against the daemon's cwd — a directory that
// belongs to no project — so a correctly-pinned session saw a spurious boundary
// violation for a file sitting in its own workspace root.
//
// Keeps the git command targeted at the pinned project rather than the daemon's
// cwd (shared across connections, may belong to another repository). When the
// connection has no pinned workspace (WorkspaceFn nil or returning ""), an empty
// repo stays empty and checkBoundary refuses — fail closed, never fall through to
// the daemon cwd, which would run git against an unrelated repository.
func (t *Git) defaultRepo(ctx context.Context, repo string) (string, error) {
	if repo == "" {
		if t.deps.Contested != nil && t.deps.Contested() {
			return "", errors.New(gitContestedNoRepo)
		}
		if t.deps.WorkspaceFn == nil {
			return "", nil
		}
		return t.deps.WorkspaceFn(ctx), nil
	}
	return t.deps.resolvePath(ctx, repo)
}

// gitContestedNoRepo is the refusal for a git call with no explicit `repo` on a
// connection whose pin is contested. An empty repo would fall back to the pinned
// workspace, which is exactly the root being fought over, so the call is refused
// rather than aimed at whichever project holds the pin right now.
const gitContestedNoRepo = "git: this connection's workspace pin is contested (several agents are multiplexing this plumb serve without declaring an identity), so a git call without an explicit `repo` cannot be attributed to a project and is refused rather than aimed at whichever project holds the pin right now. Pass an absolute `repo` path to name the repository, or identify the agents: " + PerCallIdentityRemedy

func (t *Git) checkBoundary(ctx context.Context, a gitToolArgs) error {
	// A resolved repo is mandatory. An empty repo here means neither an explicit
	// "repo" arg nor a pinned workspace was available; running git anyway would
	// fall through to the daemon's cwd (a different connection's project — a
	// cross-session isolation leak), so refuse instead.
	if a.Repo == "" {
		return errors.New("git: no repository resolved — call session_start to attach a workspace, or pass an explicit \"repo\". " +
			"If this session was working a moment ago, the daemon may have restarted (e.g. after a rebuild or upgrade), which clears the per-connection workspace pin — re-run session_start to re-attach")
	}
	if err := t.deps.checkBoundary(ctx, a.Repo); err != nil {
		return fmt.Errorf("git: %w", err)
	}
	for _, f := range a.Files {
		path := f
		if !filepath.IsAbs(path) && a.Repo != "" {
			path = filepath.Join(a.Repo, path)
		}
		if err := t.deps.checkBoundary(ctx, path); err != nil {
			return fmt.Errorf("git: %w", err)
		}
	}
	return nil
}

// partitionAddPaths splits a.Files into paths that match a git index entry or
// a working-tree entry ("valid") and paths that match neither ("unmatched" —
// almost always a typo). A path counts as matched when it is tracked (`git
// ls-files` reports index content regardless of working-tree state, so a
// tracked file deleted from disk but not yet staged as a deletion still
// counts), when it is a directory holding tracked content (see
// recordTrackedDirs — ls-files never prints a directory itself), or when it
// exists on disk (os.Stat succeeds — covers new untracked files and empty or
// untracked directories). Only meaningful for the "add" subcommand.
//
// Costs at most one extra git invocation: a single batched `git ls-files --
// <files>` (unlike `add`, ls-files does not hard-fail on an unmatched
// pathspec — it simply omits it from the output) run with the same cmd.Dir
// (the resolved repo root) that the real `git add -A -- <files>` call will
// later use via runGit, so pathspec resolution is identical between the
// precheck and the real add. Any failure resolving the repo root or running
// ls-files is treated as "could not precheck" and every path is reported
// valid — a failed precheck must never cause a path that git would actually
// have staged to be silently dropped from the real add call.
func (t *Git) partitionAddPaths(ctx context.Context, a gitToolArgs) (valid, unmatched []string) {
	repoRoot, err := findGitRoot(a.Repo)
	if err != nil {
		return a.Files, nil
	}
	canonicalRoot, canonicalFiles, pathspecs := canonicalAddPaths(repoRoot, a.Files)
	lsArgs := gitReadArgv(append([]string{"ls-files", "--"}, pathspecs...))
	cmd := exec.CommandContext(ctx, "git", lsArgs...)
	cmd.Dir = repoRoot
	out, lsErr := cmd.Output()
	if lsErr != nil {
		return a.Files, nil
	}
	tracked := map[string]bool{}
	trackedDirs := map[string]bool{}
	for line := range strings.SplitSeq(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		abs := filepath.Clean(filepath.Join(canonicalRoot, line))
		tracked[abs] = true
		recordTrackedDirs(trackedDirs, abs, canonicalRoot)
	}
	for i, f := range a.Files {
		// Relative inputs resolve against the git toplevel, not a.Repo, which may
		// be a subdirectory. canonicalRoot also resolves an existing parent when the
		// leaf was deleted, so macOS /var and /private/var aliases compare equal to
		// git ls-files output.
		abs := canonicalFiles[i]
		if tracked[abs] || trackedDirs[abs] {
			valid = append(valid, f)
			continue
		}
		if _, statErr := os.Stat(abs); statErr == nil {
			valid = append(valid, f)
			continue
		}
		unmatched = append(unmatched, f)
	}
	return valid, unmatched
}

// recordTrackedDirs marks every ancestor of file, up to and including root, as
// holding tracked content — so a DIRECTORY pathspec can be matched.
//
// `git ls-files -- somedir` prints the files under somedir, never somedir
// itself, so a directory can never be a key in the tracked-FILE set. That left
// os.Stat as the only clause matching a directory, which in turn matched it
// only while it still existed. The uncovered intersection — directory AND
// deleted, exactly what a bulk `rm -rf` produces — reported a real, stageable
// tree of deletions as an unmatched typo, and resolveAddArgv then
// short-circuited without invoking git at all. `git add -A -- <dir>` stages
// those deletions perfectly well; they simply never reached it.
func recordTrackedDirs(trackedDirs map[string]bool, file, root string) {
	root = filepath.Clean(root)
	for dir := filepath.Dir(file); dirWithinRoot(dir, root); {
		if trackedDirs[dir] {
			return // this ancestor chain is already recorded
		}
		trackedDirs[dir] = true
		if dir == root {
			return
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return // reached the filesystem root; cannot ascend further
		}
		dir = parent
	}
}

// dirWithinRoot reports whether dir is root or lies beneath it, comparing whole
// path SEGMENTS. A raw strings.HasPrefix would admit a sibling that merely
// shares a textual prefix (root "/repo" vs dir "/repository"), and would drop
// the root itself when root carried a trailing separator — the exact pitfall
// class boundary.go documents.
func dirWithinRoot(dir, root string) bool {
	if dir == root {
		return true
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func canonicalAddPaths(repoRoot string, files []string) (string, []string, []string) {
	// Named root, not canonicalRoot: the local would shadow the shared
	// canonicaliser this function calls.
	root := canonicalRoot(repoRoot)

	canonicalFiles := make([]string, len(files))
	pathspecs := make([]string, 0, len(files))
	for i, f := range files {
		abs := f
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(root, f)
		}
		canonicalFiles[i] = canonicalRoot(abs)

		if !filepath.IsAbs(f) {
			pathspecs = append(pathspecs, f)
			continue
		}
		rel, err := filepath.Rel(root, canonicalFiles[i])
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			pathspecs = append(pathspecs, rel)
		}
	}
	return root, canonicalFiles, pathspecs
}

func parseGitArgs(raw json.RawMessage) (gitToolArgs, error) {
	var a gitToolArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("git: invalid arguments: %w", err)
	}
	return a, nil
}

func (t *Git) resolvePolicy() GitPolicy {
	if t.policy == nil {
		return GitPolicy{AllowWrites: true}
	}
	return t.policy()
}
