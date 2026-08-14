package config

// config_git.go holds the [git] block. It lives in its own file because it is
// the git tool's complete safety policy AND, since [git] env, the environment
// of the process that runs a repository's hooks — a surface that earns being
// read on its own rather than as one struct among twenty.

// GitConfig controls the unified git tool's tiered allowlist. Read-only
// subcommands always run. Write, destructive, and network tiers are gated by
// these flags so the same tool can be flexible on trusted workspaces and
// locked down elsewhere. All fields can be overridden per-project via
// <workspace>/.plumb/config.toml and by environment variables.
//
// Concurrency: read-only after Load returns.
type GitConfig struct {
	// AllowWrites gates the safe-write tier (add, commit, switch, branch
	// create, tag create, stash push/pop). Default true.
	AllowWrites bool `toml:"allow_writes"`
	// AllowDestructive gates the destructive tier (reset, clean, checkout,
	// restore, rebase, revert, cherry-pick, branch/tag delete, stash
	// drop/clear). Each call also requires confirm:true. Default false.
	AllowDestructive bool `toml:"allow_destructive"`
	// AllowPush gates the network tier (push, fetch, pull). Each call also
	// requires confirm:true. Default false.
	AllowPush bool `toml:"allow_push"`
	// ProtectedBranches are branch names that may never be force-pushed, even
	// when AllowPush is true and confirm is set. Default ["main", "master"].
	ProtectedBranches []string `toml:"protected_branches"`
	// CommitTrailer stamps every plumb-mediated commit with a
	// `Plumb-Session: <session-name>` git trailer, so `git log` can attribute
	// a commit to the agent session that authored it. Default false — trailer
	// noise is opt-in. Session→commit attribution is always queryable via
	// workspace_sessions regardless of this knob.
	CommitTrailer bool `toml:"commit_trailer"`
	// Env holds environment variables set on the git child process, applied ON
	// TOP of the daemon's inherited environment (git needs PATH, HOME and
	// SSH_AUTH_SOCK to work at all, so this extends rather than replaces).
	// Empty — the default — leaves the child inheriting the daemon's
	// environment verbatim, exactly as before this knob existed.
	//
	// It lives in [git], not beside it, because [git] is the ONE block
	// forceCapabilityFieldsToBase resets whole: a git child's environment is a
	// code-execution channel (GIT_SSH_COMMAND, GIT_EXTERNAL_DIFF,
	// GIT_PROXY_COMMAND, GIT_PAGER all run commands, and GOFLAGS=-toolexec=…
	// reaches any `go` a hook invokes), so a cloned repository's
	// .plumb/config.toml must not be able to set it without `plumb trust`.
	// Being inside [git] is what gives it that boundary — see
	// project_policy.go.
	Env map[string]string `toml:"env"`
}
