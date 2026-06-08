package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/golimpio/plumb/internal/config"
	"github.com/golimpio/plumb/internal/langsupport"
)

// LanguageNone is the sentinel language returned by Detect for workspaces
// that are explicitly marked (via .plumb/) but have no enabled LSP language.
// Filesystem tools, stats attribution, and project config all still work for
// these workspaces; LSP tools fail with "LSP server not yet ready".
const LanguageNone = "none"

// Detect walks up from start looking for a workspace root, with three
// markers tried in priority order at each directory (nearest directory wins,
// since the walk returns on the first match):
//
//  1. A `.plumb/` marker. If an LSP language is also detectable from this
//     directory or any ancestor, return (root, language). Otherwise return
//     (root, "none") — the user marked this directory as a workspace, so we
//     respect that even without LSP support.
//  2. A configured language's root marker (`go.mod`, `pyproject.toml`, ...).
//     Returns (root, language).
//  3. A `.git/` directory. A git repository is an unambiguous project
//     boundary, so a repo with no language marker (a scripts / multi-language
//     repo) still resolves — returned as (root, "none"). This is what lets
//     such workspaces attach in the default config; without it the session
//     never resolves and the TUI shows "resolving…" forever. The user's $HOME
//     is excluded: a dotfiles repo at $HOME must not turn all of $HOME into a
//     workspace.
//
// If no marker is found, walk up to the parent. If we walk past the filesystem
// root, return an error.
func (p *workspacePool) Detect(start string) (root, language string, err error) {
	// Stat $HOME once so the .git guard below can compare by filesystem
	// identity (os.SameFile) rather than by string — a raw compare is defeated
	// by a trailing slash or a symlink/firmlink alias of $HOME.
	var homeInfo os.FileInfo
	if home, herr := os.UserHomeDir(); herr == nil && home != "" {
		homeInfo, _ = os.Stat(home)
	}
	d := filepath.Clean(start)
	for {
		// Highest priority: explicit .plumb marker. Honour it even when no
		// LSP language matches — the user has declared this directory a
		// plumb workspace, and stats / project config should follow that
		// declaration regardless of whether gopls or pyright can attach.
		if _, err := os.Stat(filepath.Join(d, ".plumb")); err == nil {
			if lang := p.detectLanguageAt(d); lang != "" {
				return d, lang, nil
			}
			return d, LanguageNone, nil
		}
		// Next: first language whose root marker exists.
		for _, l := range p.langs {
			for _, marker := range l.cfg.RootMarkers {
				if _, err := os.Stat(filepath.Join(d, marker)); err == nil {
					return d, l.name, nil
				}
			}
		}
		// Lowest priority: a .git directory marks a project boundary even
		// without a language. Skip $HOME (by filesystem identity, so a
		// non-canonical spelling cannot defeat the guard) so a dotfiles repo
		// there does not capture the whole home directory, and skip the
		// filesystem root.
		if d != filepath.Dir(d) && !sameDirAs(d, homeInfo) {
			if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
				return d, LanguageNone, nil
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", "", fmt.Errorf("no project root found in or above %s", start)
		}
		d = parent
	}
}

// sameDirAs reports whether dir refers to the same directory as info (typically
// the user's $HOME), comparing by filesystem identity via os.SameFile. This is
// robust to trailing slashes, "."/".." segments, and symlink / macOS-firmlink
// aliasing, where a raw string compare against $HOME would be defeated by any
// non-canonical spelling. Returns false when info is nil (home undeterminable)
// or dir cannot be stat'd, leaving the .git guard inert rather than refusing a
// legitimate repo in those cases.
func sameDirAs(dir string, info os.FileInfo) bool {
	if info == nil {
		return false
	}
	di, err := os.Stat(dir)
	if err != nil {
		return false
	}
	return os.SameFile(di, info)
}

// SynthesiseRoot returns a synthetic workspace root for seedDir, used as a
// last resort when Detect has already failed. It walks up from seedDir
// looking for a .git directory (the conventional project-root signal for
// unrecognised languages). If found, that directory is returned. If the
// filesystem root is reached without finding .git, seedDir itself is
// returned as the safest approximation.
//
// SynthesiseRoot must only be called on the Detect error path in
// OnBeforeTool — never inside route() or LSP-routing paths.
func (p *workspacePool) SynthesiseRoot(seedDir string) string {
	d := seedDir
	for {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return seedDir // reached filesystem root — use the seed itself
		}
		d = parent
	}
}

// detectLanguageAt returns the language for dir based on which root marker
// is present at dir or any ancestor. Used after a .plumb/ marker is found
// to determine which adapter to start.
func (p *workspacePool) detectLanguageAt(dir string) string {
	d := dir
	for {
		for _, l := range p.langs {
			for _, marker := range l.cfg.RootMarkers {
				if _, err := os.Stat(filepath.Join(d, marker)); err == nil {
					return l.name
				}
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
	}
}

// fileLanguage maps a file path to the ENABLED config language key whose LSP
// should handle it, or "" when no enabled language owns the file. It is the
// per-file routing primitive that lets a single root drive several language
// servers (e.g. a .html file routed to the HTML server while .go files go to
// gopls). langsupport.ByPath resolves the owning language by extension;
// normaliseLangName folds tree-sitter dialect names to the config LSP key
// (tsx/jsx/javascript share the typescript-language-server); cfgFor gates on
// the language actually being enabled.
func (p *workspacePool) fileLanguage(path string) string {
	l, ok := langsupport.ByPath(path)
	if !ok {
		return ""
	}
	key := normaliseLangName(l.Name)
	if _, ok := p.cfgFor(key); !ok {
		return ""
	}
	return key
}

// normaliseLangName folds a langsupport.Language.Name to the config LSP map key.
// The tsx/jsx/javascript dialects are all served by the typescript adapter, so
// they collapse to "typescript"; every other name already equals its config key.
func normaliseLangName(name string) string {
	switch name {
	case "tsx", "jsx", "javascript":
		return "typescript"
	default:
		return name
	}
}

// resolveCLIWorkspace resolves start to the same workspace root the daemon
// would use, without acquiring or starting a language server. If no project
// marker exists, it returns start unchanged so explicit non-project inspection
// paths keep their current behaviour.
func resolveCLIWorkspace(start string, cfg config.Config) (string, error) {
	if start == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("getwd: %w", err)
		}
		start = cwd
	}
	abs, err := filepath.Abs(start)
	if err != nil {
		return "", fmt.Errorf("resolving workspace path %s: %w", start, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("stat workspace path %s: %w", abs, err)
	}
	if !info.IsDir() {
		abs = filepath.Dir(abs)
	}
	root, _, err := newWorkspacePool(context.Background(), cfg).Detect(abs)
	if err != nil {
		return abs, nil
	}
	return root, nil
}
