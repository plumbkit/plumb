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
	// WriteTimeout bounds an index/ref-mutating git child (the write and
	// destructive tiers) once plumb has decoupled it from request and daemon
	// cancellation. When it expires the child's process group is killed and the
	// failure is reported as plumb's own timeout rather than as a refusal from
	// git. Default 10 minutes; 0 means the compiled default, and there is
	// deliberately no value that disables the bound (see below).
	//
	// The bound used to be a hardcoded 2 minutes described as "generous enough
	// for a slow pre-commit hook". That is false on any machine where several
	// agents share one toolchain: a hook running golangci-lint queues behind a
	// peer's run on the same shared cache, and those waits have been observed to
	// exceed ten minutes on plumb's own repository. A wrong bound is expensive
	// in BOTH directions, which is why this is a knob rather than a better
	// constant.
	//
	// It lives in [git] for the same reason Env does, and the reasoning is the
	// same shape: this is a safety decision, not a preference. Too LARGE lets a
	// wedged child hold the per-repository serialisation lock — and the shutdown
	// drain — against every other session on the machine; too SMALL makes plumb
	// SIGKILL git mid-commit, which is precisely what strands a
	// .git/index.lock and leaves a half-written index behind. Neither is a
	// choice a cloned repository's .plumb/config.toml should make unasked, and
	// being inside [git] is what puts it behind `plumb trust`.
	WriteTimeout Duration `toml:"write_timeout"`
}
