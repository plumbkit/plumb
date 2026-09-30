package tools

// git_own_writes.go keeps plumb's own git operations from being reported as a
// peer's edits (#529).
//
// read_file warns "changed on disk since plumb last wrote it this session — a
// peer or external process may have edited it" when a file plumb wrote has a
// newer mtime than the WriteTracker recorded. A switch, merge, restore or stash
// pop made THROUGH the git tool rewrites such files too, so the next read blamed
// a peer for plumb's own operation.
//
// runGit therefore takes a before-image of the session's recorded files in the
// repository (WriteTracker.unchangedUnder) once it holds the per-repository
// lock, and afterwards re-records each whose mtime moved during the operation
// (refreshChanged). Two properties keep the warning honest:
//
//   - A file a peer had ALREADY changed before the operation is not in the
//     before-image, so its warning survives whatever git then does to it.
//   - A change after the operation is newer than the refreshed record, so it
//     warns exactly as before.
//
// The limit is the operation's own duration: a native edit to one of those files
// while git runs is indistinguishable from git's write. That is why the refresh
// is confined to subcommands that rewrite the working tree themselves — a commit,
// whose hooks can run for minutes, is excluded, so a peer's edit during a slow
// pre-commit hook is still reported.

// trackOwnGitWrites takes the before-image for a git call and returns the
// refresh to run once the child has exited (a no-op for a call that cannot
// rewrite the working tree). nil-safe on writes.
func trackOwnGitWrites(writes *WriteTracker, repoRoot, sub string, tier gitTier) func() {
	if !rewritesWorkTree(sub, tier) {
		return func() {}
	}
	before := writes.unchangedUnder(repoRoot)
	return func() { writes.refreshChanged(before) }
}

// rewritesWorkTree reports whether a git call can itself change files in the
// working tree, and so needs its effect on the session's written files
// re-recorded. Reads never do; nor do the index- and ref-only writes (add,
// commit, branch and tag create) or fetch and push, which touch only refs.
func rewritesWorkTree(sub string, tier gitTier) bool {
	if tier == tierRead || tier == tierReject {
		return false
	}
	switch sub {
	case "add", "commit", "branch", "tag", "fetch", "push":
		return false
	}
	return true
}
