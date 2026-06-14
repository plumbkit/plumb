package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/langsupport"
)

// markerPresent reports whether root marker exists directly in dir. A marker
// containing '*' is glob-matched (e.g. "*.xcodeproj" for an Xcode project whose
// name is unknown ahead of time); otherwise it is an exact filename.
func markerPresent(dir, marker string) bool {
	if strings.ContainsRune(marker, '*') {
		matches, _ := filepath.Glob(filepath.Join(dir, marker))
		return len(matches) > 0
	}
	_, err := os.Stat(filepath.Join(dir, marker))
	return err == nil
}

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
	homeInfo := homeFileInfo()
	d := filepath.Clean(start)
	first := true
	for {
		// Highest priority: explicit .plumb marker. Honour it even when no
		// LSP language matches — the user has declared this directory a
		// plumb workspace, and stats / project config should follow that
		// declaration regardless of whether a language server can attach.
		if _, err := os.Stat(filepath.Join(d, ".plumb")); err == nil {
			return d, p.languageForRoot(d), nil
		}
		// Next: first language whose STRONG root marker exists at d. Skip $HOME
		// (by filesystem identity) so a stray ~/go.mod cannot turn all of $HOME
		// into a language workspace for any path beneath it.
		if !sameDirAs(d, homeInfo) {
			if lang := p.strongLangAt(d); lang != "" {
				return d, lang, nil
			}
		}
		// A .git directory marks a project boundary even without a strong
		// marker. A weak, promiscuous marker (package.json, index.html) names
		// the language only at such a boundary — or at the directory the caller
		// pointed at (first iteration) — never at an arbitrary ancestor, so a
		// stray tooling package.json up the tree cannot hijack the workspace.
		gitHere := false
		if d != filepath.Dir(d) && !sameDirAs(d, homeInfo) {
			if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
				gitHere = true
			}
		}
		if (gitHere || first) && !sameDirAs(d, homeInfo) {
			if lang := p.weakLangAt(d); lang != "" {
				return d, lang, nil
			}
		}
		if gitHere {
			return d, LanguageNone, nil
		}
		first = false
		parent := filepath.Dir(d)
		if parent == d {
			return "", "", fmt.Errorf("no project root found in or above %s", start)
		}
		d = parent
	}
}

// homeFileInfo stats $HOME once for os.SameFile identity comparisons (robust to
// trailing slashes and symlink/firmlink aliasing where a string compare is
// defeated). Returns nil when the home directory is undeterminable, leaving the
// $HOME guards inert rather than refusing a legitimate repo.
func homeFileInfo() os.FileInfo {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	info, _ := os.Stat(home)
	return info
}

// strongLangAt returns the first active language whose RootMarkers exist
// directly in dir, or "". Single directory, no ascent.
func (p *workspacePool) strongLangAt(dir string) string {
	for _, l := range p.langs {
		for _, marker := range l.cfg.RootMarkers {
			if markerPresent(dir, marker) {
				return l.name
			}
		}
	}
	return ""
}

// discoveredRoot pairs a subdirectory carrying a strong language root marker
// with the language that marker names. See discoverChildLanguages.
type discoveredRoot struct {
	root     string
	language string
}

// discoverChildLanguages descends up to maxDepth levels below root looking for
// strong language root markers in SUBdirectories — the monorepo case where the
// root itself carries no marker of its own (a .plumb/ root over core/build.zig +
// app/Package.swift). Detect handles the root and its ancestors; this is the
// only place plumb looks DOWNWARD, and only for an already-resolved root.
//
// A directory that matches is a language project root, so the walk records it
// and does NOT descend into it (nearest-wins, mirroring Detect). Strong markers
// only — weak markers (package.json) are promiscuous and would mis-capture
// tooling dirs. Noise dirs (.git, .plumb, dotdirs, node_modules, build outputs)
// are pruned so depth does not explode. Symlinked dirs are skipped (DirEntry.
// IsDir is false for them), avoiding cycles. maxDepth <= 0 disables discovery.
// The caller is responsible for not invoking this on $HOME.
func (p *workspacePool) discoverChildLanguages(root string, maxDepth int) []discoveredRoot {
	if maxDepth <= 0 {
		return nil
	}
	type item struct {
		dir   string
		depth int
	}
	var out []discoveredRoot
	stack := []item{{dir: root, depth: 0}}
	for len(stack) > 0 {
		it := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if it.depth >= maxDepth {
			continue
		}
		entries, err := os.ReadDir(it.dir)
		if err != nil {
			continue
		}
		for _, de := range entries {
			if !de.IsDir() || skipChildDir(de.Name()) {
				continue
			}
			child := filepath.Join(it.dir, de.Name())
			if lang := p.strongLangAt(child); lang != "" {
				out = append(out, discoveredRoot{root: child, language: lang})
				continue // a language root is a project boundary — do not descend
			}
			stack = append(stack, item{dir: child, depth: it.depth + 1})
		}
	}
	return out
}

// skipChildDir reports whether a directory name should be pruned from the child
// language scan: any dotdir (.git, .plumb, .build, .zig-cache, …) plus common
// dependency and build-output dirs that never hold a project's own root marker.
func skipChildDir(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	switch name {
	case "node_modules", "vendor", "dist", "build", "zig-cache", "zig-out", "target":
		return true
	}
	return false
}

// electPrimary picks the connection's primary from discovered child roots, with
// the same deterministic order newWorkspacePool sorts languages by: "go" first,
// then alphabetical by language, tie-broken by the shorter/lexicographic root
// path. A stable choice means workspace_symbols and the hierarchies resolve the
// same primary across reconnects; the others attach lazily and fan-out covers
// them. Panics on an empty slice — callers guard len(discovered) > 0.
func electPrimary(ds []discoveredRoot) discoveredRoot {
	best := ds[0]
	for _, d := range ds[1:] {
		if lessDiscovered(d, best) {
			best = d
		}
	}
	return best
}

func lessDiscovered(a, b discoveredRoot) bool {
	if a.language != b.language {
		if a.language == "go" {
			return true
		}
		if b.language == "go" {
			return false
		}
		return a.language < b.language
	}
	return a.root < b.root
}

// hasActiveLanguage reports whether name is an active (enabled + installed)
// language in this pool — the set workspace detection and routing consult. Used
// to validate a caller-supplied language override before pinning it.
func (p *workspacePool) hasActiveLanguage(name string) bool {
	for _, l := range p.langs {
		if l.name == name {
			return true
		}
	}
	return false
}

// weakLangAt returns the first active language whose WeakRootMarkers exist
// directly in dir, or "". Weak markers (package.json, index.html) are
// promiscuous, so they only name the language of the directory they sit in —
// never an ancestor — which is what keeps a stray package.json from capturing
// an unrelated workspace.
func (p *workspacePool) weakLangAt(dir string) string {
	for _, l := range p.langs {
		for _, marker := range l.cfg.WeakRootMarkers {
			if markerPresent(dir, marker) {
				return l.name
			}
		}
	}
	return ""
}

// languageForRoot resolves the language for an already-determined workspace root
// (a .plumb marker, or a re-pin): a strong marker at the root or an ancestor,
// else a weak marker at the root itself, else LanguageNone.
func (p *workspacePool) languageForRoot(dir string) string {
	if lang := p.lspLanguageForRoot(dir); lang != "" {
		return lang
	}
	return LanguageNone
}

// lspLanguageForRoot returns the LSP language owning dir — a strong marker at
// dir or any ancestor (bounded at $HOME), else a weak marker at dir itself — or
// "" when none. Unlike languageForRoot it returns "" (not LanguageNone) so
// callers that need an actual server language can tell "no language" apart.
func (p *workspacePool) lspLanguageForRoot(dir string) string {
	if lang := p.detectLanguageAt(dir); lang != "" {
		return lang
	}
	return p.weakLangAt(dir)
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

// detectLanguageAt returns the language whose STRONG root marker is present at
// dir or any ancestor, or "". Used to resolve the adapter for an already-known
// root. Weak markers are not consulted here (see weakLangAt / lspLanguageForRoot).
//
// The ancestor walk stops at $HOME, mirroring Detect's .git fallback guard: a
// stray language marker in the home directory (e.g. a global ~/go.mod) must not
// capture every .plumb workspace beneath it. $HOME and anything above it are
// never a project root, so they are never consulted for the language.
func (p *workspacePool) detectLanguageAt(dir string) string {
	homeInfo := homeFileInfo()
	d := dir
	for {
		if sameDirAs(d, homeInfo) {
			return ""
		}
		if lang := p.strongLangAt(d); lang != "" {
			return lang
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
