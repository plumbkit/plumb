package tools

import (
	"context"
	"encoding/json"
	"fmt"
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
		"and pushing to an ad-hoc URL are always refused. " +
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
	argv, err := buildGitArgv(a)
	if err != nil {
		return "", err
	}
	// Serialise only index/ref-mutating tiers; a read (status/log/diff) must
	// never queue behind a slow commit.
	return runGit(ctx, a.Repo, a.Subcommand, argv, tier != tierRead)
}

// defaultRepo returns repo, or the session workspace when repo is empty.
// Keeps the git command targeted at the pinned project rather than the daemon's
// cwd (shared across connections, may belong to another repository). When the
// connection has no pinned workspace (WorkspaceFn nil or returning ""), the repo
// stays empty and checkBoundary refuses — fail closed, never fall through to the
// daemon cwd, which would run git against an unrelated repository.
func (t *Git) defaultRepo(repo string) string {
	if repo != "" || t.deps.WorkspaceFn == nil {
		return repo
	}
	return t.deps.WorkspaceFn()
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
