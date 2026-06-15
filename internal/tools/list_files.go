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
      "description": "Directory to list. Absolute path, file:// URI, or workspace-relative path; defaults to the workspace root."
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
  },
  "additionalProperties": false
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
type ListFiles struct {
	ws    WorkspaceFn
	guard BoundaryGuard
}

func NewListFiles(ws WorkspaceFn) *ListFiles { return &ListFiles{ws: ws} }

func (t *ListFiles) WithBoundary(guard BoundaryGuard) *ListFiles {
	t.guard = guard
	return t
}

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
	a, err := parseListFilesArgs(raw)
	if err != nil {
		return "", err
	}
	root := filepath.Clean(resolvePath(a.Root, t.ws))
	if err := t.guard.check(root); err != nil {
		return "", fmt.Errorf("list_files: %w", err)
	}
	paths, err := listFilesWalk(root, a)
	if err != nil {
		return "", err
	}
	return formatListFilesResult(paths, root, a), nil
}

func parseListFilesArgs(raw json.RawMessage) (listFilesArgs, error) {
	var a listFilesArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("list_files: invalid arguments: %w", err)
	}
	return a, nil
}

func listFilesWalk(root string, a listFilesArgs) ([]string, error) {
	maxDepth := 8
	if a.MaxDepth != nil {
		maxDepth = *a.MaxDepth
	}
	w := &listFilesWalker{root: root, maxDepth: maxDepth, a: a}
	if err := filepath.WalkDir(root, w.visit); err != nil {
		return nil, fmt.Errorf("list_files: walking %s: %w", root, err)
	}
	return w.paths, nil
}

type listFilesWalker struct {
	root     string
	maxDepth int
	a        listFilesArgs
	paths    []string
}

func (w *listFilesWalker) visit(path string, d fs.DirEntry, err error) error {
	if err != nil {
		return nil // skip unreadable entries
	}
	name := d.Name()
	rel, _ := filepath.Rel(w.root, path)
	depth := strings.Count(rel, string(filepath.Separator))
	if d.IsDir() {
		return w.visitDir(path, name, depth)
	}
	return w.visitFile(name, rel)
}

func (w *listFilesWalker) visitDir(path, name string, depth int) error {
	if path == w.root {
		return nil
	}
	if !w.a.IncludeHidden && strings.HasPrefix(name, ".") {
		return filepath.SkipDir
	}
	if excludedDirs[name] {
		return filepath.SkipDir
	}
	if depth+1 >= w.maxDepth {
		return filepath.SkipDir
	}
	return nil
}

func (w *listFilesWalker) visitFile(name, rel string) error {
	if !w.a.IncludeHidden && strings.HasPrefix(name, ".") {
		return nil
	}
	if w.a.Pattern != "" {
		matched, err := filepath.Match(w.a.Pattern, name)
		if err != nil {
			return fmt.Errorf("invalid pattern %q: %w", w.a.Pattern, err)
		}
		if !matched {
			return nil
		}
	}
	w.paths = append(w.paths, rel)
	return nil
}

func formatListFilesResult(paths []string, root string, a listFilesArgs) string {
	if len(paths) == 0 {
		msg := fmt.Sprintf("No files found under %s", root)
		if a.Pattern != "" {
			msg += fmt.Sprintf(" matching %q", a.Pattern)
		}
		return msg + "."
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
	return sb.String()
}
