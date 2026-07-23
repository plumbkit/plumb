package topology

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Search performs a ranked FTS5 search over the topology index.
func Search(ctx context.Context, db *sql.DB, query string, opts SearchOpts) ([]SearchResult, error) {
	if opts.Limit <= 0 {
		opts.Limit = 20
	}
	if query == "" {
		return nil, fmt.Errorf("topology: search query is empty")
	}
	ftsQuery := buildFTSQuery(query)
	sqlStr, args := buildSearchSQL(ftsQuery, opts)
	rows, err := db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, fmt.Errorf("topology: fts query: %w", err)
	}
	defer rows.Close()
	return collectSearchResults(rows, query, opts)
}

// buildSearchSQL builds the FTS5 search query with optional kind and language
// filters pushed into SQL, avoiding an over-fetch + post-scan kind filter.
//
//nolint:gosec // G202: WHERE clause built from constant strings and bind params; no user data interpolated
func buildSearchSQL(ftsQuery string, opts SearchOpts) (string, []any) {
	args := []any{ftsQuery}
	where := `WHERE topology_fts MATCH ?`

	if len(opts.Kinds) > 0 {
		ph := strings.Repeat("?,", len(opts.Kinds))
		where += ` AND n.kind IN (` + ph[:len(ph)-1] + `)`
		for _, k := range opts.Kinds {
			args = append(args, k)
		}
	}
	if opts.Language != "" {
		where += ` AND n.language = ?`
		args = append(args, opts.Language)
	}
	args = append(args, opts.Limit)

	// JOIN topology_nodes inline to avoid a per-row nodeByID roundtrip (N+1 → 1 query).
	return `SELECT fts.rowid, fts.name, fts.name_tokens, fts.qualified, fts.signature,
		        fts.docstring, fts.path, fts.kind, fts.rank,
		        n.start_line, n.end_line, n.language, n.file_id
		 FROM topology_fts fts
		 JOIN topology_nodes n ON n.id = fts.rowid
		 ` + where + `
		 ORDER BY fts.rank
		 LIMIT ?`, args
}

func buildFTSQuery(query string) string {
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

func collectSearchResults(rows *sql.Rows, query string, opts SearchOpts) ([]SearchResult, error) {
	var results []SearchResult
	for rows.Next() && len(results) < opts.Limit {
		var (
			rowid                                    int64
			name, tokens, qual, sig, doc, path, kind string
			rank                                     float64
			startLine, endLine                       int
			language                                 string
			fileID                                   int64
		)
		if err := rows.Scan(&rowid, &name, &tokens, &qual, &sig, &doc, &path, &kind, &rank,
			&startLine, &endLine, &language, &fileID); err != nil {
			continue
		}
		n := Node{
			ID:        rowid,
			FileID:    fileID,
			Kind:      NodeKind(kind),
			Name:      name,
			Qualified: qual,
			Signature: sig,
			Docstring: doc,
			StartLine: startLine,
			EndLine:   endLine,
			Language:  language,
			Path:      path,
		}
		var snippet string
		if opts.Snippets {
			snippet = buildSnippet(name, tokens, sig)
		}
		results = append(results, SearchResult{
			Node:    n,
			Score:   -rank, // FTS5 rank is negative; higher (less negative) is better
			Field:   matchField(query, name, tokens, qual, sig, doc),
			Snippet: snippet,
		})
	}
	return results, rows.Err()
}

// matchField returns the FTS column most likely responsible for the match by
// checking which field contains the query terms as substrings. This is a
// heuristic — FTS5 does not expose which column matched without fts5_highlight().
// Priority: name → name_tokens → qualified → signature → docstring.
func matchField(query, name, tokens, qual, sig, doc string) string {
	nl := strings.ToLower(name)
	tl := strings.ToLower(tokens)
	ql := strings.ToLower(qual)
	sl := strings.ToLower(sig)
	dl := strings.ToLower(doc)
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
		case strings.Contains(ql, t):
			return "qualified"
		case strings.Contains(sl, t):
			return "signature"
		case strings.Contains(dl, t):
			return "docstring"
		}
	}
	return "name"
}

func buildSnippet(name, tokens, sig string) string {
	if name != "" {
		return name
	}
	if tokens != "" {
		return tokens
	}
	if sig != "" {
		return truncate(sig, 80)
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Count by runes so a multi-byte character is never split mid-encoding
	// (byte slicing s[:n] could land inside a UTF-8 sequence).
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}
