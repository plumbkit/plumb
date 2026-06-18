package tools

import (
	"strconv"
	"strings"
)

// maxClosestCandidates caps how many alignment candidates we score when hunting
// for the near-match region, bounding the cost on large files.
const maxClosestCandidates = 64

// minClosestSimilarity is the fraction (0–1) of line content that must coincide
// between old_string and a candidate region before we surface it as a
// suggestion. Below this the "closest" match is noise and we stay silent.
const minClosestSimilarity = 0.5

// charAnchorFloor is the minimum character similarity an individual line must
// reach to seed a candidate window when no whitespace-only (trimmed-equal)
// anchor exists — i.e. the single-line-typo path.
const charAnchorFloor = 0.5

// closestMatchDiff returns a labelled unified diff between a not-found
// old_string (searched, after newline normalisation) and the most similar
// region actually present in content, or "" when nothing is similar enough.
//
// The diff reads as: '-' lines are what you searched for, '+' lines are what
// the file actually contains — so an agent can see the exact whitespace or
// token drift without a re-read. It is a suggestion, never an applied edit;
// the exactly-once contract still governs any real edit.
func closestMatchDiff(content, searched, path string) string {
	oldLines := diffSplitLines(searched)
	contentLines := diffSplitLines(content)
	if len(oldLines) == 0 || len(contentLines) == 0 {
		return ""
	}

	bestStart, bestScore := bestClosestWindow(oldLines, contentLines)
	if bestStart < 0 || bestScore < minClosestSimilarity {
		return ""
	}

	end := min(bestStart+len(oldLines), len(contentLines))
	script := computeEditScript(oldLines, contentLines[bestStart:end])
	diff := renderUnifiedDiff(path, script)
	if diff == "" {
		return ""
	}

	var b strings.Builder
	b.WriteString("  closest match in the file is near line ")
	b.WriteString(strconv.Itoa(bestStart + 1))
	b.WriteString(" (\"-\" = your old_string, \"+\" = current file content; suggestion only, not applied):\n")
	b.WriteString(diff)
	return b.String()
}

// bestClosestWindow scores every candidate window proposed by
// closestCandidateStarts and returns the best-scoring start index and its
// similarity. Returns (-1, 0) when there are no candidates.
func bestClosestWindow(oldLines, contentLines []string) (int, float64) {
	bestStart, bestScore := -1, 0.0
	for _, s := range closestCandidateStarts(oldLines, contentLines) {
		end := min(s+len(oldLines), len(contentLines))
		score := lineWindowSimilarity(oldLines, contentLines[s:end])
		if score > bestScore {
			bestStart, bestScore = s, score
		}
	}
	return bestStart, bestScore
}

// closestCandidateStarts proposes window start indices in contentLines. The
// primary signal is whitespace-only drift: each non-blank old line is aligned
// against content lines with the same trimmed text (the dominant cause of a
// failed match). When no such anchor exists — e.g. a single-line typo — it
// falls back to the content line most character-similar to the first old line.
// Results are deduplicated and capped at maxClosestCandidates.
func closestCandidateStarts(oldLines, contentLines []string) []int {
	byTrim := make(map[string][]int, len(contentLines))
	for ci, ln := range contentLines {
		if t := strings.TrimSpace(ln); t != "" {
			byTrim[t] = append(byTrim[t], ci)
		}
	}

	seen := make(map[int]struct{})
	var starts []int
	for oi, ln := range oldLines {
		t := strings.TrimSpace(ln)
		if t == "" {
			continue
		}
		for _, ci := range byTrim[t] {
			s := ci - oi
			if s < 0 {
				continue
			}
			if _, ok := seen[s]; ok {
				continue
			}
			seen[s] = struct{}{}
			starts = append(starts, s)
			if len(starts) >= maxClosestCandidates {
				return starts
			}
		}
	}

	if len(starts) == 0 {
		if s := bestCharAnchor(oldLines, contentLines); s >= 0 {
			starts = append(starts, s)
		}
	}
	return starts
}

// bestCharAnchor returns a window start derived from the content line most
// character-similar to the first non-blank old line, or -1 when nothing clears
// charAnchorFloor. This rescues the typo case, where no line is trimmed-equal.
func bestCharAnchor(oldLines, contentLines []string) int {
	anchor, anchorIdx := "", -1
	for oi, ln := range oldLines {
		if t := strings.TrimSpace(ln); t != "" {
			anchor, anchorIdx = t, oi
			break
		}
	}
	if anchorIdx < 0 {
		return -1
	}

	best, bestSim := -1, 0.0
	for ci, ln := range contentLines {
		if sim := stringSimilarity(anchor, strings.TrimSpace(ln)); sim > bestSim {
			best, bestSim = ci, sim
		}
	}
	if bestSim < charAnchorFloor {
		return -1
	}
	if s := best - anchorIdx; s >= 0 {
		return s
	}
	return -1
}

// lineWindowSimilarity scores how well window b reproduces old block a, as the
// mean per-line similarity over the longer of the two lengths (so a length
// mismatch is penalised). Each line pair is scored whitespace- and typo-
// tolerantly by lineSim.
func lineWindowSimilarity(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	n := min(len(a), len(b))
	total := 0.0
	for i := 0; i < n; i++ {
		total += lineSim(a[i], b[i])
	}
	return total / float64(max(len(a), len(b)))
}

// lineSim scores two lines: 1.0 when equal or whitespace-only different,
// otherwise their character (bigram Dice) similarity over the trimmed text.
func lineSim(a, b string) float64 {
	if a == b {
		return 1
	}
	ta, tb := strings.TrimSpace(a), strings.TrimSpace(b)
	if ta == tb {
		return 1
	}
	return stringSimilarity(ta, tb)
}

// stringSimilarity is the Sørensen–Dice coefficient over adjacent-character
// bigrams: 2·|shared| / (|a|+|b|). Cheap, order-tolerant, and robust to small
// typos. Returns 1.0 for identical strings and 0.0 when either is too short to
// form a bigram and they differ.
func stringSimilarity(a, b string) float64 {
	if a == b {
		return 1
	}
	ba, bb := bigrams(a), bigrams(b)
	if len(ba) == 0 || len(bb) == 0 {
		return 0
	}
	counts := make(map[string]int, len(ba))
	for _, g := range ba {
		counts[g]++
	}
	inter := 0
	for _, g := range bb {
		if counts[g] > 0 {
			counts[g]--
			inter++
		}
	}
	return 2 * float64(inter) / float64(len(ba)+len(bb))
}

// bigrams returns the adjacent-rune bigrams of s (rune-aware, so multi-byte
// characters are not split). Returns nil when s has fewer than two runes.
func bigrams(s string) []string {
	r := []rune(s)
	if len(r) < 2 {
		return nil
	}
	out := make([]string, 0, len(r)-1)
	for i := 0; i+1 < len(r); i++ {
		out = append(out, string(r[i:i+2]))
	}
	return out
}
