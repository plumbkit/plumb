package cli

// pool_homeguard.go — the identity machinery behind every home-directory
// workspace guard. Detect, SynthesiseRoot, detectLanguageAt, and the attach
// paths all refuse to treat $HOME as (or ascend past it into) a workspace
// root; this file owns how "is this directory the home directory?" is decided.

import (
	"log/slog"
	"os"
	"os/user"
)

// homeDirInfos stats the candidate home directories for os.SameFile identity
// comparisons (robust to trailing slashes and symlink/firmlink aliasing, where
// a string compare is defeated). Two sources feed it: $HOME (os.UserHomeDir)
// and the OS user database (os/user.Current). The second is not redundancy —
// the daemon inherits its environment from whichever client spawned it, so an
// emptied or repointed $HOME alone must not disarm the guards protecting the
// REAL home directory; they are what keeps a dotfiles repo from widening a
// workspace to every credential under it. Duplicates collapse by identity.
//
// Returns an empty slice only when no home is determinable at all. The guards
// then run inert (sameDirAs fails open) rather than refusing every repo — but
// loudly: from that point the process has no home-boundary net, which an
// operator should be able to see in the log rather than infer from a breach.
func homeDirInfos() []os.FileInfo {
	var infos []os.FileInfo
	add := func(dir string) {
		if dir == "" {
			return
		}
		info, err := os.Stat(dir)
		if err != nil {
			return
		}
		for _, have := range infos {
			if os.SameFile(have, info) {
				return
			}
		}
		infos = append(infos, info)
	}
	if home, err := os.UserHomeDir(); err == nil {
		add(home)
	}
	if u, err := user.Current(); err == nil {
		add(u.HomeDir)
	}
	if len(infos) == 0 {
		slog.Warn("workspace detection: no home directory is determinable ($HOME unset and no user database entry) — the home-directory workspace guards are disarmed for this process")
	}
	return infos
}

// sameDirAs reports whether dir refers to the same directory as any of infos
// (the home-directory identities from homeDirInfos), comparing by filesystem
// identity via os.SameFile. This is robust to trailing slashes, "."/".."
// segments, and symlink / macOS-firmlink aliasing, where a raw string compare
// against $HOME would be defeated by any non-canonical spelling.
//
// Returns false when infos is empty (home undeterminable — homeDirInfos has
// already logged that loudly) or dir cannot be stat'd: a deliberate fail-OPEN,
// leaving the guards inert rather than refusing every legitimate repo on a
// machine with a broken environment. Pinned by TestSameDirAs_NoHomeFailsOpen —
// flipping this to fail-closed would make every directory look like $HOME and
// silently refuse all detection.
func sameDirAs(dir string, infos []os.FileInfo) bool {
	if len(infos) == 0 {
		return false
	}
	di, err := os.Stat(dir)
	if err != nil {
		return false
	}
	for _, info := range infos {
		if info != nil && os.SameFile(di, info) {
			return true
		}
	}
	return false
}
