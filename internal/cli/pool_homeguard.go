package cli

// pool_homeguard.go — the identity machinery behind every home-directory
// workspace guard. Detect, SynthesiseRoot, detectLanguageAt, and the attach
// paths all refuse to treat $HOME as (or ascend past it into) a workspace
// root; this file owns how "is this directory the home directory?" is decided.

import (
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/plumbkit/plumb/internal/paths"
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

// containsHomeDir reports whether dir is a strict ANCESTOR of a home directory.
//
// sameDirAs answers identity, which leaves the rung above unguarded: refusing
// $HOME while accepting /Users (or /home, or /) admits a workspace that
// CONTAINS the home directory and every credential in it — wider than the
// boundary the identity guard exists to keep, and reachable without any
// deliberate declaration through a client reporting such a folder in its roots.
// The guard's own stated rationale is that a root at or above the home
// directory can only ever be too wide, so it has to test ancestry too.
//
// Ancestry is decided on resolved paths, because the identity guard's whole
// reason for using os.SameFile is that a string compare against $HOME loses to
// symlink and firmlink aliasing; a lexical prefix test here would lose the same
// way. Strict: dir being the home directory itself is sameDirAs's question, not
// this one.
func containsHomeDir(dir string, infos []os.FileInfo) bool {
	if len(infos) == 0 || dir == "" {
		return false
	}
	resolved := paths.Canonical(dir)
	for _, info := range infos {
		if info == nil {
			continue
		}
		home := homePathFor(info)
		if home == "" || sameDirAs(dir, []os.FileInfo{info}) {
			continue
		}
		rel, err := filepath.Rel(resolved, paths.Canonical(home))
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		return true
	}
	return false
}

// homePathFor recovers a path for one of homeDirInfos' entries. os.FileInfo
// carries only a base name, so the candidate sources are re-consulted and
// matched by identity — the same two homeDirInfos itself used.
func homePathFor(info os.FileInfo) string {
	var candidates []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, home)
	}
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		candidates = append(candidates, u.HomeDir)
	}
	for _, c := range candidates {
		if ci, err := os.Stat(c); err == nil && os.SameFile(ci, info) {
			return c
		}
	}
	return ""
}
