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
	// Over-fetch to allow kind filtering without losing top results.
	rows, err := db.QueryContext(ctx,
		`SELECT rowid, name, name_tokens, qualified, signature, docstring, path, kind, rank
         FROM topology_fts
         WHERE topology_fts MATCH ?
         ORDER BY rank
         LIMIT ?`, ftsQuery, opts.Limit*3)
	if err != nil {
		return nil, fmt.Errorf("topology: fts query: %w", err)
	}
	defer rows.Close()
	return collectSearchResults(db, rows, opts)
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

func collectSearchResults(db *sql.DB, rows *sql.Rows, opts SearchOpts) ([]SearchResult, error) {
	var results []SearchResult
	for rows.Next() && len(results) < opts.Limit {
		var (
			rowid                                    int64
			name, tokens, qual, sig, doc, path, kind string
			rank                                     float64
		)
		if err := rows.Scan(&rowid, &name, &tokens, &qual, &sig, &doc, &path, &kind, &rank); err != nil {
			continue
		}
		if !matchesKindOpts(kind, opts) {
			continue
		}
		n, err := nodeByID(db, rowid)
		if err != nil {
			continue
		}
		n.Path = path
		field := matchField(opts.Snippets, name, tokens, qual, sig, doc)
		results = append(results, SearchResult{
			Node:    n,
			Score:   -rank, // FTS5 rank is negative; higher (less negative) is better
			Field:   field,
			Snippet: buildSnippet(name, tokens, sig),
		})
	}
	return results, rows.Err()
}

func matchesKindOpts(kind string, opts SearchOpts) bool {
	if len(opts.Kinds) == 0 {
		return true
	}
	for _, k := range opts.Kinds {
		if k == kind {
			return true
		}
	}
	return false
}

func nodeByID(db *sql.DB, id int64) (Node, error) {
	var n Node
	row := db.QueryRow(
		`SELECT n.id, n.file_id, n.kind, n.name, n.qualified, n.signature,
                n.start_line, n.end_line, n.docstring, n.language, f.path
         FROM topology_nodes n
         JOIN topology_files f ON f.id = n.file_id
         WHERE n.id = ?`, id)
	err := row.Scan(&n.ID, &n.FileID, &n.Kind, &n.Name, &n.Qualified, &n.Signature,
		&n.StartLine, &n.EndLine, &n.Docstring, &n.Language, &n.Path)
	return n, err
}

// matchField returns the FTS field that most likely caused the match.
// The snippets parameter is kept for future use but currently unused.
func matchField(_ bool, name, tokens, qual, sig, doc string) string {
	switch {
	case name != "":
		return "name"
	case tokens != "":
		return "name_tokens"
	case qual != "":
		return "qualified"
	case sig != "":
		return "signature"
	case doc != "":
		return "docstring"
	default:
		return "name"
	}
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
	return s[:n] + "…"
}
