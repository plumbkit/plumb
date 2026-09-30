package config

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/plumbkit/plumb/internal/paths"
)

// trust_worktree.go lets a linked git worktree share its repository's trust
// grant (#530).
//
// A grant is keyed on the path `plumb trust` was run in. A linked worktree —
// the recommended way to isolate an agent — is a new path, so it had no record
// and silently fell back to the global capability values: `[git] allow_push`
// honoured on the main checkout, refused in its worktree, with the same
// checked-in .plumb/config.toml in both.
//
// The rule, and why each clause is there:
//
//   - The grant is still bound to CONTENT. A worktree shares another checkout's
//     record only when its own request hashes to that record's hash, so a branch
//     that widens [git] or rewrites a task command is untrusted exactly as it
//     would be anywhere else. What is shared is an approval of this content in
//     this repository — equivalent to switching the trusted checkout to the
//     worktree's branch, which never re-prompted either.
//   - Both checkouts must share one common git directory: the approval covered
//     this repository, and a different repository with byte-identical config is
//     a different surface (its task commands run its own scripts).
//   - The worktree must be one git itself vouches for. A .git FILE is plain
//     text any directory can carry, naming a trusted repository's worktree; the
//     back-link git writes to <gitdir>/gitdir lives inside that repository's own
//     git directory, which a forged directory cannot write. It must name this
//     worktree.
//
// Only the two hash-bound gates use this (IsTrustedForPolicy, IsTrustedForTasks).
// The coarse Trusted flag is not bound to content, so sharing it would let a
// branch change whatever it gates without an approval; it stays per path.

// sharedWorktreeGrant returns the root whose grant a verified linked worktree
// at root shares — another checkout of the same repository holding a grant that
// match accepts — or "" when there is none. When several qualify, the
// lexically first is named, so the answer (which surfaces in user-facing text)
// does not depend on map order.
func sharedWorktreeGrant(m map[string]trustRecord, root string, match func(trustRecord) bool) string {
	key := canonRoot(root)
	common := linkedWorktreeCommonDir(key)
	if common == "" {
		return ""
	}
	from := ""
	for other, rec := range m {
		if other == key || !match(rec) || (from != "" && other > from) {
			continue
		}
		if checkoutCommonDir(other) == common {
			from = other
		}
	}
	return from
}

// linkedWorktreeCommonDir returns the canonical common git directory of the
// linked worktree whose top level is root, or "" when root is not one git
// vouches for: its .git must be a link file to a directory that sits directly
// in <common>/worktrees/ — inside the common git directory it names — and that
// directory's back-link must name root's own .git.
//
// Both halves are needed. The back-link alone proves nothing when the linked
// directory is one the borrower wrote itself (evil/fake/worktrees/x, naming
// evil/.git and a trusted repository's .git as its common directory); only a
// linked directory INSIDE the trusted repository's git directory puts the
// back-link where the borrower cannot write.
func linkedWorktreeCommonDir(root string) string {
	dotGit := filepath.Join(root, ".git")
	gitDir := readGitDirLink(dotGit)
	if gitDir == "" {
		return ""
	}
	back, err := readSmallFile(filepath.Join(gitDir, "gitdir"))
	if err != nil || paths.Canonical(resolveAgainst(gitDir, back)) != paths.Canonical(dotGit) {
		return ""
	}
	commonLink, err := readSmallFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		return ""
	}
	common := paths.Canonical(resolveAgainst(gitDir, commonLink))
	if filepath.Dir(paths.Canonical(gitDir)) != filepath.Join(common, "worktrees") {
		return ""
	}
	return common
}

// checkoutCommonDir returns the canonical common git directory of the checkout
// at root — a main checkout (.git directory), a linked worktree, or a
// submodule's checkout (.git link to a git directory with no commondir, which is
// then its own common directory) — or "" when root has no readable .git. It is
// applied only to roots already holding a grant, which the user approved.
func checkoutCommonDir(root string) string {
	dotGit := filepath.Join(root, ".git")
	info, err := os.Lstat(dotGit)
	if err != nil {
		return ""
	}
	if info.IsDir() {
		return paths.Canonical(dotGit)
	}
	gitDir := readGitDirLink(dotGit)
	if gitDir == "" {
		return ""
	}
	common, err := readSmallFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		return paths.Canonical(gitDir)
	}
	return paths.Canonical(resolveAgainst(gitDir, common))
}

// readGitDirLink reads a .git link file ("gitdir: <dir>") and returns the
// directory it names, resolved against the file's own directory; "" when
// dotGit is not a readable link file.
func readGitDirLink(dotGit string) string {
	data, err := readSmallFile(dotGit)
	if err != nil {
		return ""
	}
	target, ok := strings.CutPrefix(data, "gitdir:")
	if !ok || strings.TrimSpace(target) == "" {
		return ""
	}
	return resolveAgainst(filepath.Dir(dotGit), strings.TrimSpace(target))
}

// resolveAgainst makes p absolute against dir, the way git resolves the
// relative paths it writes into its link files.
func resolveAgainst(dir, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(dir, p)
}

// maxGitLinkFileBytes bounds a link-file read: git writes one path, and the
// files read here sit in directories the trust decision does not yet trust.
const maxGitLinkFileBytes = 4096

// readSmallFile returns a git link file's trimmed content, refusing anything
// that is not a regular file (a FIFO planted where git keeps a link would block
// the attach reading it) or is larger than a link file can legitimately be.
func readSmallFile(path string) (string, error) {
	if info, err := os.Lstat(path); err != nil {
		return "", err
	} else if !info.Mode().IsRegular() {
		return "", os.ErrInvalid
	}
	f, err := os.Open(path) //nolint:gosec // G304: reading git's own link files is the point
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxGitLinkFileBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxGitLinkFileBytes {
		return "", os.ErrInvalid
	}
	return strings.TrimSpace(string(data)), nil
}
