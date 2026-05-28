package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/golimpio/plumb/internal/memory"
)

type searchMemoriesTool struct {
	ws    WorkspaceFn
	guard BoundaryGuard
}

func NewSearchMemories(ws WorkspaceFn) *searchMemoriesTool {
	return &searchMemoriesTool{ws: ws}
}

func (t *searchMemoriesTool) WithBoundary(guard BoundaryGuard) *searchMemoriesTool {
	t.guard = guard
	return t
}

func (*searchMemoriesTool) Name() string { return "search_memories" }

func (*searchMemoriesTool) Description() string {
	return `Grep across saved memories for a workspace.

Returns each match with the memory name and the matching line. Smart-case (case-insensitive if 'pattern' is all lowercase) unless 'case_sensitive' is set. Set 'use_regex' for regex patterns.

Useful when you don't know which memory contains a piece of context — much faster than reading every memory.`
}

func (*searchMemoriesTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"pattern":{"type":"string","description":"Text or regex pattern to search for."},
			"use_regex":{"type":"boolean","default":false},
			"case_sensitive":{"type":"boolean","description":"Default: smart-case."},
			"workspace":{"type":"string","description":"Absolute workspace path. Defaults to the daemon's resolved workspace."}
		},
		"required":["pattern"],
  "additionalProperties": false
}`)
}

func (t *searchMemoriesTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Pattern       string `json:"pattern"`
		UseRegex      bool   `json:"use_regex"`
		CaseSensitive *bool  `json:"case_sensitive,omitempty"`
		Workspace     string `json:"workspace"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if a.Pattern == "" {
		return "", fmt.Errorf("`pattern` is required")
	}
	ws := resolveWorkspace(a.Workspace, t.ws)
	if ws == "" {
		return "", noWorkspaceError()
	}
	if err := t.guard.check(ws); err != nil {
		return "", fmt.Errorf("search_memories: %w", err)
	}

	caseSensitive := !allLower(a.Pattern)
	if a.CaseSensitive != nil {
		caseSensitive = *a.CaseSensitive
	}
	var re *regexp.Regexp
	flags := ""
	if !caseSensitive {
		flags = "(?i)"
	}
	if a.UseRegex {
		var err error
		re, err = regexp.Compile(flags + a.Pattern)
		if err != nil {
			return "", fmt.Errorf("invalid regex: %w", err)
		}
	} else {
		re = regexp.MustCompile(flags + regexp.QuoteMeta(a.Pattern))
	}

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
					fmt.Fprintf(&sb, "Matches for %q in %s/.plumb/memories/:\n\n", a.Pattern, ws)
				}
				fmt.Fprintf(&sb, "  %s:%d  %s\n", m.Name, i+1, strings.TrimSpace(line))
				hits++
			}
		}
	}
	if hits == 0 {
		return fmt.Sprintf("No matches for %q in any memory.", a.Pattern), nil
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
