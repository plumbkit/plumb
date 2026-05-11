// Package fsguard guards filesystem walks against macOS TCC false-positive
// prompts. On macOS, opening or reading the contents of $HOME or its
// "protected" subdirectories (Desktop, Documents, Downloads, Pictures, Music,
// Movies, Public, iCloud Drive) triggers a TCC consent prompt attributed to
// the binary doing the syscall. plumb's workspace walks (session_start,
// init --discover) accept whatever root the caller hands them, so a misplaced
// roots/list response or a user invoking plumb in the wrong directory can
// trigger prompts in folders plumb has no business reading.
//
// RefuseWalk reports whether a given root should be refused. It is a no-op on
// non-Darwin platforms — Linux and Windows do not have the same per-folder
// consent model, so guarding there would only produce false negatives.
package fsguard

import (
	"os"
	"path/filepath"
	"runtime"
)

// RefuseWalk reports whether root is a macOS-protected directory that plumb
// should not crawl. Returns (false, "") on non-Darwin or when refuseHomeRoots
// is false. The string is a short reason suitable for logging.
//
// Refused roots are matched by exact path (after Clean + symlink resolution),
// not prefix: a legitimate project at $HOME/Documents/MyProject is allowed,
// but $HOME/Documents itself is refused. The intent is to catch false roots
// (e.g. an MCP client returning the home directory) without preventing users
// from working on projects that happen to live inside a protected folder.
func RefuseWalk(root string, refuseHomeRoots bool) (bool, string) {
	if !refuseHomeRoots || runtime.GOOS != "darwin" {
		return false, ""
	}
	if root == "" {
		return false, ""
	}
	resolved := canonical(root)
	for _, p := range protectedRoots() {
		if resolved == p {
			return true, "macOS-protected root: " + p
		}
	}
	return false, ""
}

// canonical resolves symlinks and cleans the path. If symlink resolution
// fails (e.g. path doesn't exist), the cleaned absolute path is returned.
func canonical(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = filepath.Clean(p)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// protectedRoots returns the set of paths that should not be crawled.
// $HOME itself plus the macOS folders that are TCC-gated by default. iCloud
// Drive's on-disk location is included; the Public folder is borderline but
// included for completeness (it is TCC-checked under "Network Volumes").
func protectedRoots() []string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	home = canonical(home)
	return []string{
		home,
		filepath.Join(home, "Desktop"),
		filepath.Join(home, "Documents"),
		filepath.Join(home, "Downloads"),
		filepath.Join(home, "Pictures"),
		filepath.Join(home, "Music"),
		filepath.Join(home, "Movies"),
		filepath.Join(home, "Public"),
		filepath.Join(home, "Library", "Mobile Documents", "com~apple~CloudDocs"),
	}
}
