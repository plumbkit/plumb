package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var gitSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "subcommand": {
      "type": "string",
      "description": "Git subcommand to run. Read (always): diff, log, show, blame, status, shortlog, check-ignore, plus branch/tag/stash listing. Write (needs allow_writes, default on): add, commit, switch, mv, branch/tag create, stash push/pop. Destructive (needs allow_destructive + confirm): reset, clean, checkout, restore, rebase, revert, branch/tag delete, stash drop. Network (needs allow_push + confirm): push, fetch, pull."
    },
    "args": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Flags and arguments passed directly to git for all subcommands except add and commit. Examples: [\"--oneline\", \"-10\"] for log; [\"--cached\"] or [\"--staged\"] for diff (shows staged changes ready to commit); [\"--staged\"] for restore. Ignored when subcommand is \"add\" (use files) or \"commit\" (use message)."
    },
    "files": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Paths to act on. For subcommand \"add\": paths to stage (-A semantics — new, modified, and deleted entries all staged). For subcommand \"commit\": optional path-limited commit — commits ONLY these tracked paths (git commit -m <message> -- <files>), ignoring any unrelated staged changes already in the index; omit to commit the whole index. No glob expansion. Ignored by other subcommands."
    },
    "message": {
      "type": "string",
      "description": "Commit message — only used for subcommand \"commit\". Maps to -m; pre-commit hooks always run. Combine with files to commit only specific paths. Not used by any other subcommand."
    },
    "repo": {
      "type": "string",
      "description": "Path to any file or directory inside the repository. Omit to use the attached workspace; if no workspace is attached the call is refused (git never falls back to the daemon's working directory). To operate on a nested git submodule, set this to a path inside the submodule — git resolves to the submodule's own root, so add/commit land there; a command run against the superproject only records the submodule's commit pointer, never its file contents."
    },
    "confirm": {
      "type": "boolean",
      "description": "Required (true) for destructive and network subcommands. Acknowledges the operation may discard work or contact a remote."
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
// Concurrency: Execute is safe for concurrent use (no shared mutable state).
type Git struct {
	deps   WriteDeps
	policy GitPolicyFn
}

func NewGit(deps WriteDeps, policy GitPolicyFn) *Git {
	return &Git{deps: deps, policy: policy}
}

func (t *Git) Name() string                 { return "git" }
func (t *Git) InputSchema() json.RawMessage { return gitSchema }
func (t *Git) Description() string {
	return "Run git through one tiered, policy-gated tool (no shell). Read subcommands (status, log, diff, " +
		"show, blame, shortlog, branch/tag/stash listing) always run. " +
		"Write subcommands (add, commit, switch, mv, branch/tag create, stash push/pop) need [git] allow_writes (default on). " +
		"Destructive subcommands (reset, clean, checkout, restore, rebase, revert, branch/tag delete, stash drop) " +
		"need allow_destructive AND confirm:true. " +
		"Network subcommands (push, fetch, pull) need allow_push AND confirm:true; force-pushing a protected branch " +
		"(via -f/--force or a +refspec) and using an ad-hoc URL/remote (incl. any <transport>:: helper) on any network " +
		"subcommand are always refused — and a force push must name its destination branch (a bare -f or +HEAD that " +
		"relies on the current branch is refused, since it may target a protected branch). " +
		"Typed parameters: add uses files (staged with -A semantics — new/modified/deleted); commit uses message " +
		"(plus an optional files list for a path-limited commit, the safe way to commit just your change in a shared " +
		"worktree); every other subcommand uses args. " +
		"Essential for clients without shell access (Claude Desktop, Cursor MCP)."
}

type gitToolArgs struct {
	Subcommand string   `json:"subcommand"`
	Args       []string `json:"args"`
	Files      []string `json:"files"`
	Message    string   `json:"message"`
	Repo       string   `json:"repo"`
	Confirm    bool     `json:"confirm"`
}

func (a gitToolArgs) validate() error {
	if strings.TrimSpace(a.Subcommand) == "" {
		return fmt.Errorf("git: subcommand is required")
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
	if err := checkGitGlobalFlags(a.Args); err != nil {
		return "", err
	}
	tier := classifyGit(a.Subcommand, a.Args)
	if tier == tierReject {
		if a.Subcommand == "stash" && len(a.Args) > 0 {
			return "", fmt.Errorf("git stash: sub-command %q is not permitted; use list, show, push, pop, apply, drop, or clear", a.Args[0])
		}
		if a.Subcommand == "rm" {
			return "", fmt.Errorf("git: subcommand \"rm\" is not permitted; to remove a tracked file, use delete_file to remove it from disk, then stage the deletion with git add")
		}
		return "", fmt.Errorf("git: subcommand %q is not permitted", a.Subcommand)
	}
	policy := t.resolvePolicy()
	if err := gateGit(tier, policy, a.Confirm); err != nil {
		return "", err
	}
	if err := checkPushProtection(a, policy, tier); err != nil {
		return "", err
	}
	if tier != tierRead && !t.deps.Limiter.Allow() {
		return "", rateLimitError("git", t.deps.Limiter)
	}
	a.Repo = t.defaultRepo(a.Repo)
	if err := t.checkBoundary(a); err != nil {
		return "", err
	}
	return t.runGitCommand(ctx, a, tier, switchNote)
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
func (t *Git) runGitCommand(ctx context.Context, a gitToolArgs, tier gitTier, switchNote string) (string, error) {
	argv, err := buildGitArgv(a)
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
	out, err := runGit(ctx, a.Repo, a.Subcommand, argv, tier)
	if err != nil {
		return "", err
	}
	return switchNote + out + warning, nil
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
	newArgv, err = buildGitArgv(filtered)
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
func (t *Git) defaultRepo(repo string) string {
	if repo == "" {
		if t.deps.WorkspaceFn == nil {
			return ""
		}
		return t.deps.WorkspaceFn()
	}
	return t.deps.resolvePath(repo)
}

func (t *Git) checkBoundary(a gitToolArgs) error {
	// A resolved repo is mandatory. An empty repo here means neither an explicit
	// "repo" arg nor a pinned workspace was available; running git anyway would
	// fall through to the daemon's cwd (a different connection's project — a
	// cross-session isolation leak), so refuse instead.
	if a.Repo == "" {
		return fmt.Errorf("git: no repository resolved — call session_start to attach a workspace, or pass an explicit \"repo\". " +
			"If this session was working a moment ago, the daemon may have restarted (e.g. after a rebuild or upgrade), which clears the per-connection workspace pin — re-run session_start to re-attach")
	}
	if err := t.deps.checkBoundary(a.Repo); err != nil {
		return fmt.Errorf("git: %w", err)
	}
	for _, f := range a.Files {
		path := f
		if !filepath.IsAbs(path) && a.Repo != "" {
			path = filepath.Join(a.Repo, path)
		}
		if err := t.deps.checkBoundary(path); err != nil {
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
// counts) or when it exists on disk (os.Stat succeeds — covers new untracked
// files and existing directories). Only meaningful for the "add" subcommand.
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
	lsArgs := append([]string{"ls-files", "--"}, a.Files...)
	cmd := exec.CommandContext(ctx, "git", lsArgs...)
	cmd.Dir = repoRoot
	out, lsErr := cmd.Output()
	if lsErr != nil {
		return a.Files, nil
	}
	tracked := map[string]bool{}
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		tracked[filepath.Clean(filepath.Join(repoRoot, line))] = true
	}
	for _, f := range a.Files {
		abs := f
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(a.Repo, f)
		}
		abs = filepath.Clean(abs)
		if tracked[abs] {
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
