package memory

import (
	"context"
	"fmt"
	"strings"
)

// Hit is one ranked memory search result.
type Hit struct {
	Name        string
	Description string
	Snippet     string
	Field       string // which FTS column most likely matched
	Confidence  string
	Score       float64 // higher is better (negated FTS rank)
}

// SearchOpts controls a memory FTS search.
type SearchOpts struct {
	Limit    int
	Snippets bool
}

// Search runs a ranked FTS5 query over the indexed memories. Results are ordered
// by BM25 with a small bonus for user-authored memories over generated ones and
// a recency tiebreak. Returns an empty slice (not an error) when nothing matches.
func (ix *Index) Search(ctx context.Context, query string, opts SearchOpts) ([]Hit, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("memory: search query is empty")
	}
	if opts.Limit <= 0 {
		opts.Limit = 20
	}
	ftsQuery := buildMemoryFTSQuery(query)

	ix.mu.Lock()
	defer ix.mu.Unlock()
	rows, err := ix.db.QueryContext(ctx, `
		SELECT f.rowid, f.name, f.name_tokens, f.description, f.body,
		       f.path_globs, f.source_paths, f.source_symbols, f.rank,
		       r.confidence
		FROM memory_fts f
		JOIN memory_records r ON r.id = f.rowid
		WHERE memory_fts MATCH ?
		ORDER BY f.rank + (CASE r.confidence WHEN 'user' THEN -1.0 ELSE 0.0 END) ASC,
		         r.last_used_at DESC
		LIMIT ?`, ftsQuery, opts.Limit)
	if err != nil {
		return nil, fmt.Errorf("memory: fts query: %w", err)
	}
	defer rows.Close()
	return collectHits(rows, query, opts)
}

func collectHits(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}, query string, opts SearchOpts,
) ([]Hit, error) {
	var hits []Hit
	for rows.Next() {
		var (
			rowid                                          int64
			name, tokens, desc, body, globs, spaths, ssyms string
			rank                                           float64
			confidence                                     string
		)
		if err := rows.Scan(&rowid, &name, &tokens, &desc, &body, &globs, &spaths, &ssyms, &rank, &confidence); err != nil {
			continue
		}
		h := Hit{
			Name:        name,
			Description: desc,
			Field:       matchMemoryField(query, name, tokens, desc, body, globs),
			Confidence:  confidence,
			Score:       -rank,
		}
		if opts.Snippets {
			h.Snippet = memorySnippet(desc, body)
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// buildMemoryFTSQuery turns a free-text query into an FTS5 OR-of-terms, quoting
// each term so punctuation in identifiers is treated literally.
func buildMemoryFTSQuery(query string) string {
	terms := strings.Fields(query)
	if len(terms) == 0 {
		return query
	}
	quoted := make([]string, 0, len(terms))
	for _, t := range terms {
		quoted = append(quoted, `"`+strings.ReplaceAll(t, `"`, ``)+`"`)
	}
	return strings.Join(quoted, " OR ")
}

// matchMemoryField returns the FTS column most likely responsible for the match,
// by substring-checking the query terms. Heuristic — FTS5 does not expose the
// matched column without fts5_highlight().
func matchMemoryField(query, name, tokens, desc, body, globs string) string {
	nl, tl, dl, bl, gl := strings.ToLower(name), strings.ToLower(tokens),
		strings.ToLower(desc), strings.ToLower(body), strings.ToLower(globs)
	for t := range strings.FieldsSeq(strings.ToLower(query)) {
		t = strings.Trim(t, `"`)
		if t == "" {
			continue
		}
		switch {
		case strings.Contains(nl, t):
			return "name"
		case strings.Contains(tl, t):
			return "name_tokens"
		case strings.Contains(dl, t):
			return "description"
		case strings.Contains(gl, t):
			return "path"
		case strings.Contains(bl, t):
			return "body"
		}
	}
	return "name"
}

func memorySnippet(desc, body string) string {
	if desc != "" {
		return truncateMemory(desc, 120)
	}
	return truncateMemory(firstNonBlankLine(body), 120)
}

func firstNonBlankLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			return t
		}
	}
	return ""
}

func truncateMemory(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
