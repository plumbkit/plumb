package cli

import (
	"os"
	"path/filepath"
	"strings"
)

// Deciding a language from FILE EVIDENCE, as opposed to from the marker walk in
// pool_detect.go. Two callers want the same KIND of evidence — a bounded count
// of source files beneath a directory — and read it differently: extLangAt as a
// last resort when no marker named a language at all, and strongLangAt's
// tie-break when several markers named different ones for the same directory.
// They walk with DIFFERENT budgets: the sniff's is a shallow guess over the
// first files it meets; the tie-break's is far larger and prunes known
// non-source directories, because it must survive the very trees a contested
// JVM project ships.

// extScanDepth / extScanMaxFiles bound the content sniff so it can never stall
// detection on a large tree: it descends at most this many levels below the root
// and examines at most this many files before giving up.
const (
	extScanDepth    = 2
	extScanMaxFiles = 2000
	// tieScanDepth is deeper than extScanDepth because the tie it breaks is a
	// JVM one, and the JVM convention buries sources far down. The depth that
	// matters is the MULTI-project one, which is the standard Gradle shape:
	//
	//	<module>/src/main/kotlin/com/example/App.kt
	//	   1     2    3     4     5      6
	//
	// Six directory levels before the file, so the walk must descend six. At 5
	// — enough for a SINGLE-project src/main/kotlin/com/example — every module's
	// sources sit just out of reach, and a multi-project root counted nothing
	// but build scripts.
	//
	// EXACTLY six, with no spare level, because depth is not free: every level
	// is more directories whose entries are charged against the walk's file
	// budget. A seventh bought nothing any test could show and cost a real
	// project — src/main/resources/static/css/fonts/ sits at six, so a seventh
	// descends into the asset directory below it and can spend the whole budget
	// there before reaching src/main/kotlin. Deeper is not safer; it is a wider
	// surface for the cap to be hit on the wrong files.
	tieScanDepth = 6
	// tieScanMaxFiles is the tie-break's OWN file budget, deliberately far
	// larger than extScanMaxFiles. Sharing the sniff's 2000 was the bug: the
	// tie-break exists for JVM projects, and they bury arbitrarily large
	// non-source trees — src/main/resources full of property files — in the very
	// directories it walks. The walk is a LIFO over sorted listings, so
	// resources pops before kotlin, and one big resources tree exhausted the
	// budget before a source file was reached; the truncated count was then
	// discarded and the tie fell back to the language order — java for a
	// pure-Kotlin project, above a size threshold.
	//
	// Ten times the sniff's budget is cheap: ties are rare (strongLangAt scans
	// only when several markers claim one directory) and a 20k-file walk takes
	// a few milliseconds. The budget stays FINITE — it is the work bound on a
	// hostile tree, not a target — and a walk that still hits it reports
	// truncation, which strongLangAt degrades to the deterministic order on.
	// Both sides are pinned: the repro tree sits comfortably under it, and a
	// tree over it must still truncate and fall back.
	tieScanMaxFiles = 20000
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
	// The truncation flag is deliberately ignored here, where strongLangAt
	// heeds it. The two callers want opposite things from a partial answer:
	// this one is a LAST RESORT whose alternative is LanguageNone, so a coarse
	// guess off the first 2000 files beats no language at all; the tie-break is
	// choosing between two specific candidates, where a partial count is not a
	// weaker answer but a differently-wrong one.
	counts, _ := p.sniffCounts(dir, extScanDepth, nil)
	return bestSniffedLang(counts)
}

// sniffCounts counts source files per ACTIVE language in a bounded shallow scan
// of dir, descending at most maxDepth levels and examining at most
// extScanMaxFiles files. This is the LAST-RESORT SNIFF's walk (extLangAt): a
// coarse guess over the first files the scan meets, whose alternative is
// LanguageNone. The contested-marker tie-break wants the same evidence under a
// looser budget and with known non-source directories pruned — that walk is
// tieBreakCounts; both delegate to walkLangCounts. Defensive throughout — any
// read error skips that entry rather than failing, so detection never crashes
// on an odd filesystem.
func (p *workspacePool) sniffCounts(dir string, maxDepth int, ignore []string) (counts map[string]int, truncated bool) {
	return p.walkLangCounts(dir, maxDepth, extScanMaxFiles, ignore, nil)
}

// tieBreakCounts is the contested-marker tie-break's walk: the same count as
// sniffCounts, but under tieScanDepth / tieScanMaxFiles and pruning the
// directory names tieSkipDir recognises. Both relaxations belong to THIS path
// alone: the tie-break exists for JVM projects, which bury large non-source
// trees (src/main/resources, Android assets/res) in exactly the directories
// the walk examines, and under the sniff's 2000-file budget one such tree
// exhausted the scan before a source file was reached — truncation, discarded
// counts, and a pure-Kotlin project falling back to java above a size
// threshold. extLangAt's pinned semantics are untouched: it keeps the tight
// budget and no pruning.
//
// ignore names the contested root markers, which must not be counted ANYWHERE
// in the walk; walkLangCounts says why that reaches the whole subtree.
func (p *workspacePool) tieBreakCounts(dir string, ignore []string) (counts map[string]int, truncated bool) {
	return p.walkLangCounts(dir, tieScanDepth, tieScanMaxFiles, ignore, tieSkipDir)
}

// tieSkipDir reports whether a directory name is pruned from the tie-break
// walk: the conventional non-source containers of the ecosystems the tie-break
// decides between. src/main/resources holds property and asset files by the
// thousand in a JVM project; Android mirrors the shape with assets/ and res/.
// None of them holds sources the tied candidates contest, and they are exactly
// the trees large enough to starve the walk's budget before it reaches a
// source directory. Pruning stays a tie-break-only mercy: the last-resort
// sniff counts what is actually there, coarsely, and its truncation behaviour
// is pinned as-is.
func tieSkipDir(name string) bool {
	switch name {
	case "resources", "assets", "res":
		return true
	}
	return false
}

// walkLangCounts is the shared bounded walk behind sniffCounts and
// tieBreakCounts: it counts source files per ACTIVE language beneath dir,
// descending at most maxDepth levels, examining at most maxFiles files, and
// pruning any directory skipDir names (nil prunes nothing).
//
// ignore names files that must not be counted ANYWHERE in the walk — the
// contested root markers, for the tie-break; nil for the plain sniff. It has
// to reach the whole subtree, not just dir, because a build script is a source
// file of one of the languages it is contested between and the standard Gradle
// MULTI-project ships one per module: a root with `settings.gradle.kts`,
// `build.gradle.kts` and `app/build.gradle.kts` reads as three Kotlin files
// before a line of anyone's code is counted, and modules outvote sources as
// the project grows. Discounting only the markers sitting directly in dir left
// every nested one voting, which handed Java multi-projects to Kotlin — the
// very bug the tie-break exists to fix, pointed the other way.
//
// The counted files all live BENEATH dir, and that is an invariant rather than
// a happy accident: a symlink is skipped outright, so the walk neither descends
// through one nor counts one. Both halves matter. A symlinked DIRECTORY was
// already never descended, because DirEntry.IsDir is false for a link — but
// only incidentally, and the property this walk needs should not rest on a
// detail of how ReadDir reports dirent types. A symlinked FILE was counted, by
// its name alone, so `src/main/kotlin/A.kt` pointing anywhere on the
// filesystem added a Kotlin file to a project that contains none.
//
// Skipping rather than resolving is the deliberate half. Resolving a link and
// re-checking that the target is still under dir names a DIFFERENT file from
// the one a later read would open, which is the defect shape of #264 — the
// check and the syscall must name the same file. There is nothing to gain by
// resolving here: the count is a heuristic about what a project holds, and a
// link's target is by definition not part of the tree being measured.
func (p *workspacePool) walkLangCounts(dir string, maxDepth, maxFiles int, ignore []string, skipDir func(string) bool) (counts map[string]int, truncated bool) {
	counts = map[string]int{}
	if len(p.langsSnapshot()) == 0 {
		return counts, false
	}
	if skipDir == nil {
		skipDir = func(string) bool { return false }
	}
	type item struct {
		dir   string
		depth int
	}
	scanned := 0
	stack := []item{{dir: dir, depth: 0}}
	for len(stack) > 0 && scanned < maxFiles {
		it := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		entries, err := os.ReadDir(it.dir)
		if err != nil {
			continue
		}
		for _, de := range entries {
			// Neither descended nor counted — see the invariant above.
			if de.Type()&os.ModeSymlink != 0 {
				continue
			}
			if de.IsDir() {
				// Pruned outright — neither descended nor charged against the
				// budget. See tieSkipDir.
				if skipDir(de.Name()) {
					continue
				}
				if it.depth < maxDepth && !skipChildDir(de.Name()) {
					stack = append(stack, item{dir: filepath.Join(it.dir, de.Name()), depth: it.depth + 1})
				}
				continue
			}
			if scanned >= maxFiles {
				break
			}
			scanned++
			p.countSniffedFile(de.Name(), ignore, counts)
		}
	}
	// Hitting the cap means the counts describe a PREFIX of the tree in walk
	// order, not the tree. Reported rather than swallowed because the caller
	// cannot otherwise tell that from a complete count, and for the tie-break
	// the difference decides the answer. Conservative: a tree of exactly
	// maxFiles files reports truncated with nothing actually missed, which
	// costs only a fall back to the deterministic order.
	return counts, scanned >= maxFiles
}

// countSniffedFile charges one examined FILE against the walk's tally: an
// ignored marker is dropped — see walkLangCounts for why the markers are
// ignored wherever they sit — and any other file votes for the ACTIVE language
// that owns its extension. Extracted from walkLangCounts to keep the walk's
// cyclomatic headroom; it is the walk's innermost step, not a second policy.
func (p *workspacePool) countSniffedFile(name string, ignore []string, counts map[string]int) {
	if matchesAnyMarker(name, ignore) {
		return
	}
	if lang := p.fileLanguage(name); lang != "" {
		counts[lang]++
	}
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

// dominantAmong returns the candidate language owning the most source files in
// counts, or "" when none of them owns any. Restricting the count map to the
// tied candidates first is what keeps an unrelated third language — a stray
// build script, a vendored directory — from winning a tie it was never part of.
func dominantAmong(counts map[string]int, candidates []string) string {
	among := make(map[string]int, len(candidates))
	for _, name := range candidates {
		if n := counts[name]; n > 0 {
			among[name] = n
		}
	}
	return bestSniffedLang(among)
}

// contestedMarkerPatterns returns the root markers the tied languages claim, as
// the patterns the tie-break must refuse to count. A build script is often a
// source file of one of the very languages it is contested between —
// `build.gradle.kts` is Kotlin by extension — so counting it hands every Gradle
// project to Kotlin, including a Java one that merely uses the Kotlin DSL. These
// files are precisely the ones carrying no signal: their presence is why the tie
// exists.
//
// Patterns rather than resolved filenames, and applied at every level of the
// walk rather than only in dir, because the standard Gradle MULTI-project ships
// one build script per module. An earlier version discounted only the markers
// sitting directly in dir — the one place markerPresent looks — and every nested
// `app/build.gradle.kts` kept its vote, so a Java multi-project with no Kotlin in
// it resolved kotlin once it had more modules than the root had sources.
func contestedMarkerPatterns(matched []langConfig) []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range matched {
		for _, marker := range l.cfg.RootMarkers {
			if seen[marker] {
				continue
			}
			seen[marker] = true
			out = append(out, marker)
		}
	}
	return out
}

// matchesAnyMarker reports whether a bare file name matches one of the marker
// patterns — an exact name, or a glob for a marker carrying '*' ("*.xcodeproj").
// Matched against the base name alone, so it holds at any depth; filepath.Match
// never touches the filesystem, which keeps this free of the resolve-then-check
// hazard the walk avoids elsewhere.
func matchesAnyMarker(name string, patterns []string) bool {
	for _, p := range patterns {
		if strings.ContainsRune(p, '*') {
			if ok, err := filepath.Match(p, name); err == nil && ok {
				return true
			}
			continue
		}
		if p == name {
			return true
		}
	}
	return false
}
