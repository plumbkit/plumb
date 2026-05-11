package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var findFilesSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern": {
      "type": "string",
      "description": "Glob (or regex if use_regex=true) matched against the file/directory name. When the pattern contains '/' it matches the full relative path. Use \"*\" to match everything — a literal \".\" only matches a file named \".\"."
    },
    "path": {
      "type": "string",
      "description": "Directory to search in (file:// URI or absolute path). Defaults to the workspace root."
    },
    "type": {
      "type": "string",
      "enum": ["file", "dir", "any"],
      "description": "Restrict to files, directories, or both. Default: 'file'."
    },
    "extension": {
      "type": "string",
      "description": "Filter by file extension, e.g. 'go' or '.go'."
    },
    "max_depth": {
      "type": "integer",
      "description": "Maximum directory depth to descend. Default: unlimited.",
      "minimum": 1
    },
    "max_results": {
      "type": "integer",
      "description": "Maximum number of results to return. Default 500.",
      "minimum": 1,
      "maximum": 5000
    },
    "include_hidden": {
      "type": "boolean",
      "description": "Include hidden files and directories (starting with '.'). Default false."
    },
    "use_regex": {
      "type": "boolean",
      "description": "Treat pattern as a regular expression instead of a glob. Default false."
    }
  },
  "required": ["pattern"]
}`)

// FindFiles implements fd-like recursive file/directory finding.
type FindFiles struct{}

func NewFindFiles() *FindFiles { return &FindFiles{} }

func (t *FindFiles) Name() string             { return "find_files" }
func (t *FindFiles) InputSchema() json.RawMessage { return findFilesSchema }
func (t *FindFiles) Description() string {
	return "Workspace-scoped file/directory finder. Prefer this over shelling out to find/fd: " +
		"results are confined to the active project (no .git/, node_modules/, build output, or anything else .gitignore excludes), " +
		"every call is recorded in the project's stats, and the pattern semantics are consistent across hosts. " +
		"Supports glob and regex patterns, extension filters, type filters (file/dir), and depth limits. " +
		"Essential for clients without filesystem access of their own (Claude Desktop, Cursor MCP, etc.)."
}

type findFilesArgs struct {
	Pattern       string `json:"pattern"`
	Path          string `json:"path"`
	Type          string `json:"type"`
	Extension     string `json:"extension"`
	MaxDepth      int    `json:"max_depth"`
	MaxResults    int    `json:"max_results"`
	IncludeHidden bool   `json:"include_hidden"`
	UseRegex      bool   `json:"use_regex"`
}

func (t *FindFiles) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a findFilesArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("find_files: invalid arguments: %w", err)
	}
	if a.Pattern == "" {
		return "", fmt.Errorf("find_files: pattern must not be empty")
	}

	// Resolve search root.
	root := strings.TrimPrefix(a.Path, "file://")
	if root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("find_files: getting cwd: %w", err)
		}
		root = cwd
	}
	if info, err := os.Stat(root); err != nil {
		return "", fmt.Errorf("find_files: path %q: %w", root, err)
	} else if !info.IsDir() {
		root = filepath.Dir(root)
	}

	// Defaults.
	if a.MaxResults <= 0 {
		a.MaxResults = 500
	}
	if a.Type == "" {
		a.Type = "file"
	}

	// Normalise extension.
	ext := strings.ToLower(strings.TrimPrefix(a.Extension, "."))

	// Compile matcher.
	matchFn, err := buildMatcher(a.Pattern, a.UseRegex)
	if err != nil {
		return "", fmt.Errorf("find_files: invalid pattern %q: %w", a.Pattern, err)
	}

	// Determine if pattern contains a slash → match against relative path.
	patternHasSlash := strings.Contains(a.Pattern, "/")

	var hits []string
	truncated := false

	opts := walkOptions{
		root:          root,
		maxDepth:      a.MaxDepth,
		includeHidden: a.IncludeHidden,
		respectIgnore: true,
	}

	_ = walk(ctx, opts, func(path string, d fs.DirEntry, _ int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if truncated {
			return nil
		}

		isDir := d.IsDir()

		// Type filter.
		switch a.Type {
		case "file":
			if isDir {
				return nil
			}
		case "dir":
			if !isDir {
				return nil
			}
		}

		// Extension filter (files only).
		if ext != "" && !isDir {
			if strings.ToLower(strings.TrimPrefix(filepath.Ext(d.Name()), ".")) != ext {
				return nil
			}
		}

		// Pattern matching.
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		var target string
		if patternHasSlash {
			target = rel
		} else {
			target = d.Name()
		}
		if !matchFn(target) {
			return nil
		}

		hits = append(hits, rel)
		if len(hits) >= a.MaxResults {
			truncated = true
		}
		return nil
	})

	if len(hits) == 0 {
		return fmt.Sprintf("No files found matching %q.", a.Pattern), nil
	}

	var sb strings.Builder
	for _, h := range hits {
		sb.WriteString(h)
		sb.WriteByte('\n')
	}
	if truncated {
		fmt.Fprintf(&sb, "\n(truncated at %d results — use a more specific pattern or set max_depth)", a.MaxResults)
	} else {
		fmt.Fprintf(&sb, "\n%d result(s)", len(hits))
	}
	return sb.String(), nil
}

// buildMatcher returns a function that tests a name/path against the pattern.
func buildMatcher(pattern string, useRegex bool) (func(string) bool, error) {
	if useRegex {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, err
		}
		return re.MatchString, nil
	}

	// Glob mode: if the pattern contains **, use doubleStarMatch.
	// Otherwise use filepath.Match which handles *, ?, [...].
	if strings.Contains(pattern, "**") {
		return func(name string) bool {
			return doubleStarMatch(pattern, name)
		}, nil
	}
	// Validate the glob before returning.
	if _, err := filepath.Match(pattern, ""); err != nil {
		return nil, err
	}
	return func(name string) bool {
		m, _ := filepath.Match(pattern, name)
		return m
	}, nil
}
