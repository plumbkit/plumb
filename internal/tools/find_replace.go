package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type findReplaceTool struct{}

func NewFindReplace() *findReplaceTool { return &findReplaceTool{} }

func (*findReplaceTool) Name() string { return "find_replace" }

func (*findReplaceTool) Description() string {
	return `Search and replace text across files in a directory tree.

Defaults to dry_run=true so you can preview the diff before committing. Set dry_run=false to write changes.

Skips binary files. Honours .gitignore. Use 'glob' to limit which files to touch (e.g. "*.go", "**/*.md").

PREFER LSP semantic tools (rename_symbol, etc.) when refactoring identifiers — they understand scope and types. Use find_replace for plain-text edits like updating doc strings, license headers, hostnames, version strings, or non-code files.`
}

func (*findReplaceTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"path":{"type":"string","description":"Directory to walk, or a single file."},
			"pattern":{"type":"string","description":"Search pattern. Plain text by default; regex if use_regex=true."},
			"replacement":{"type":"string","description":"Replacement text. With regex, supports $1, $2 backreferences."},
			"use_regex":{"type":"boolean","default":false},
			"glob":{"type":"string","description":"File glob filter, e.g. '*.go' or '**/*.md'. Empty = all non-binary files."},
			"case_sensitive":{"type":"boolean","description":"Default: smart-case (case-insensitive iff pattern is all lowercase)."},
			"dry_run":{"type":"boolean","default":true,"description":"If true (default), preview only; do not write files."},
			"max_files":{"type":"integer","default":100,"description":"Cap on number of files modified."}
		},
		"required":["path","pattern","replacement"]
	}`)
}

func (t *findReplaceTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Path          string `json:"path"`
		Pattern       string `json:"pattern"`
		Replacement   string `json:"replacement"`
		UseRegex      bool   `json:"use_regex"`
		Glob          string `json:"glob"`
		CaseSensitive *bool  `json:"case_sensitive,omitempty"`
		DryRun        *bool  `json:"dry_run,omitempty"`
		MaxFiles      int    `json:"max_files"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if a.Path == "" || a.Pattern == "" {
		return "", fmt.Errorf("`path` and `pattern` are required")
	}

	dryRun := true
	if a.DryRun != nil {
		dryRun = *a.DryRun
	}
	caseSensitive := !allLower(a.Pattern)
	if a.CaseSensitive != nil {
		caseSensitive = *a.CaseSensitive
	}
	if a.MaxFiles == 0 {
		a.MaxFiles = 100
	}

	// Build the matcher up front so we fail fast on bad regex.
	var re *regexp.Regexp
	if a.UseRegex {
		flags := ""
		if !caseSensitive {
			flags = "(?i)"
		}
		var err error
		re, err = regexp.Compile(flags + a.Pattern)
		if err != nil {
			return "", fmt.Errorf("invalid regex %q: %w", a.Pattern, err)
		}
	} else if !caseSensitive {
		re = regexp.MustCompile("(?i)" + regexp.QuoteMeta(a.Pattern))
	}

	info, err := os.Stat(a.Path)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", a.Path, err)
	}

	var files []string
	if info.IsDir() {
		opts := walkOptions{root: a.Path, respectIgnore: true}
		_ = walk(opts, func(path string, d fs.DirEntry, _ int) error {
			if d.IsDir() {
				return nil
			}
			if a.Glob != "" {
				matched, _ := filepath.Match(a.Glob, d.Name())
				if !matched {
					rel, _ := filepath.Rel(a.Path, path)
					matched2, _ := doubleStarMatchFile(a.Glob, filepath.ToSlash(rel))
					if !matched2 {
						return nil
					}
				}
			}
			files = append(files, path)
			return nil
		})
	} else {
		files = []string{a.Path}
	}

	type fileChange struct {
		path  string
		count int
	}
	var changes []fileChange
	totalReplacements := 0

	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if looksLikeBinary(bytes.NewReader(data)) {
			continue
		}

		var newData []byte
		var count int

		switch {
		case a.UseRegex:
			count = len(re.FindAll(data, -1))
			if count > 0 {
				newData = re.ReplaceAll(data, []byte(a.Replacement))
			}
		case !caseSensitive:
			count = len(re.FindAll(data, -1))
			if count > 0 {
				newData = re.ReplaceAllLiteral(data, []byte(a.Replacement))
			}
		default:
			count = strings.Count(string(data), a.Pattern)
			if count > 0 {
				newData = []byte(strings.ReplaceAll(string(data), a.Pattern, a.Replacement))
			}
		}

		if count == 0 {
			continue
		}

		if !dryRun {
			tmp := path + ".tmp"
			if err := os.WriteFile(tmp, newData, 0o644); err != nil {
				continue
			}
			if err := os.Rename(tmp, path); err != nil {
				os.Remove(tmp)
				continue
			}
		}

		changes = append(changes, fileChange{path: path, count: count})
		totalReplacements += count

		if len(changes) >= a.MaxFiles {
			break
		}
	}

	var sb strings.Builder
	if dryRun {
		sb.WriteString("DRY RUN — no files modified.\n\n")
	}
	verb := "would change"
	if !dryRun {
		verb = "changed"
	}
	fmt.Fprintf(&sb, "%d file(s), %d replacement(s) %s\n\n",
		len(changes), totalReplacements, verb)

	for _, c := range changes {
		rel := c.path
		if r, err := filepath.Rel(a.Path, c.path); err == nil && !strings.HasPrefix(r, "..") {
			rel = r
		}
		fmt.Fprintf(&sb, "  %s  (%d)\n", rel, c.count)
	}

	if dryRun && len(changes) > 0 {
		sb.WriteString("\nTo apply, re-run with dry_run=false.")
	}

	return sb.String(), nil
}
