package tools

import "encoding/json"

// git_schema.go owns the git tool's wire contract: the JSON schema a client reads
// in tools/list. It sits apart from git.go (the MCP surface and request
// orchestration) so the contract a caller reads and the behaviour it triggers can
// each be read whole — and because the schema's prose is what the per-tool byte
// budget is spent on, which catalogue_budget_test.go enforces.

var gitSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "subcommand": {
      "type": "string",
      "description": "Git subcommand. Read: diff, log, show, blame, status, shortlog, check-ignore, merge-tree, check-attr, worktree list, branch/tag/stash listing. Write: add, commit, switch, mv, merge, worktree add/remove/lock/unlock, branch/tag create, stash push/pop. Destructive: reset, clean, checkout, restore, rebase, revert, cherry-pick, merge --abort/--quit, worktree move/prune/repair, branch/tag delete, stash drop. Network: push, fetch, pull."
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
    },
    "amend": {
      "type": "boolean",
      "description": "commit only: fold the staged changes into HEAD (--amend) instead of adding a commit. Omit message to keep HEAD's message. Refused when HEAD is already on a remote-tracking ref."
    },
    "wait": {
      "type": "boolean",
      "description": "Wait for a mutating git child (commit and friends) instead of detaching at [git] detach_after with STILL RUNNING; bounded by [git] write_timeout. Returns the real result, including a hook's output when it refuses."
    },
    "start_line": {
      "type": "integer",
      "description": "Read tier: first output line to return (1-based, counted in the command's own output — for show <rev>:<path> it windows that file's lines)."
    },
    "end_line": {
      "type": "integer",
      "description": "Read tier: last output line to return, inclusive."
    },
    "pattern": {
      "type": "string",
      "description": "Read tier: matching output lines instead of a contiguous range; literal text unless use_regex. Narrow with start_line/end_line."
    },
    "use_regex": {
      "type": "boolean",
      "description": "Read tier: treat pattern as a Go RE2 regex, not literal text."
    },
    "case_sensitive": {
      "type": "boolean",
      "description": "Read tier: default smart-case (insensitive unless the pattern has an uppercase letter); set to force."
    }
  },
  "required": ["subcommand"],
  "additionalProperties": false
}`)

// Name, InputSchema and Description are the git tool's advertised surface. They
// live here, with the schema, rather than in git.go: the schema-contract test
// (schema_contract_test.go) counts each file's InputSchema declarations against
// its additionalProperties markers, so a schema and its accessor belong together.

func (t *Git) Name() string                 { return "git" }
func (t *Git) InputSchema() json.RawMessage { return gitSchema }
func (t *Git) Description() string {
	return "Policy-gated git, no shell. Reads always run. Writes need [git] allow_writes (default on). Destructive subcommands need allow_destructive and confirm:true; push, fetch and pull need allow_push and confirm:true. Force-pushing a protected branch or using an ad-hoc remote is always refused. add (-A) and commit (message, optional files) are typed; others take args. A write is refused if a DIFFERENT session moved HEAD since you looked (confirm:true overrides). Details: the plumb-git skill."
}
