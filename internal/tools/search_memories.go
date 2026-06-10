package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/plumbkit/plumb/internal/memory"
)

type searchMemoriesTool struct {
	ws      WorkspaceFn
	guard   BoundaryGuard
	indexFn func() *memory.Index
}

func NewSearchMemories(ws WorkspaceFn) *searchMemoriesTool {
	return &searchMemoriesTool{ws: ws}
}

func (t *searchMemoriesTool) WithBoundary(guard BoundaryGuard) *searchMemoriesTool {
	t.guard = guard
	return t
}

// WithIndex wires the per-connection memory FTS index for ranked search.
func (t *searchMemoriesTool) WithIndex(fn func() *memory.Index) *searchMemoriesTool {
	t.indexFn = fn
	return t
}

func (*searchMemoriesTool) Name() string { return "search_memories" }

func (*searchMemoriesTool) Description() string {
	return `Search saved memories for a workspace.

When the FTS5 memory index is available and fresh, returns ranked hits (by relevance, with a bonus for user-authored memories) annotated source=memory-fts. Otherwise falls back to a deterministic grep over the markdown files, returning each match with the memory name and line. Smart-case (case-insensitive if 'pattern' is all lowercase) unless 'case_sensitive' is set; 'use_regex' forces the grep path. 'mode' (auto|fts|grep) overrides the choice; default auto.

Memory-only corpus with a deterministic grep fallback — for ranked discovery across code, docs, AND memories in one call, use workspace_search instead. Useful when you don't know which memory contains a piece of context — much faster than reading every memory.`
}

func (*searchMemoriesTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"pattern":{"type":"string","description":"Text or regex pattern to search for."},
			"use_regex":{"type":"boolean","default":false,"description":"Treat pattern as a regex. Forces the grep path (FTS is not regex)."},
			"case_sensitive":{"type":"boolean","description":"Default: smart-case. Setting this forces the grep path (FTS is case-insensitive)."},
			"mode":{"type":"string","enum":["auto","fts","grep"],"description":"Search strategy. auto (default): ranked FTS when the index is fresh, falling back to grep when the index is stale OR FTS finds no hits (FTS matches whole tokens, grep matches substrings). fts: force ranked FTS (reindex if stale; keeps an empty result). grep: force literal/regex grep."},
			"workspace":{"type":"string","description":"Absolute workspace path. Defaults to the daemon's resolved workspace."}
		},
		"required":["pattern"],
  "additionalProperties": false
}`)
}

type searchMemoriesArgs struct {
	Pattern       string `json:"pattern"`
	UseRegex      bool   `json:"use_regex"`
	CaseSensitive *bool  `json:"case_sensitive,omitempty"`
	Mode          string `json:"mode"`
	Workspace     string `json:"workspace"`
}

func parseSearchMemoriesArgs(args json.RawMessage) (searchMemoriesArgs, error) {
	var a searchMemoriesArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return a, fmt.Errorf("invalid args: %w", err)
	}
	if a.Pattern == "" {
		return a, fmt.Errorf("`pattern` is required")
	}
	a.Mode = strings.ToLower(strings.TrimSpace(a.Mode))
	return a, nil
}

func (t *searchMemoriesTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	a, err := parseSearchMemoriesArgs(args)
	if err != nil {
		return "", err
	}
	ws := resolveWorkspace(a.Workspace, t.ws)
	if ws == "" {
		return "", noWorkspaceError()
	}
	if err := t.guard.check(ws); err != nil {
		return "", fmt.Errorf("search_memories: %w", err)
	}
	if res, handled := t.searchFTS(ctx, resolveMemoryIndex(t.indexFn, ws), ws, a); handled {
		return res, nil
	}
	return t.searchGrep(a, ws)
}

// searchFTS runs the ranked FTS path when an index is present and either fresh
// (mode auto) or force-reindexed (mode fts). Returns handled=false to defer to
// grep. It defers when: mode=grep, a regex or case-sensitive query (FTS5's
// unicode61 tokeniser is whole-token and case-insensitive), no index, a stale
// auto query, any error, OR (mode auto) zero FTS hits — because FTS cannot
// represent a substring the tokeniser splits away (e.g. "essio" for
// "UserSession"), so grep, which does substring matching, must get a chance.
// mode=fts keeps the explicit empty FTS result. A stale auto query also kicks an
// async reindex so subsequent queries self-heal back onto FTS.
func (t *searchMemoriesTool) searchFTS(ctx context.Context, ix *memory.Index, ws string, a searchMemoriesArgs) (string, bool) {
	if a.Mode == "grep" || a.UseRegex || ix == nil || (a.CaseSensitive != nil && *a.CaseSensitive) {
		return "", false
	}
	fresh, _ := ix.Fresh(ws)
	if !fresh {
		if a.Mode != "fts" {
			ix.ReindexAsync(ws) // self-heal for the next query; this one greps
			return "", false    // auto + stale → grep
		}
		if _, err := ix.Reindex(ws); err != nil {
			return "", false
		}
	}
	hits, err := ix.Search(ctx, a.Pattern, memory.SearchOpts{Limit: 50, Snippets: true})
	if err != nil {
		return "", false
	}
	if len(hits) == 0 && a.Mode != "fts" {
		return "", false // auto + zero FTS hits → let grep try a substring match
	}
	return formatMemoryHits(a.Pattern, hits), true
}

func (t *searchMemoriesTool) searchGrep(a searchMemoriesArgs, ws string) (string, error) {
	re, err := buildMemoryRegex(a.Pattern, a.UseRegex, a.CaseSensitive)
	if err != nil {
		return "", err
	}
	return runMemorySearch(re, a.Pattern, ws)
}

// resolveMemoryIndex returns the index only when it belongs to ws, so a tool
// whose workspace arg differs from the connection's pinned workspace never reads
// a mismatched index.
func resolveMemoryIndex(fn func() *memory.Index, ws string) *memory.Index {
	if fn == nil {
		return nil
	}
	ix := fn()
	if ix == nil || ix.Workspace() != ws {
		return nil
	}
	return ix
}

func formatMemoryHits(pattern string, hits []memory.Hit) string {
	if len(hits) == 0 {
		return fmt.Sprintf("No memory matches for %q (source=memory-fts, mode=ranked, index=up-to-date).", pattern)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "memory search: %d hit(s) for %q (source=memory-fts, mode=ranked, index=up-to-date)\n\n", len(hits), pattern)
	for _, h := range hits {
		fmt.Fprintf(&sb, "  %s", h.Name)
		if h.Confidence != "" && h.Confidence != "user" {
			fmt.Fprintf(&sb, " [%s]", h.Confidence)
		}
		fmt.Fprintf(&sb, "  (field=%s, score=%.3f)\n", h.Field, h.Score)
		if snip := h.Description; snip != "" {
			fmt.Fprintf(&sb, "    %s\n", snip)
		} else if h.Snippet != "" {
			fmt.Fprintf(&sb, "    %s\n", h.Snippet)
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

// buildMemoryRegex compiles the search pattern. Smart-case applies when
// caseSensitive is nil: case-insensitive iff the pattern is all lowercase.
func buildMemoryRegex(pattern string, useRegex bool, caseSensitive *bool) (*regexp.Regexp, error) {
	cs := !allLower(pattern)
	if caseSensitive != nil {
		cs = *caseSensitive
	}
	flags := ""
	if !cs {
		flags = "(?i)"
	}
	if useRegex {
		re, err := regexp.Compile(flags + pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid regex: %w", err)
		}
		return re, nil
	}
	return regexp.MustCompile(flags + regexp.QuoteMeta(pattern)), nil
}

func runMemorySearch(re *regexp.Regexp, pattern, ws string) (string, error) {
	mems, err := memory.List(ws)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	hits := 0
	for _, m := range mems {
		body, err := memory.Read(ws, m.Name)
		if err != nil {
			continue
		}
		for i, line := range strings.Split(body, "\n") {
			if re.MatchString(line) {
				if hits == 0 {
					fmt.Fprintf(&sb, "Matches for %q in %s/.plumb/memories/:\n\n", pattern, ws)
				}
				fmt.Fprintf(&sb, "  %s:%d  %s\n", m.Name, i+1, strings.TrimSpace(line))
				hits++
			}
		}
	}
	if hits == 0 {
		return fmt.Sprintf("No matches for %q in any memory.", pattern), nil
	}
	fmt.Fprintf(&sb, "\n%d match(es) across %d memor(ies).", hits, countMemoriesWithMatches(re, ws))
	return sb.String(), nil
}

func countMemoriesWithMatches(re *regexp.Regexp, ws string) int {
	mems, _ := memory.List(ws)
	n := 0
	for _, m := range mems {
		body, err := memory.Read(ws, m.Name)
		if err != nil {
			continue
		}
		if re.MatchString(body) {
			n++
		}
	}
	return n
}
