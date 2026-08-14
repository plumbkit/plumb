package cli

// pool_homeguard.go — the identity machinery behind every home-directory
// workspace guard. This file owns one question: "is this directory a home
// directory?" (sameDirAs, by os.SameFile). Identity terminates the walks —
// Detect, SynthesiseRoot, detectLanguageAt — so $HOME itself never becomes a
// workspace root without a deliberate marker or an explicit declaration.
//
// It deliberately does NOT answer "does this directory CONTAIN a home
// directory?", which would refuse a root like /Users. That guard was built and
// removed again: seven rounds of adversarial review defeated three successive
// implementations, each on a different macOS path-aliasing shape — a lexical
// ancestry test lost to the /System/Volumes/Data/Users firmlink, and the
// os.SameFile ancestor walk that replaced it still missed /System/Volumes/Data
// itself, which contains $HOME while sharing an inode with no ancestor of
// canonical($HOME). Tracked separately rather than shipped defeated; see the
// issue linked from the CHANGELOG. The identity guard here fixes the reported
// defect (a dotfiles repo at $HOME capturing the whole home directory) and is
// the part that survived every round.

import (
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
)

// deliberatePlumbMarker reports whether the .plumb directory at dir carries
// evidence a human put it there. Consulted only when the marker sits at the
// home directory; everywhere else any .plumb is honoured exactly as before — a
// project marker does not need papers, only the one directory whose capture
// puts every credential file inside the boundary does.
//
// context.md ONLY, and the narrowness is the point. It is written by `plumb
// init` and by git_init's init_plumb, both of which a human runs at a directory
// they chose. An earlier version also accepted config.toml — and review showed
// that defeats the guard, because config.toml is MACHINE-written by
// config.SetProjectValue, whose callers include the agent_config tool, the web
// settings API, and (with no opt-in at all) any project-scoped save in the TUI.
// So a user who once pinned $HOME deliberately and then changed one setting
// would have minted their own permanent proof of intent, converting a one-off
// exemption into a standing home-directory workspace.
//
// materialisePlumbDir — the [workspace] auto_attach_persist path — creates a
// BARE .plumb and is separately refused at $HOME, and the daemon's own
// artefacts (topology.db, collab.db, memories/) are machine-generated wherever
// a session happened to run, so none of them prove anything about intent.
//
// The test of a file belonging here is not "does a human usually create it" but
// "can anything other than a human create it".
func deliberatePlumbMarker(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".plumb", "context.md"))
	return err == nil
}

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
