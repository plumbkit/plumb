package tools

import (
	"bytes"
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
      "description": "Git subcommand to run. Read (always): diff, log, show, blame, status, shortlog, plus branch/tag/stash listing. Write (needs allow_writes, default on): add, commit, switch, mv, branch/tag create, stash push/pop. Destructive (needs allow_destructive + confirm): reset, clean, checkout, restore, rebase, revert, branch/tag delete, stash drop. Network (needs allow_push + confirm): push, fetch, pull."
    },
    "args": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Flags and arguments passed directly to git for all subcommands except add and commit, e.g. [\"--oneline\", \"-10\"] for log or [\"--staged\"] for restore. Ignored when subcommand is \"add\" (use files) or \"commit\" (use message)."
    },
    "files": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Paths to stage — only used for subcommand \"add\". Uses -A semantics: new, modified, and deleted entries are all staged for each path. No glob expansion. Not used by any other subcommand."
    },
    "message": {
      "type": "string",
      "description": "Commit message — only used for subcommand \"commit\". Maps to -m; pre-commit hooks always run. Not used by any other subcommand."
    },
    "repo": {
      "type": "string",
      "description": "Path to any file or directory inside the repository. Omit to use the current working directory."
    },
    "confirm": {
      "type": "boolean",
      "description": "Required (true) for destructive and network subcommands. Acknowledges the operation may discard work or contact a remote."
    }
  },
  "required": ["subcommand"],
  "additionalProperties": false
}`)

const maxGitBytes = 100 * 1024 // 100 KiB

// GitPolicy is the resolved per-connection gating policy for the git tool.
// It mirrors the [git] config section but lives in the tools package so the
// package keeps its no-import-config boundary (the daemon adapts config →
// policy via GitPolicyFn).
type GitPolicy struct {
	AllowWrites       bool
	AllowDestructive  bool
	AllowPush         bool
	ProtectedBranches []string
}

// GitPolicyFn resolves the current GitPolicy at call time. nil falls back to a
// safe default (writes on, destructive and network off).
type GitPolicyFn = func() GitPolicy

// gitTier classifies a git invocation by blast radius. Higher tiers require
// more permission.
type gitTier int

const (
	tierReject gitTier = iota
	tierRead
	tierWrite
	tierDestructive
	tierNetwork
)

// dangerousGitGlobalFlags are never accepted in args. They are inert when they
// follow the subcommand (git interprets them per-subcommand), but rejecting
// them is defence-in-depth against any future code path that prepends args, and
// blocks the upload-pack/receive-pack remote-helper RCE vectors on push/fetch.
// -c and -C are included because some subcommands pass unknown flags up to git's
// global parser, making -c <key>=<val> a live config-injection vector there.
var dangerousGitGlobalFlags = map[string]bool{
	"-c":             true,
	"-C":             true,
	"--exec-path":    true,
	"--git-dir":      true,
	"--work-tree":    true,
	"--namespace":    true,
	"--upload-pack":  true,
	"--receive-pack": true,
}

// Git runs git through a single tiered interface: read subcommands always run;
// write, destructive, and network subcommands are gated by the resolved
// GitPolicy. The subcommand always leads the argv, so global flags supplied in
// args cannot reconfigure git; there is no shell.
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
	return "Run git through one tiered tool. Read subcommands (status, log, diff, show, blame, shortlog, branch/tag/stash listing) always run. " +
		"Write subcommands (add, commit, switch, mv, branch/tag create, stash push/pop) run when [git] allow_writes is enabled (default on). " +
		"Destructive subcommands (reset, clean, checkout, restore, rebase, revert, branch/tag delete, stash drop) require [git] allow_destructive AND confirm:true. " +
		"Network subcommands (push, fetch, pull) require [git] allow_push AND confirm:true; force-pushing a protected branch is always refused, as is pushing to an ad-hoc URL. " +
		"Typed-parameter contract: add uses files (staged with -A semantics — new, modified, and deleted entries all staged); commit uses message; all other subcommands use args. " +
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
	return runGit(ctx, a.Repo, a.Subcommand, argv)
}

// defaultRepo returns repo, or the session workspace when repo is empty.
// Keeps the git command targeted at the pinned project rather than the
// daemon's cwd (shared across connections, may belong to another repository).
func (t *Git) defaultRepo(repo string) string {
	if repo != "" || t.deps.WorkspaceFn == nil {
		return repo
	}
	return t.deps.WorkspaceFn()
}

func (t *Git) checkBoundary(a gitToolArgs) error {
	if a.Repo != "" {
		if err := t.deps.checkBoundary(a.Repo); err != nil {
			return fmt.Errorf("git: %w", err)
		}
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

// checkGitGlobalFlags rejects args that name an unconditionally-global,
// never-legitimate-as-a-subcommand-flag option (both bare and key=value forms).
func checkGitGlobalFlags(args []string) error {
	for _, a := range args {
		name := a
		if i := strings.IndexByte(a, '='); i >= 0 {
			name = a[:i]
		}
		if dangerousGitGlobalFlags[name] {
			return fmt.Errorf("git: flag %q is not permitted", name)
		}
	}
	return nil
}

// classifyGit maps a subcommand + args to a tier. Ambiguous subcommands
// (branch, tag, stash, checkout, switch, restore) inspect their args; the
// classification is safe-biased — when in doubt it returns the higher tier.
func classifyGit(sub string, args []string) gitTier {
	switch sub {
	case "diff", "log", "show", "blame", "status", "shortlog":
		return tierRead
	case "add", "commit", "mv":
		return tierWrite
	case "switch":
		return classifySwitch(args)
	case "restore":
		return classifyRestore(args)
	case "branch":
		return classifyBranch(args)
	case "tag":
		return classifyTag(args)
	case "stash":
		return classifyStash(args)
	case "checkout":
		return classifyCheckout(args)
	case "reset", "clean", "rebase", "revert":
		return tierDestructive
	case "push", "fetch", "pull":
		return tierNetwork
	case "rm":
		return tierReject
	default:
		return tierReject
	}
}

func classifySwitch(args []string) gitTier {
	if hasAnyFlag(args, "-f", "--force", "--discard-changes") {
		return tierDestructive
	}
	return tierWrite
}

// classifyRestore: `restore --staged <path>` only touches the index (safe to
// treat as a write); any form that touches the working tree discards changes.
func classifyRestore(args []string) gitTier {
	staged := hasAnyFlag(args, "--staged", "-S")
	worktree := hasAnyFlag(args, "--worktree", "-W")
	if staged && !worktree {
		return tierWrite
	}
	return tierDestructive
}

func classifyBranch(args []string) gitTier {
	if hasAnyFlag(args, "-d", "-D", "--delete") {
		return tierDestructive
	}
	if hasAnyFlag(args, "-m", "-M", "--move", "-c", "-C", "--copy") {
		return tierWrite
	}
	if hasAnyFlag(args, "--list", "-l", "-a", "--all", "-r", "--remotes",
		"-v", "-vv", "--show-current", "--contains", "--merged", "--no-merged") {
		return tierRead
	}
	if hasNonFlagArg(args) {
		return tierWrite // creating a branch
	}
	return tierRead
}

func classifyTag(args []string) gitTier {
	if hasAnyFlag(args, "-d", "--delete") {
		return tierDestructive
	}
	if hasAnyFlag(args, "-l", "--list", "-n", "--contains", "--merged") {
		return tierRead
	}
	if hasNonFlagArg(args) {
		return tierWrite // creating a tag
	}
	return tierRead
}

func classifyStash(args []string) gitTier {
	if len(args) == 0 {
		return tierWrite // bare `git stash` pushes (mutates working tree)
	}
	switch args[0] {
	case "list", "show":
		return tierRead
	case "push", "save", "pop", "apply", "create", "store":
		return tierWrite
	case "drop", "clear":
		return tierDestructive
	default:
		return tierReject // unknown stash sub-subcommand; caller reports "not permitted"
	}
}

// classifyCheckout treats only pure branch creation (-b/-B) as a write; every
// other checkout form can discard the working tree or detach HEAD, so it is
// destructive. Prefer `switch` for safe branch changes.
func classifyCheckout(args []string) gitTier {
	if len(args) > 0 && (args[0] == "-b" || args[0] == "-B") {
		return tierWrite
	}
	return tierDestructive
}

func gateGit(tier gitTier, p GitPolicy, confirm bool) error {
	switch tier {
	case tierRead:
		return nil
	case tierWrite:
		if !p.AllowWrites {
			return fmt.Errorf("git: write operations are disabled; set [git] allow_writes = true to enable")
		}
		return nil
	case tierDestructive:
		if !p.AllowDestructive {
			return fmt.Errorf("git: destructive operations are disabled; set [git] allow_destructive = true to enable")
		}
		if !confirm {
			return fmt.Errorf("git: this destructive operation requires confirm: true")
		}
		return nil
	case tierNetwork:
		if !p.AllowPush {
			return fmt.Errorf("git: network operations (push/fetch/pull) are disabled; set [git] allow_push = true to enable")
		}
		if !confirm {
			return fmt.Errorf("git: this network operation requires confirm: true")
		}
		return nil
	default:
		return fmt.Errorf("git: subcommand is not permitted")
	}
}

// checkPushProtection refuses pushing to an ad-hoc URL and force-pushing any
// protected branch, regardless of confirm.
func checkPushProtection(a gitToolArgs, p GitPolicy, tier gitTier) error {
	if tier != tierNetwork || a.Subcommand != "push" {
		return nil
	}
	for _, arg := range a.Args {
		if looksLikeGitURL(arg) {
			return fmt.Errorf("git push: pushing to an ad-hoc URL is not permitted; use a named remote")
		}
	}
	if !hasForceFlag(a.Args) {
		return nil
	}
	for _, arg := range a.Args {
		if isProtectedBranch(arg, p.ProtectedBranches) {
			return fmt.Errorf("git push: force-pushing protected branch %q is not permitted", arg)
		}
	}
	return nil
}

func hasForceFlag(args []string) bool {
	for _, a := range args {
		if a == "-f" || strings.HasPrefix(a, "--force") {
			return true
		}
	}
	return false
}

func looksLikeGitURL(s string) bool {
	if strings.Contains(s, "://") || strings.HasPrefix(s, "ext::") {
		return true
	}
	at := strings.IndexByte(s, '@')
	colon := strings.IndexByte(s, ':')
	return at >= 0 && colon > at // scp-like user@host:path
}

func isProtectedBranch(arg string, protected []string) bool {
	if strings.HasPrefix(arg, "-") {
		return false
	}
	for _, part := range strings.Split(arg, ":") {
		name := strings.TrimPrefix(part, "+")
		name = strings.TrimPrefix(name, "refs/heads/")
		for _, pb := range protected {
			if name == pb {
				return true
			}
		}
	}
	return false
}

// buildGitArgv assembles the full git argv. add and commit use typed params so
// free-form path args and commit footguns (-F, editor, --no-verify, --amend)
// are unreachable; all other subcommands pass args through.
func buildGitArgv(a gitToolArgs) ([]string, error) {
	switch a.Subcommand {
	case "commit":
		if strings.TrimSpace(a.Message) == "" {
			return nil, fmt.Errorf("git commit: message is required")
		}
		return []string{"commit", "-m", a.Message}, nil
	case "add":
		if len(a.Files) == 0 {
			return nil, fmt.Errorf("git add: at least one path is required (use the files parameter)")
		}
		return append([]string{"add", "-A", "--"}, a.Files...), nil
	default:
		return append([]string{a.Subcommand}, a.Args...), nil
	}
}

func runGit(ctx context.Context, repo, sub string, argv []string) (string, error) {
	repoRoot, err := findGitRoot(repo)
	if err != nil {
		return "", fmt.Errorf("git: %w", err)
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", argv...)
	cmd.Dir = repoRoot
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", sub, msg)
	}
	out := stdout.String()
	if strings.TrimSpace(out) == "" {
		out = stderr.String() // switch/push and friends report on stderr
	}
	return postProcessGit(ctx, repoRoot, sub, out)
}

// postProcessGit replaces the raw output of add/commit with the concise
// feedback the dedicated tools used to provide.
func postProcessGit(ctx context.Context, repoRoot, sub, out string) (string, error) {
	switch sub {
	case "add":
		return stagedSummary(ctx, repoRoot)
	case "commit":
		if res, err := resolveCommitInfo(ctx, repoRoot); err == nil {
			return formatGitCommitResult(res), nil
		}
	}
	return formatGitOutput(sub, out), nil
}

func formatGitOutput(sub, result string) string {
	const maxLogLines = 200
	if sub == "log" || sub == "blame" {
		result = truncateLines(result, maxLogLines,
			fmt.Sprintf("… (showing first %d lines — add --oneline / -n N to narrow, or use args to filter)", maxLogLines))
	}
	if len(result) > maxGitBytes {
		result = result[:maxGitBytes] + "\n… (output truncated at 100 KiB)"
	}
	if strings.TrimSpace(result) == "" {
		return "(no output)"
	}
	return result
}

// stagedSummary returns a description of what is currently in the index.
func stagedSummary(ctx context.Context, repoRoot string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "diff", "--cached", "--name-status")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return "staged (could not read index summary)", nil
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return "nothing staged", nil
	}
	lines := strings.Split(trimmed, "\n")
	return fmt.Sprintf("staged %d file(s):\n%s", len(lines), trimmed), nil
}

type gitCommitResult struct {
	Hash    string // full SHA-1
	Subject string // first line of commit message
}

func resolveCommitInfo(ctx context.Context, repoRoot string) (gitCommitResult, error) {
	cmd := exec.CommandContext(ctx, "git", "log", "-1", "--format=%H\t%s")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return gitCommitResult{}, fmt.Errorf("git commit: reading commit info: %w", err)
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "\t", 2)
	if len(parts) < 2 {
		return gitCommitResult{Hash: strings.TrimSpace(string(out))}, nil
	}
	return gitCommitResult{Hash: parts[0], Subject: parts[1]}, nil
}

func formatGitCommitResult(r gitCommitResult) string {
	short := r.Hash
	if len(short) > 7 {
		short = short[:7]
	}
	if short == "" {
		return r.Subject
	}
	return fmt.Sprintf("%s %s", short, r.Subject)
}

func hasAnyFlag(args []string, flags ...string) bool {
	set := make(map[string]bool, len(flags))
	for _, f := range flags {
		set[f] = true
	}
	for _, a := range args {
		if set[a] {
			return true
		}
	}
	return false
}

func hasNonFlagArg(args []string) bool {
	for _, a := range args {
		if a != "" && !strings.HasPrefix(a, "-") {
			return true
		}
	}
	return false
}

// truncateLines caps s at maxLines lines. If the output is longer, the suffix
// is appended on a new line after the last included line.
func truncateLines(s string, maxLines int, suffix string) string {
	lines := strings.SplitN(s, "\n", maxLines+2)
	if len(lines) <= maxLines+1 {
		return s // fits within limit
	}
	return strings.Join(lines[:maxLines], "\n") + "\n" + suffix
}

// findGitRoot returns the root of the git repository that contains path.
// If path is empty, the current working directory is used.
func findGitRoot(path string) (string, error) {
	start := path
	if start == "" {
		var err error
		start, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("getting working directory: %w", err)
		}
	}

	info, err := os.Stat(start)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", start, err)
	}
	dir := start
	if !info.IsDir() {
		dir = filepath.Dir(start)
	}

	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("not a git repository: %s", dir)
	}
	return strings.TrimSpace(string(out)), nil
}
