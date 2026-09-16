package cli

import (
	"log/slog"
	"slices"
	"sort"

	"github.com/plumbkit/plumb/internal/paths"
)

// Nominating a language that no MARKER speaks for, alongside the ones markers
// do. pool_detect.go finds the marker-backed roots and pool_detect_sniff.go
// counts files; this decides which languages the count alone earns a seat for.
//
// The gap it closes: detection had two contributors that each refused to run
// when the other had answered. discoverChildLanguages matches strong markers in
// subdirectories, and the last-resort sniff (extLangAt) fires only when NOTHING
// else resolved — so one marker-carrying child locked the sniff out of the whole
// workspace. A repo with app/tsconfig.json and forty .py files spread across
// five sibling directories, with no pyproject.toml anywhere, resolved typescript
// and nothing else: python appeared in no identity line, no badge, no
// workspace_symbols fan-out and no run_task reachability, although per-file
// routing had been serving those .py files all along. Detection should follow
// the evidence a project actually carries, and a language whose sources fill a
// tree is evidence whether or not anyone wrote a manifest for it.
//
// The census runs AFTER child discovery and takes its result as given: a
// language a marker already named is not re-counted, and the subtrees those
// markers claim are pruned from the walk, so what is measured is the remainder
// no marker speaks for.

const (
	// censusScanDepth / censusScanMaxFiles are the census's own bounds, and
	// deliberately not extScanDepth / extScanMaxFiles. Raising those would
	// change the last-resort sniff's answer for every existing workspace, which
	// is a different question from how deep this walk should look; they are
	// separate constants so the two can be tuned without one silently moving the
	// other.
	//
	// Depth 4 where the sniff uses 2, because the census has to reach sources a
	// shallow sample is happy to miss: server/api/handlers/x.py is three
	// directories down, and a flat-package layout puts them at two to four.
	// Deeper is not free and not safer — every level charges more entries
	// against the cap, and 5-6 is where the asset trees live (the reasoning
	// tieScanDepth already sets out), so the budget would be spent below the
	// code rather than on it.
	censusScanDepth = 4
	// Larger than extScanMaxFiles because the census MEASURES a remainder where
	// the sniff samples a prefix, and a share read off a truncated count is a
	// share of whatever the walk reached first. Still well under tieScanMaxFiles:
	// pruning the claimed roots, plus skipChildDir, plus .gitignore, has already
	// removed the trees that make a repo large.
	//
	// Raising the budget makes truncation rarer; it does not make it impossible,
	// and a truncated count is ACCEPTED rather than discarded. That is the choice
	// extLangAtIn documents, for the same reason — the alternative here is
	// nominating nothing, so a coarse answer beats silence — and it is a weaker
	// guarantee than resolveMarkerTie's, which discards a partial count because
	// there the alternative is a neutral fallback rather than nothing. The
	// outcome is logged so a surprising nomination on a huge tree can be traced
	// to the cap.
	censusScanMaxFiles = 5000

	// censusMinFiles and censusMinShare are BOTH required, and each exists
	// because the other one alone admits a shape it should not.
	//
	// A floor alone: a 30k-file TypeScript monorepo with eight .py helpers in
	// scripts/ clears any floor small enough to be useful, and starting pyright
	// there costs a process, its memory, and a badge that misdescribes the
	// project.
	//
	// A share alone: a scripts repo holding one .py and two .ts files gives
	// python a third of the counted remainder on the strength of a single file,
	// and a lone .py in an otherwise codeless directory gives it 100%. One stray
	// file must never start a server. (Stated in terms of files that COUNT: the
	// denominator is restricted to nominatable languages, so .md and .json
	// neighbours are not what holds such a share down — an earlier draft of this
	// comment claimed they were, which was wrong in the direction that makes the
	// floor look better justified than it is.)
	//
	// Five is the smallest count that cannot be a single incidental file plus
	// the __init__.py / conftest.py scaffolding that travels with it. The repo
	// this was written for has about forty.
	censusMinFiles = 5
	// Ten percent is deliberately NOT a majority rule. The whole point of the
	// census is that the markerless language is a genuine MINORITY contributor —
	// a Python service whose frontend outnumbers it, a tools tree beside an app
	// — so a majority threshold would reject exactly the case being fixed. The
	// denominator is the unclaimed remainder, never the whole tree, so the
	// claimed subtrees cannot dilute the vote of the part they do not cover.
	censusMinShare = 0.10
)

// censusMarkerlessLanguages nominates the ACTIVE languages that own a material
// share of the part of root no discovered marker claims, as additional
// discovered roots flagged sniffed. Returns nil when none qualifies.
//
// claimed is the marker-backed set from discoverChildLanguages. It is used twice
// and for two different reasons: its ROOTS are pruned from the walk (a language
// server already covers that subtree, and counting it would let the claimed tree
// decide the remainder's answer), and its LANGUAGES are skipped (a marker has
// already spoken for them, more strongly than a file count can).
//
// Every nominated entry is rooted at the WORKSPACE ROOT rather than at some
// subdirectory, deliberately. The files that earn the seat are typically spread
// across sibling directories with no common subroot — five of them, in the case
// this was written for — and the workspace root is exactly what resolveFileTarget
// resolves for those files when per-file routing lazily acquires a server. Both
// paths therefore name the same (root, language) pool key and share one server,
// where a synthesised subroot would start a second one serving the same files.
func (p *workspacePool) censusMarkerlessLanguages(root string, claimed []discoveredRoot) []discoveredRoot {
	// Resolved ONCE for the whole walk, the discipline extLangAtIn documents: a
	// project's .plumb/config.toml is parsed at most once here, never once per
	// file, which keeps the file budget a filesystem cost rather than a
	// config-parsing one.
	langs := p.effectiveLanguages(root)
	if len(langs) == 0 {
		return nil
	}
	// Canonicalised ONCE, here, rather than per directory inside the walk. Both
	// sides of the prune comparison have to agree on a spelling — a discovered
	// root can arrive by another one (a symlinked checkout, the macOS /tmp
	// firmlink) and a raw string compare would then fail to prune exactly the
	// subtree that is already served. Canonicalising the ROOT gives that
	// agreement for every path the walk builds from it: paths are joined onto it
	// and symlinked entries are skipped outright, so each `abs` is canonical by
	// construction. Doing it per directory instead cost an lstat chain per path
	// component per directory — about a fifth of the walk on a 1700-directory
	// tree — to re-derive what the root already guarantees.
	root = paths.Canonical(root)
	claimedPaths := make(map[string]bool, len(claimed))
	for _, d := range claimed {
		claimedPaths[paths.Canonical(d.root)] = true
	}
	claimedLangs := distinctLanguages(claimed)

	counts, truncated := p.sniffCountsIn(langs, root, censusScanDepth, censusScanMaxFiles, nil, skipChildDir,
		func(abs string) bool { return claimedPaths[abs] })

	// The denominator counts only languages that could themselves be nominated,
	// and that restriction is load-bearing rather than tidying. sniffCountsIn
	// classifies through langsupport.ByPath, which recognises json, yaml,
	// markdown, css and more — file types with no language server, which can never
	// pass the install -> on gate below. Counted in the denominator they only
	// dilute: 40 .py files beside a 400-file JSON fixture tree put python at 9%
	// and suppressed the very nomination this census exists to make. A share is
	// meant to say "this language is a material part of the CODE here", so the
	// comparison is against the code, not against everything on disk.
	total := 0
	for lang, n := range counts {
		if _, ok := cfgAmong(langs, lang); ok {
			total += n
		}
	}
	if total == 0 {
		return nil
	}

	// A sorted key slice, never a map range: the result reaches the session's
	// language label and adapter list, and those must not reorder between two
	// attaches of the same unchanged workspace.
	names := make([]string, 0, len(counts))
	for lang := range counts {
		names = append(names, lang)
	}
	sort.Slice(names, func(i, j int) bool {
		return sniffLess(names[i], counts[names[i]], names[j], counts[names[j]])
	})

	var out []discoveredRoot
	for _, lang := range names {
		if slices.Contains(claimedLangs, lang) {
			continue
		}
		// The install -> on gate, the same one extLangAtIn applies: a language
		// whose server is not installed, or which this project has disabled, is
		// counted by the census but never nominated by it.
		if _, ok := cfgAmong(langs, lang); !ok {
			continue
		}
		if counts[lang] < censusMinFiles || float64(counts[lang]) < censusMinShare*float64(total) {
			continue
		}
		out = append(out, discoveredRoot{root: root, language: lang, sniffed: true, files: counts[lang]})
	}
	if len(out) > 0 {
		slog.Debug("daemon: census nominated markerless languages",
			"root", root, "nominated", distinctLanguages(out), "counts", counts,
			"unclaimed_total", total, "truncated", truncated)
	}
	return out
}
