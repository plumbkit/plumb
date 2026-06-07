package main

import (
	"fmt"
	"sort"
	"strings"
)

const candidateN = 50 // FTS/semantic candidate window feeding rerank/rrf

// arm is one ranking strategy producing node ids best-first.
type arm struct {
	name string
	rank func(qi int) []int64
}

type armScore struct {
	name    string
	recall  float64
	mrr     float64
	perFirst []int // first-relevant rank per query (0 = not found in window)
}

func evaluate(dbPath string, k int, corpus []symbol, byID map[int64]symbol, corpusVecs, qVecs [][]float32) string {
	idToVec := make(map[int64][]float32, len(corpus))
	for i, s := range corpus {
		idToVec[s.ID] = corpusVecs[i]
	}

	semanticRank := func(qi int) []int64 {
		type scored struct {
			id    int64
			score float64
		}
		all := make([]scored, len(corpus))
		for i, s := range corpus {
			all[i] = scored{s.ID, cosine(qVecs[qi], corpusVecs[i])}
		}
		sort.SliceStable(all, func(a, b int) bool { return all[a].score > all[b].score })
		out := make([]int64, 0, candidateN)
		for i := 0; i < candidateN && i < len(all); i++ {
			out = append(out, all[i].id)
		}
		return out
	}

	ftsRank := func(qi int) []int64 {
		ids, err := ftsSearch(dbPath, queries[qi].q, candidateN)
		if err != nil {
			fmt.Println("fts error:", err)
		}
		return ids
	}

	rerankRank := func(qi int) []int64 {
		cands := ftsRank(qi) // FTS5 spine: only re-order what FTS5 already surfaced
		sort.SliceStable(cands, func(a, b int) bool {
			return cosine(qVecs[qi], idToVec[cands[a]]) > cosine(qVecs[qi], idToVec[cands[b]])
		})
		return cands
	}

	rrfRank := func(qi int) []int64 {
		return rrf(ftsRank(qi), semanticRank(qi))
	}

	arms := []arm{
		{"fts (baseline)", ftsRank},
		{"semantic", semanticRank},
		{"rerank (fts spine)", rerankRank},
		{"rrf (fusion)", rrfRank},
	}

	scores := make([]armScore, len(arms))
	for ai, a := range arms {
		sc := armScore{name: a.name, perFirst: make([]int, len(queries))}
		for qi, ql := range queries {
			ranked := a.rank(qi)
			names := namesOf(ranked, byID)
			rel := toSet(ql.relevant)
			found, first := 0, 0
			seen := map[string]bool{}
			for pos, n := range names {
				if pos >= k {
					break
				}
				if rel[n] && !seen[n] {
					seen[n] = true
					found++
					if first == 0 {
						first = pos + 1
					}
				}
			}
			sc.recall += float64(found) / float64(len(ql.relevant))
			if first > 0 {
				sc.mrr += 1.0 / float64(first)
			}
			sc.perFirst[qi] = first
		}
		sc.recall /= float64(len(queries))
		sc.mrr /= float64(len(queries))
		scores[ai] = sc
	}

	return formatReport(k, len(corpus), scores)
}

// rrf fuses two rankings with reciprocal-rank fusion (k0=60, the standard).
func rrf(a, b []int64) []int64 {
	const k0 = 60.0
	score := map[int64]float64{}
	add := func(ids []int64) {
		for i, id := range ids {
			score[id] += 1.0 / (k0 + float64(i+1))
		}
	}
	add(a)
	add(b)
	ids := make([]int64, 0, len(score))
	for id := range score {
		ids = append(ids, id)
	}
	sort.SliceStable(ids, func(i, j int) bool {
		if score[ids[i]] != score[ids[j]] {
			return score[ids[i]] > score[ids[j]]
		}
		return ids[i] < ids[j]
	})
	return ids
}

func namesOf(ids []int64, byID map[int64]symbol) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, byID[id].Name)
	}
	return out
}

func toSet(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func formatReport(k, corpusSize int, scores []armScore) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# Semantic-search spike — results\n\n")
	fmt.Fprintf(&sb, "Corpus: %d code symbols (function/method/type) from plumb's topology index. "+
		"Embedder: `%s`. Queries: %d hand-labelled. Cutoff: recall@%d / MRR@%d.\n\n",
		corpusSize, embedModel, len(queries), k, k)

	fmt.Fprintf(&sb, "## Aggregate\n\n| arm | recall@%d | MRR@%d |\n|---|---|---|\n", k, k)
	for _, s := range scores {
		fmt.Fprintf(&sb, "| %s | %.3f | %.3f |\n", s.name, s.recall, s.mrr)
	}
	sb.WriteString("\n")

	fmt.Fprintf(&sb, "## First-relevant rank per query (0 = not found in top %d)\n\n", candidateN)
	sb.WriteString("| query |")
	for _, s := range scores {
		fmt.Fprintf(&sb, " %s |", shortArm(s.name))
	}
	sb.WriteString("\n|---|")
	for range scores {
		sb.WriteString("---|")
	}
	sb.WriteString("\n")
	for qi, ql := range queries {
		fmt.Fprintf(&sb, "| %s |", truncQ(ql.q))
		for _, s := range scores {
			fmt.Fprintf(&sb, " %s |", rankCell(s.perFirst[qi]))
		}
		sb.WriteString("\n")
	}
	sb.WriteString("\n")

	sb.WriteString(verdict(scores))
	return sb.String()
}

func shortArm(name string) string {
	if i := strings.IndexByte(name, ' '); i > 0 {
		return name[:i]
	}
	return name
}

func truncQ(q string) string {
	if len(q) > 44 {
		return q[:44] + "…"
	}
	return q
}

func rankCell(r int) string {
	if r == 0 {
		return "—"
	}
	return fmt.Sprintf("%d", r)
}

// verdict turns the aggregate deltas into a plain build/don't-build recommendation.
func verdict(scores []armScore) string {
	byName := map[string]armScore{}
	for _, s := range scores {
		byName[shortArm(s.name)] = s
	}
	fts, sem, rerank, rrfS := byName["fts"], byName["semantic"], byName["rerank"], byName["rrf"]
	best := sem
	for _, s := range []armScore{rerank, rrfS} {
		if s.recall+s.mrr > best.recall+best.mrr {
			best = s
		}
	}
	dRecall := best.recall - fts.recall
	dMRR := best.mrr - fts.mrr

	var sb strings.Builder
	sb.WriteString("## Verdict\n\n")
	fmt.Fprintf(&sb, "Best semantic arm: **%s** — recall@k %+.3f, MRR@k %+.3f vs the FTS5 baseline.\n\n",
		best.name, dRecall, dMRR)
	switch {
	case dRecall >= 0.10 || dMRR >= 0.10:
		sb.WriteString("**Clear win.** Semantic re-rank materially beats FTS5 on these vocabulary-gap " +
			"queries. Proceed to build the opt-in hybrid (FTS5 spine + semantic re-rank, behind a build tag).\n")
	case dRecall <= 0.02 && dMRR <= 0.02:
		sb.WriteString("**No clear win.** Semantic does not materially beat FTS5 here — the risk of " +
			"'lexical overlap dressed up as semantic' is real. Close Phase 7 as evaluated, not built.\n")
	default:
		sb.WriteString("**Marginal.** Semantic helps some queries but the aggregate gain is small. " +
			"Worth a second look with a larger labelled set before committing to the build.\n")
	}
	return sb.String()
}
