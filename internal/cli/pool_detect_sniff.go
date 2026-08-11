package cli

import (
	"os"
	"path/filepath"
	"strings"
)

// Deciding a language from FILE EVIDENCE, as opposed to from the marker walk in
// pool_detect.go. Two callers want the same underlying count and read it
// differently: extLangAt as a last resort when no marker named a language at
// all, and strongLangAt's tie-break when several markers named different ones
// for the same directory.

// extScanDepth / extScanMaxFiles bound the content sniff so it can never stall
// detection on a large tree: it descends at most this many levels below the root
// and examines at most this many files before giving up.
const (
	extScanDepth    = 2
	extScanMaxFiles = 2000
	// tieScanDepth is deeper than extScanDepth because the tie it breaks is a
	// JVM one, and the JVM convention buries sources three levels down
	// (src/main/kotlin/App.kt) before any package directories. At extScanDepth
	// the scan would reach src/main/ and count nothing at all, making every
	// contested Gradle root look empty. extScanMaxFiles still bounds the walk.
	tieScanDepth = 5
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
	return bestSniffedLang(p.sniffCounts(dir, extScanDepth))
}

// sniffCounts counts source files per ACTIVE language in a bounded shallow scan
// of dir, descending at most maxDepth levels and examining at most
// extScanMaxFiles files. Shared by the last-resort language sniff (extLangAt)
// and the contested-root-marker tie-break (strongLangAt), which want the same
// evidence at different depths. Defensive throughout — any read error skips that
// entry rather than failing, so detection never crashes on an odd filesystem.
func (p *workspacePool) sniffCounts(dir string, maxDepth int) map[string]int {
	counts := map[string]int{}
	if len(p.langsSnapshot()) == 0 {
		return counts
	}
	type item struct {
		dir   string
		depth int
	}
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
				if it.depth < maxDepth && !skipChildDir(de.Name()) {
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
	return counts
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

// discountMarkers removes the contested root-marker files themselves from a
// tie-break count. A build script is often a source file of one of the very
// languages it is contested between — `build.gradle.kts` is Kotlin by extension
// — so counting it hands every Gradle project to Kotlin, including a Java one
// that merely uses the Kotlin DSL. These files are precisely the ones carrying
// no signal: their presence is why the tie exists. Only markers sitting directly
// in dir are discounted, which is the only place markerPresent looks.
func (p *workspacePool) discountMarkers(counts map[string]int, dir string, matched []langConfig) {
	seen := map[string]bool{}
	for _, l := range matched {
		for _, marker := range l.cfg.RootMarkers {
			for _, name := range markerFileNames(dir, marker) {
				if seen[name] {
					continue
				}
				seen[name] = true
				if lang := p.fileLanguage(name); lang != "" && counts[lang] > 0 {
					counts[lang]--
				}
			}
		}
	}
}

// markerFileNames returns the base names the marker actually matches in dir:
// the marker itself when it is a literal filename that exists, or every glob
// match when it contains '*'. Empty when nothing matches.
func markerFileNames(dir, marker string) []string {
	if !strings.ContainsRune(marker, '*') {
		if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
			return []string{marker}
		}
		return nil
	}
	matches, _ := filepath.Glob(filepath.Join(dir, marker))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, filepath.Base(m))
	}
	return out
}
