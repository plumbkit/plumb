package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/langsupport"
	"github.com/plumbkit/plumb/internal/paths"
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
//     never resolves and the TUI shows "resolving…" forever.
//
// If no marker is found, walk up to the parent. If we walk past the filesystem
// root, return an error.
//
// The user's home directory TERMINATES the walk: a dotfiles repo or a stray
// ~/go.mod must not turn all of $HOME into a workspace, and nothing above the
// home directory can legitimately be a project root either — so reaching
// $HOME without a match is an error, not a rung. The one thing honoured AT
// $HOME is a deliberate `.plumb/` marker (one carrying a context.md — see
// deliberatePlumbMarker, which does NOT accept config.toml, because plumb
// writes that itself from three places): a user who ran `plumb init` in their
// home directory has declared that intent. A bare `.plumb/` there is
// treated as residue (the auto_attach_persist path of an earlier build could
// materialise one) and ignored with a logged remediation, because honouring it
// would silently re-open the whole-home workspace on every future session,
// with auto_attach off.
//
// The root is returned CANONICALISED — symlinks resolved (issue #263). The pool
// is where plumb answers "which project is this?", so it is where that answer
// gets one spelling: the session pin, the session registry, the boundary policy,
// the collab store, and the (root, language) key the language-server pool is
// indexed by all derive from it. Without this they disagreed whenever one project
// was reachable two ways — the macOS /tmp → /private/tmp firmlink, a symlinked
// checkout — which routed same-project mail cross-project (where the default
// config drops it unread) and let one project hold two language servers.
//
// Only the RESULT is canonicalised, never the starting point: the marker walk
// must keep following the caller's own spelling, or a project reached through a
// symlinked parent would search a different ancestor chain and miss the .plumb/
// marker sitting beside the link.
func (p *workspacePool) Detect(start string) (root, language string, err error) {
	root, language, err = p.detect(start)
	if err != nil {
		return "", "", err
	}
	return paths.Canonical(root), language, nil
}

// detect is Detect's marker walk, before canonicalisation.
func (p *workspacePool) detect(start string) (root, language string, err error) {
	homeInfo := homeDirInfos()
	d := filepath.Clean(start)
	first := true
	for {
		// IDENTITY only, deliberately. An earlier round widened this to "at or
		// above a home directory" so that a marker-carrying wide directory could
		// not make Detect succeed — and the next round showed that shape is wrong
		// twice over. It applied the deliberate-marker exemption ABOVE $HOME (a
		// context.md at /Users, mintable from inside one explicit pin, resolved
		// /Users for every caller); and detect() has no notion of a DECLARED
		// workspace, so it could only refuse for everyone — making a repo that
		// legitimately contains its own home directory (HOME=$PWD/.home hermetic
		// sandboxes, Bazel execroots, nix-shell, CI images) undetectable, which
		// cost such workspaces their real language with nothing naming the cause.
		//
		// Containment is instead refused where the session's root is SET and the
		// pin's origin is in scope — undeclaredWideRootErr, consulted by all three
		// writers of v.acquiredRoot — so a wide detection result still cannot be
		// pinned without an explicit session_start declaration, while detection
		// itself keeps answering "which project is this?" for everyone.
		atHome := sameDirAs(d, homeInfo)
		// Highest priority: explicit .plumb marker. Honour it even when no
		// LSP language matches — the user has declared this directory a
		// plumb workspace, and stats / project config should follow that
		// declaration regardless of whether a language server can attach.
		// AT the home directory only a deliberate marker counts: a bare
		// ~/.plumb is residue an earlier build's auto_attach_persist could
		// have created, and honouring it would resolve $HOME as the workspace
		// on every session from then on, auto_attach or not.
		if _, err := os.Stat(filepath.Join(d, ".plumb")); err == nil {
			if !atHome || deliberatePlumbMarker(d) {
				return d, p.languageForRoot(d), nil
			}
			p.homePlumbWarn.Do(func() {
				slog.Warn("workspace detection: ignoring the .plumb marker at the home directory — it carries no context.md, so it looks machine-created (an earlier build's auto_attach_persist). To keep the directory's memories and index and silence this, create the file: touch ~/.plumb/context.md. To discard it entirely, remove ~/.plumb — note that deletes ~/.plumb/memories and the topology index with it",
					"dir", filepath.Join(d, ".plumb"))
			})
		}
		// The home directory terminates the walk: neither $HOME (a dotfiles
		// .git, a stray ~/go.mod) nor anything above it may become a workspace
		// root for a path beneath it. Continuing upward — the old shape, which
		// merely SKIPPED the markers here — meant a .git above the home
		// directory resolved $HOME's parent: a root strictly wider than the
		// escape this guard exists to block.
		if atHome {
			return "", "", fmt.Errorf("no project root found between %s and the home directory (which is never used as a workspace root, nor ascended past)", start)
		}
		// Next: first language whose STRONG root marker exists at d.
		if lang := p.strongLangAt(d); lang != "" {
			return d, lang, nil
		}
		// A .git directory marks a project boundary even without a strong
		// marker. A weak, promiscuous marker (package.json, index.html) names
		// the language only at such a boundary — or at the directory the caller
		// pointed at (first iteration) — never at an arbitrary ancestor, so a
		// stray tooling package.json up the tree cannot hijack the workspace.
		gitHere := false
		if d != filepath.Dir(d) {
			if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
				gitHere = true
			}
		}
		if gitHere || first {
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

// strongLangAt returns the first active language whose RootMarkers exist
// directly in dir, or "". Single directory, no ascent.
func (p *workspacePool) strongLangAt(dir string) string {
	for _, l := range p.langsSnapshot() {
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
	for _, l := range p.langsSnapshot() {
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
	for _, l := range p.langsSnapshot() {
		for _, marker := range l.cfg.WeakRootMarkers {
			if markerPresent(dir, marker) {
				return l.name
			}
		}
	}
	return ""
}

// extScanDepth / extScanMaxFiles bound the content sniff so it can never stall
// detection on a large tree: it descends at most this many levels below the root
// and examines at most this many files before giving up.
const (
	extScanDepth    = 2
	extScanMaxFiles = 2000
)

// extLangAt is the last-resort content sniff for a resolved LanguageNone root:
// it returns the ACTIVE language (installed + enabled — fileLanguage gates on
// the effective set) owning the most source files in a bounded shallow scan of
// dir, or "". This is what lets a git repo full of .py files with no
// pyproject.toml attach Python when pyright is installed, matching the
// "install → on" philosophy for ecosystems that have no mandatory manifest. It
// runs at attach only AFTER strong-marker child discovery finds nothing (so a
// true monorepo is rooted per-child, not collapsed to one language here), and
// scans dir without ascending. Defensive throughout — any read error skips that
// entry rather than failing, so detection never crashes on an odd filesystem;
// noise dirs (.git, node_modules, build outputs) are pruned. The dominant-file
// count is a coarse heuristic — a large generated/vendored tree with a
// non-standard dir name (not caught by skipChildDir) can skew it — but as a
// last resort it is strictly better than LanguageNone and never overrides a
// strong or weak marker.
func (p *workspacePool) extLangAt(dir string) string {
	if len(p.langsSnapshot()) == 0 {
		return ""
	}
	type item struct {
		dir   string
		depth int
	}
	counts := map[string]int{}
	scanned := 0
	stack := []item{{dir: dir, depth: 0}}
	for len(stack) > 0 && scanned < extScanMaxFiles {
		it := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		entries, err := os.ReadDir(it.dir)
		if err != nil {
			continue
		}
		for _, de := range entries {
			if de.IsDir() {
				if it.depth < extScanDepth && !skipChildDir(de.Name()) {
					stack = append(stack, item{dir: filepath.Join(it.dir, de.Name()), depth: it.depth + 1})
				}
				continue
			}
			if scanned >= extScanMaxFiles {
				break
			}
			scanned++
			if lang := p.fileLanguage(de.Name()); lang != "" {
				counts[lang]++
			}
		}
	}
	return bestSniffedLang(counts)
}

// bestSniffedLang picks the dominant language from a sniff count map with a
// deterministic total order (independent of map iteration): most files wins,
// then "go" first, then alphabetical. Returns "" for an empty map.
func bestSniffedLang(counts map[string]int) string {
	if len(counts) == 0 {
		return ""
	}
	best := ""
	for lang := range counts {
		if best == "" || sniffLess(lang, counts[lang], best, counts[best]) {
			best = lang
		}
	}
	return best
}

func sniffLess(a string, na int, b string, nb int) bool {
	if na != nb {
		return na > nb
	}
	if a == "go" {
		return true
	}
	if b == "go" {
		return false
	}
	return a < b
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

// detectLanguageAt returns the language whose STRONG root marker is present at
// dir or any ancestor, or "". Used to resolve the adapter for an already-known
// root. Weak markers are not consulted here (see weakLangAt / lspLanguageForRoot).
//
// The ancestor walk stops at $HOME, mirroring Detect's .git fallback guard: a
// stray language marker in the home directory (e.g. a global ~/go.mod) must not
// capture every .plumb workspace beneath it. $HOME and anything above it are
// never a project root, so they are never consulted for the language.
func (p *workspacePool) detectLanguageAt(dir string) string {
	homeInfo := homeDirInfos()
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
	root, _, err := resolveCLIWorkspaceDetailed(start, cfg)
	return root, err
}

// resolveCLIWorkspaceDetailed resolves start like resolveCLIWorkspace and
// additionally reports whether the daemon would actually ATTACH there.
// attachable is false for a path that resolved only because Detect found no
// marker and start was echoed back unchanged (no .plumb/, no language root
// marker, no .git/ above) — the daemon refuses such a path with "cannot
// determine workspace root" unless [workspace].auto_attach is enabled (then it
// synthesises a root). `config show` uses this to flag a path the daemon would
// reject instead of printing a misleading ✓; the optimistic root is still
// returned so the inspection use-case keeps working.
func resolveCLIWorkspaceDetailed(start string, cfg config.Config) (root string, attachable bool, err error) {
	if start == "" {
		cwd, gerr := os.Getwd()
		if gerr != nil {
			return "", false, fmt.Errorf("getwd: %w", gerr)
		}
		start = cwd
	}
	abs, err := filepath.Abs(start)
	if err != nil {
		return "", false, fmt.Errorf("resolving workspace path %s: %w", start, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", false, fmt.Errorf("stat workspace path %s: %w", abs, err)
	}
	if !info.IsDir() {
		abs = filepath.Dir(abs)
	}
	resolved, _, derr := newWorkspacePool(context.Background(), cfg).Detect(abs)
	if derr != nil {
		// No marker found. The directory the user named IS the answer here — only
		// canonicalised, so the CLI (stats, config show, trust, run_task) keys on
		// the same spelling the daemon's rows do (issue #263).
		//
		// Deliberately NOT SynthesiseRoot. The original reason — it had no $HOME
		// guard where Detect did, so under a dotfiles repo `plumb config unset
		// --workspace ~/scratch` would have edited $HOME/.plumb/config.toml — no
		// longer applies: SynthesiseRoot now carries the same guard and its walk
		// stops at $HOME, so on this branch the two agree almost everywhere. The
		// reasons that remain: (1) the CLI's markerless contract is "the
		// directory the user NAMED", not "whatever the daemon's auto_attach
		// fallback would synthesise" — the daemon only synthesises when
		// auto_attach is on, and this branch is precisely where the default
		// config leaves it with no root at all; and (2) SynthesiseRoot's
		// non-explicit mode refuses a $HOME seed outright, which would turn
		// `plumb config show --workspace ~` into an empty answer for an
		// inspection command that must be able to name any directory
		// (TestResolveCLIWorkspaceDetailed_HomeItselfIsInspectable pins this).
		return paths.Canonical(abs), cfg.Workspace.AutoAttach, nil
	}
	return resolved, true, nil
}
