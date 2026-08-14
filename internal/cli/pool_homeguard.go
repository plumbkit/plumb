package cli

// pool_homeguard.go — the home-directory machinery behind the workspace
// guards. This file owns two questions.
//
// "Is this directory A home directory?" — sameDirAs, by os.SameFile identity.
// Identity terminates the walks (Detect, SynthesiseRoot, detectLanguageAt),
// so $HOME itself never becomes a workspace root without a deliberate marker
// or an explicit declaration.
//
// "Does this directory CONTAIN a home directory?" — homeUnder. Three earlier
// implementations of this guard were each defeated by a different macOS
// path-aliasing shape (issue #306 records them all): a lexical ancestry test
// lost to the /System/Volumes/Data/Users firmlink — that path IS /Users, same
// inode, and every resolve-then-compare-strings verdict says otherwise — and
// the os.SameFile ancestor walk that replaced it missed /System/Volumes/Data
// itself, which contains $HOME while sharing an inode with NEITHER /Users nor
// /. The predicate that ships probes in the other direction: for every suffix
// of home's own components, join the candidate to that suffix and let the
// KERNEL resolve the join (firmlinks, volume aliases and symlinks are
// answered by path traversal), then compare the result to home by identity.
// Any path to home ends in home's own component names — dentry names are
// unique within a parent, and firmlinks duplicate subtrees under identical
// names — so one of the probes lands. Known limits, stated rather than
// claimed away: a SYMLINK below the candidate reaching home under a name of
// its own, Linux bind mounts, and spellings that differ only in case.
//
// The refusal built on it (undeclaredWideRootErr) sits where the session's
// root is SET and the pin's origin is in scope — never in detect(), which has
// no notion of a declaration and broke hermetic sandboxes (HOME repointed
// into the checkout) the last time containment was tried there.

import (
	"fmt"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"

	"github.com/plumbkit/plumb/internal/sessionstate"
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

// userDBHome returns the home directory recorded in the OS user database
// (os/user.Current), or "" when there is none. Memoised — a process's uid
// does not move while it runs.
//
// Deliberately NOT $HOME. The containment guard refuses UNDECLARED wide roots
// like /Users or /home, and hermetic build sandboxes repoint $HOME INSIDE the
// checkout (HOME=$PWD/.home; Bazel execroots, nix-shell and CI images do the
// same thing). Keying containment on $HOME would refuse those checkouts on
// every non-explicit attach — exactly the undetectable-workspace failure that
// killed the guard's first placement in detect(). The user-database home is
// the machine's real credential store; a repointed $HOME is a sandbox the
// caller built. Falls open (and logs once, loudly) when the database has no
// entry — the guard is inert rather than absolute on a broken machine, the
// same trade homeDirInfos makes.
var userDBHome = sync.OnceValue(func() string {
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		slog.Warn("workspace detection: no user-database home directory — the home-containment guard is disarmed for this process (the identity guard still runs)")
		return ""
	}
	return u.HomeDir
})

// homeUnder reports whether dir CONTAINS the home directory: dir sits on some
// kernel path that reaches home, whether or not dir is a LEXICAL ancestor of
// home's spelling. "Directory C contains directory H" has no syscall; every
// answer is either a string comparison (defeated by aliasing) or a walk from
// one end (defeated by aliases reachable only from the other). This probes
// instead of walking: for every suffix of home's components — including the
// empty suffix, which covers dir BEING home — it joins dir to the suffix,
// hands the join to the kernel to resolve, and compares the result to home by
// identity (dev+inode), never by spelling.
//
// Why the probes suffice: any kernel path to home ends in home's own
// component names — a directory's dentry name is unique within its parent, and
// macOS firmlinks duplicate a subtree under IDENTICAL names on both sides —
// so a container of home reaches it through a chain whose tail is one of
// these suffixes, and the kernel's own traversal resolves firmlinks and
// symlinked spellings of the container along the way. That is the shape
// /System/Volumes/Data (suffix "Users/<name>") and /System/Volumes/Data/Users
// (suffix "<name>") both present, and both are covered by
// TestHomeUnder_MacOSSystemVolumeAliases on a real macOS machine.
//
// Known limits: a symlink BELOW dir reaching home under a name of its own
// (the probe set only enumerates home's names), a Linux bind mount of home
// under dir, and components differing only in case. A root so constructed is
// a caller's own planting, not a wide system directory.
func homeUnder(dir, home string) bool {
	if dir == "" || home == "" {
		return false
	}
	homeInfo, err := os.Stat(home)
	if err != nil {
		return false
	}
	comps := strings.Split(filepath.Clean(home), string(filepath.Separator))
	for i := len(comps); i >= 1; i-- {
		probe := dir
		if i < len(comps) {
			probe = filepath.Join(append([]string{dir}, comps[i:]...)...)
		}
		if info, err := os.Stat(probe); err == nil && os.SameFile(info, homeInfo) {
			return true
		}
	}
	return false
}

// containsUserHome reports whether dir is, or CONTAINS, a home directory:
// identity against every determinable home (the $HOME the daemon inherited
// included), then containment against the machine's real one. The two halves
// key differently on purpose — see homeDirInfos and userDBHome.
func containsUserHome(dir string) bool {
	if sameDirAs(dir, homeDirInfos()) {
		return true
	}
	if home := userDBHome(); home != "" {
		return homeUnder(dir, home)
	}
	return false
}

// undeclaredWideRootErr is the containment guard consulted by all three
// writers of v.acquiredRoot (attachWorkspacePinFrom, attachSynthetic, and
// attachOrRepinTo via repinWorkspaceFrom): a root that is or CONTAINS a home
// directory may only be pinned by an explicit session_start declaration
// (issue #182's contract — an explicit pin always succeeds — is the
// carve-out). Every other origin is a weaker signal than "the caller named
// this directory": a client-reported root, a rehydrated row, an incidental
// tool path. Pinning /Users, /home, / or /System/Volumes/Data without a
// declaration puts every home directory on the machine — every SSH key and
// credential under them — inside the session's read-write boundary
// (issue #306).
//
// The check lives HERE, at the root-setting choke point where the origin is
// in scope, rather than in detect(): detection has no notion of a
// declaration, so it could only refuse for everyone — and the last time
// containment was tried there, hermetic sandboxes whose $HOME lives inside
// the checkout became undetectable with nothing naming the cause. Detection
// keeps answering "which project is this?" for everyone; this function decides who may PIN the answer.
func undeclaredWideRootErr(root string, origin sessionstate.PinSource) error {
	if origin == sessionstate.PinSourceSessionStart {
		return nil
	}
	if sameDirAs(root, homeDirInfos()) {
		return fmt.Errorf(
			"root %s is a home directory and was not declared by an explicit session_start — pinning it would put every credential under it inside the boundary", root)
	}
	if home := userDBHome(); home != "" && homeUnder(root, home) {
		return fmt.Errorf(
			"root %s contains the home directory %s — pinning it would put every credential under that home inside the boundary. Call session_start({workspace: %q}) if you really mean it",
			root, home, root)
	}
	return nil
}
