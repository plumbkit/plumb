package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
)

var listFilesSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "root": {
      "type": "string",
      "description": "Directory to list. Defaults to the current working directory."
    },
    "pattern": {
      "type": "string",
      "description": "Glob pattern to filter filenames, e.g. \"*.go\" or \"*_test.go\". Omit to list all files."
    },
    "max_depth": {
      "type": "integer",
      "description": "Maximum directory depth to recurse (default 8). Set to 1 for top-level only."
    },
    "include_hidden": {
      "type": "boolean",
      "description": "Include hidden files and directories (those starting with \".\"). Default false."
    }
  }
}`)

// always-excluded directory names regardless of settings.
var excludedDirs = map[string]bool{
	".git":          true,
	"vendor":        true,
	"node_modules":  true,
	"__pycache__":   true,
	".pytest_cache": true,
	"dist":          false, // only excluded when hidden
	"build":         false,
}

// ListFiles walks a directory tree and returns matching file paths.
//
// Concurrency: Execute is safe for concurrent use.
type ListFiles struct{ ws WorkspaceFn }

func NewListFiles(ws WorkspaceFn) *ListFiles { return &ListFiles{ws: ws} }

func (t *ListFiles) Name() string                 { return "list_files" }
func (t *ListFiles) InputSchema() json.RawMessage { return listFilesSchema }
func (t *ListFiles) Description() string {
	return "Workspace-aware directory listing. Returns paths relative to the specified root, " +
		"with glob filtering (e.g. \"*.go\"), depth control, and optional hidden-file inclusion. " +
		"Prefer over plain ls/find: every call is recorded in the project's stats and the glob semantics " +
		"are consistent across hosts. " +
		"Essential for clients without filesystem access of their own (Claude Desktop, Cursor MCP, etc.). " +
		"For locating files by name pattern across the whole tree, find_files is more efficient."
}

type listFilesArgs struct {
	Root          string `json:"root"`
	Pattern       string `json:"pattern"`
	MaxDepth      *int   `json:"max_depth"`
	IncludeHidden bool   `json:"include_hidden"`
}

func (t *ListFiles) Execute(_ context.Context, raw json.RawMessage) (string, error) {
	var a listFilesArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("list_files: invalid arguments: %w", err)
	}

	root := filepath.Clean(resolvePath(a.Root, t.ws))

	maxDepth := 8
	if a.MaxDepth != nil {
		maxDepth = *a.MaxDepth
	}

	var paths []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}

		name := d.Name()
		rel, _ := filepath.Rel(root, path)
		depth := strings.Count(rel, string(filepath.Separator))

		if d.IsDir() {
			if path == root {
				return nil
			}
			if !a.IncludeHidden && strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			if excludedDirs[name] {
				return filepath.SkipDir
			}
			if depth+1 >= maxDepth {
				return filepath.SkipDir
			}
			return nil
		}

		if !a.IncludeHidden && strings.HasPrefix(name, ".") {
			return nil
		}

		if a.Pattern != "" {
			matched, err := filepath.Match(a.Pattern, name)
			if err != nil {
				return fmt.Errorf("invalid pattern %q: %w", a.Pattern, err)
			}
			if !matched {
				return nil
			}
		}

		paths = append(paths, rel)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("list_files: walking %s: %w", root, err)
	}

	if len(paths) == 0 {
		msg := fmt.Sprintf("No files found under %s", root)
		if a.Pattern != "" {
			msg += fmt.Sprintf(" matching %q", a.Pattern)
		}
		return msg + ".", nil
	}

	var sb strings.Builder
	label := fmt.Sprintf("%d file(s)", len(paths))
	if a.Pattern != "" {
		label = fmt.Sprintf("%d file(s) matching %q", len(paths), a.Pattern)
	}
	fmt.Fprintf(&sb, "%s under %s\n\n", label, root)
	for _, p := range paths {
		sb.WriteString(p + "\n")
	}
	return sb.String(), nil
}
