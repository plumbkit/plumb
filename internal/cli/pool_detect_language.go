package cli

// pool_detect_language.go — naming the LANGUAGE, once the root is settled.
//
// pool_detect.go answers "which project is this?" by walking for markers. The
// questions here start from an answer to that: given a root the caller already
// holds, which language owns it; given one file, which server should serve it;
// and is a caller-supplied language one this workspace may use at all. Each
// takes the effective language set as an argument, or resolves it once from the
// root it is given — never per ancestor, and never per file.

import (
	"path/filepath"

	"github.com/plumbkit/plumb/internal/langsupport"
	"github.com/plumbkit/plumb/internal/paths"
)

// resolveFileTarget answers, in ONE policy resolution, the two questions every
// URI-bearing routing call asks: which workspace owns this file, and which
// language server should serve it. The language is the file's own by extension,
// falling back to the root's detected language for files no active language owns
// (a .md beside .go still goes to gopls, which ignores it).
//
// It exists for cost, not tidiness. Routing used to call Detect and then
// fileLanguage, and once both resolve the project's language set that is TWO
// ancestor walks and two stamp stats per LSP request — measured at ~135us on a
// deep file where the old global-slice lookup was ~30ns. Detection has to walk
// regardless; the second walk bought nothing, because Detect has already
// resolved the very set fileLanguage needs. Every caller that holds a URI should
// use this rather than the two calls.
func (p *workspacePool) resolveFileTarget(path string) (root, language string, err error) {
	dir := filepath.Dir(path)
	langs := p.effectiveLanguages(dir)
	root, language, err = p.detectIn(dir, langs)
	if err != nil {
		return "", "", err
	}
	if fileLang := p.fileLanguageIn(langs, path); fileLang != "" {
		language = fileLang
	}
	// Canonicalised here, not in detectIn, so this matches Detect's contract
	// exactly — the routing key must have the one spelling every other consumer
	// of a root uses (issue #263).
	return paths.Canonical(root), language, nil
}

// hasActiveLanguage reports whether name is an active (enabled + installed)
// language in this pool — the set workspace detection and routing consult. Used
// to validate a caller-supplied language override before pinning it.
func (p *workspacePool) hasActiveLanguage(name string) bool {
	_, ok := cfgAmong(p.langsSnapshot(), name)
	return ok
}

// hasActiveLanguageIn reports whether name is active for the project governing
// root — the set that project's detection and routing consult, which a
// .plumb/config.toml may have widened or narrowed relative to the global one.
func (p *workspacePool) hasActiveLanguageIn(root, name string) bool {
	_, ok := cfgAmong(p.effectiveLanguages(root), name)
	return ok
}

// languageForRoot resolves the language for an already-determined workspace root
// (a .plumb marker, or a re-pin): a strong marker at the root or an ancestor,
// else a weak marker at the root itself, else LanguageNone.
func (p *workspacePool) languageForRoot(dir string) string {
	return p.languageForRootIn(dir, p.effectiveLanguages(dir))
}

func (p *workspacePool) languageForRootIn(dir string, langs []langConfig) string {
	if lang := p.lspLanguageForRootIn(dir, langs); lang != "" {
		return lang
	}
	return LanguageNone
}

// lspLanguageForRoot returns the LSP language owning dir — a strong marker at
// dir or any ancestor (bounded at $HOME), else a weak marker at dir itself — or
// "" when none. Unlike languageForRoot it returns "" (not LanguageNone) so
// callers that need an actual server language can tell "no language" apart.
func (p *workspacePool) lspLanguageForRoot(dir string) string {
	return p.lspLanguageForRootIn(dir, p.effectiveLanguages(dir))
}

func (p *workspacePool) lspLanguageForRootIn(dir string, langs []langConfig) string {
	if lang := p.detectLanguageAt(dir, langs); lang != "" {
		return lang
	}
	return p.weakLangAtIn(dir, langs)
}

// detectLanguageAt returns the language whose STRONG root marker is present at
// dir or any ancestor, or "". Used to resolve the adapter for an already-known
// root. Weak markers are not consulted here (see weakLangAt / lspLanguageForRoot).
//
// The ancestor walk stops at $HOME by IDENTITY, mirroring Detect's .git
// fallback guard: a stray language marker in the home directory (e.g. a global
// ~/go.mod) must not capture every .plumb workspace beneath it. For a walk
// starting beneath $HOME that also covers everything above it — the walk meets
// $HOME first — but a walk starting elsewhere does consult directories that
// contain a home directory. That is deliberate, not an oversight: this
// function names a LANGUAGE for an already-fixed root, never the root or the
// boundary, and testing containment here would return "" for a repo whose
// sandbox $HOME lives inside it (the round-6 B3 regression class).
//
// The ascent keeps the language policy of the KNOWN root it started from,
// rather than re-resolving at each ancestor. The root is already fixed; the
// question is only which language owns it, and answering that with an ancestor
// project's enablement would let a directory above a workspace decide what its
// own .plumb/config.toml had settled.
func (p *workspacePool) detectLanguageAt(dir string, langs []langConfig) string {
	homeInfo := homeDirInfos()
	d := dir
	for {
		if sameDirAs(d, homeInfo) {
			return ""
		}
		if lang := p.strongLangAtIn(d, langs); lang != "" {
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
// The language set is the governing PROJECT's, not the daemon's global one, so a
// repository that enables Python in its own .plumb/config.toml routes its .py
// files to pyright on a daemon whose global config leaves Python off — and one
// that disables a language stops routing to it. A bounded scan resolves the set
// once and calls fileLanguageIn per file instead (see sniffCounts).
func (p *workspacePool) fileLanguage(path string) string {
	return p.fileLanguageIn(p.effectiveLanguages(filepath.Dir(path)), path)
}

// fileLanguageIn is fileLanguage against an already-resolved effective language
// set. It takes a bare file NAME as readily as a path — langsupport.ByPath keys
// on the extension — which is what lets the sniff walk call it per entry.
func (p *workspacePool) fileLanguageIn(langs []langConfig, path string) string {
	l, ok := langsupport.ByPath(path)
	if !ok {
		return ""
	}
	key := normaliseLangName(l.Name)
	if _, ok := cfgAmong(langs, key); !ok {
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
