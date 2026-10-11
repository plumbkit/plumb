package paths

import (
	"os"
	"path/filepath"
	"strings"
)

// checkout.go resolves a workspace's git layout from the disk, and answers the one
// question the mail boundary asks of it: are these two workspaces one project?
//
// It never runs git. The send path calls it once per note, but the READ path calls
// it on every mailbox check and the Stop hook calls it at every turn end, so a
// subprocess per call is out of the question — and so is trusting a path that was
// merely written down. Every layout it accepts is VERIFIED against a back-link git
// itself wrote inside the repository's own .git directory, which a checkout cloned
// inside that tree cannot write to:
//
//   - a main checkout's .git is a directory;
//   - a linked worktree's git directory holds `commondir` (its common directory)
//     and `gitdir` (a back-link naming the worktree's own .git FILE), and it must
//     sit directly in <common>/worktrees/;
//   - a submodule's git directory holds no commondir, but its config carries
//     `core.worktree` naming the checkout it serves.
//
// Without that verification a hostile clone inside a workspace could write its own
// `.git` file naming a real worktree's git directory, inherit its commondir, and
// pass as part of the project — naming a path is free, only writing is not.
//
// Concurrency: stateless and read-only; safe for concurrent use.

// Checkout is a verified git checkout: the root it was resolved from, its own git
// directory, and the common git directory it shares with the repository's other
// worktrees (equal to GitDir for a main checkout and for a submodule, whose git
// directory is nobody's worktree).
type Checkout struct {
	Root      string
	GitDir    string
	CommonDir string
}

// CheckoutAt resolves and verifies the git checkout rooted at root. ok is false
// when root holds no readable .git, or when the link it holds cannot be verified —
// which is deliberately the same answer: an unverifiable claim is not evidence.
//
// A `.git` SYMLINK is one of the rejected shapes (it is neither a directory nor a
// readable link FILE). Git does not write one, and accepting it would let a
// checkout point at somebody else's git directory and inherit that repository's
// identity for a boundary decision.
func CheckoutAt(root string) (Checkout, bool) {
	canon := Canonical(root)
	if canon == "" {
		return Checkout{}, false
	}
	dotGit := filepath.Join(canon, ".git")
	info, err := os.Lstat(dotGit)
	if err != nil {
		return Checkout{}, false
	}
	if info.IsDir() {
		return Checkout{Root: canon, GitDir: dotGit, CommonDir: dotGit}, true
	}
	// Lstat reported what the name IS, but every read after this one follows
	// links: a `.git` symlink to a real worktree's .git file would read that
	// file's gitdir line, and its back-link — compared by file identity — would
	// then name "this" checkout. Only a regular file is a link git wrote here.
	if !info.Mode().IsRegular() {
		return Checkout{}, false
	}
	gitDir, ok := readGitDirLink(dotGit, canon)
	if !ok {
		return Checkout{}, false
	}
	if common, ok := linkedWorktreeCommon(gitDir, dotGit); ok {
		return Checkout{Root: canon, GitDir: gitDir, CommonDir: common}, true
	}
	if submoduleServes(canon, gitDir) {
		return Checkout{Root: canon, GitDir: gitDir, CommonDir: gitDir}, true
	}
	return Checkout{}, false
}

// SameProject reports whether two workspace roots belong to one repository family:
// equal roots, equal common directories (a main checkout and its worktrees, or two
// sibling worktrees), or one side's common directory inside the other's (a
// submodule inside its superproject, either way round — and nested submodules
// follow transitively).
//
// The containing side is the COMMON directory, not the checkout's own git
// directory, so a linked worktree of the superproject reaches the submodule just
// as the main checkout does: the submodule's git directory sits in
// <common>/modules/, never in <common>/worktrees/<id>/. Containment is evidence
// for the same reason the back-link is: only the repository's owner writes inside
// its common directory, and both sides were verified before they got here.
//
// It is symmetric by construction: every clause is either an equality or a
// containment tested in both directions.
func SameProject(a, b string) bool {
	ca, oka := CheckoutAt(a)
	cb, okb := CheckoutAt(b)
	if !oka || !okb {
		return false
	}
	return samePath(ca.Root, cb.Root) || sameFamily(ca.CommonDir, cb.CommonDir)
}

// sameFamily is the clause SameProject and SameCommonDir share: two verified
// common directories are one family when they are the same directory or one lies
// inside the other.
func sameFamily(a, b string) bool {
	return samePath(a, b) || withinDir(a, b) || withinDir(b, a)
}

// SameCommonDir reports whether a workspace root belongs to the same project as a
// common git directory recorded earlier — the sender's, stamped on a mail row at
// send time. It is the read-time half of SameProject, and it exists because the
// sender's own checkout may be GONE by the time the note is read (a worktree
// removed minutes after sending): the recorded common directory is then the only
// side available, and the caller's own checkout is the one verified here.
//
// It applies exactly SameProject's family clause, so the two answers agree for any
// pair of live checkouts: SameProject(a, b) == SameCommonDir(CheckoutAt(a).CommonDir, b).
func SameCommonDir(stampedCommon, root string) bool {
	if stampedCommon == "" {
		return false
	}
	c, ok := CheckoutAt(root)
	if !ok {
		return false
	}
	return sameFamily(stampedCommon, c.CommonDir)
}

// readGitDirLink reads a checkout's `.git` FILE, "gitdir: <dir>", resolving the
// target against the checkout it sits in (git writes it relative for a submodule).
func readGitDirLink(dotGit, root string) (string, bool) {
	data, err := os.ReadFile(dotGit)
	if err != nil {
		return "", false
	}
	target, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	if !ok {
		return "", false
	}
	target = strings.TrimSpace(target)
	if target == "" {
		return "", false
	}
	return Canonical(resolveAgainst(root, target)), true
}

// linkedWorktreeCommon verifies that gitDir is a linked worktree's git directory
// for the checkout whose .git FILE is dotGit, and returns its common directory.
//
// Both halves are required, and they are the whole point of this file: the linked
// directory must sit DIRECTLY in <common>/worktrees/ (a parent comparison, not a
// prefix — "worktrees/evil-extra" is not "worktrees"), and its `gitdir` back-link
// must name this checkout's own .git file. A forged `.git` naming someone else's
// worktree fails the second test, because that back-link names the real checkout.
func linkedWorktreeCommon(gitDir, dotGit string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		return "", false
	}
	common := strings.TrimSpace(string(data))
	if common == "" {
		return "", false
	}
	common = Canonical(resolveAgainst(gitDir, common))
	if !samePath(filepath.Dir(gitDir), filepath.Join(common, "worktrees")) {
		return "", false
	}
	back, err := os.ReadFile(filepath.Join(gitDir, "gitdir"))
	if err != nil {
		return "", false
	}
	if !samePath(resolveAgainst(gitDir, strings.TrimSpace(string(back))), dotGit) {
		return "", false
	}
	return common, true
}

// submoduleServes reports whether gitDir is the git directory of the submodule
// checked out at root: its config's core.worktree must name that checkout.
func submoduleServes(root, gitDir string) bool {
	worktree := gitConfigWorktree(filepath.Join(gitDir, "config"))
	if worktree == "" {
		return false
	}
	return samePath(resolveAgainst(gitDir, worktree), root)
}

// gitConfigWorktree returns the `worktree` value under [core] in a git config, or
// "" when there is none. Only that one key is read, from the small config git
// writes into a submodule's git directory: a line scanner is honest about how much
// of the format this needs, and a config this file cannot parse verifies nothing.
func gitConfigWorktree(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	inCore := false
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			inCore = strings.EqualFold(line, "[core]")
			continue
		}
		if !inCore {
			continue
		}
		if key, value, ok := strings.Cut(line, "="); ok && strings.EqualFold(strings.TrimSpace(key), "worktree") {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// resolveAgainst makes p absolute against base when it is relative, which is how
// each of the three links is written: a .git file's target against its checkout, a
// commondir and a core.worktree against the git directory holding them.
func resolveAgainst(base, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}

// samePath reports whether two paths name the same place, by file identity rather
// than by spelling: os.Stat follows symlinks and os.SameFile compares what the two
// names actually resolve to. That is what the macOS cases need — /var and
// /private/var are two spellings of one directory, and the default filesystem is
// case-insensitive — and it is also why this is not a string comparison: the same
// checkout reached through a symlinked home must not read as a different project.
func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	//nolint:gosec // G703: a path here comes either from a workspace root the daemon
	// resolved or from a .git file's contents, and the latter is verified against a
	// back-link inside the repository's own .git before it is used (see the file
	// comment). Both are only ever stat-ed and compared — never written, never exec'd.
	ia, err := os.Stat(a)
	if err != nil {
		return false
	}
	//nolint:gosec // G703: as above.
	ib, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ia, ib)
}

// withinDir reports whether child lies inside parent (or IS parent). Both sides
// are canonicalised first, so a symlink cannot smuggle a path out of the tree it
// names; a path that no longer exists is not inside anything.
func withinDir(child, parent string) bool {
	if child == "" || parent == "" {
		return false
	}
	c, err := filepath.EvalSymlinks(child)
	if err != nil {
		return false
	}
	p, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(p, c)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
