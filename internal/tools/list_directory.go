package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var listDirectorySchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Absolute path or file:// URI of the directory to list."
    },
    "pattern": {
      "type": "string",
      "description": "Optional glob filter applied to entry names, e.g. '*.go' or 'README*'."
    },
    "include_hidden": {
      "type": "boolean",
      "description": "Include hidden entries (names starting with '.'). Default false."
    },
    "sort_by": {
      "type": "string",
      "enum": ["name", "size", "modified"],
      "description": "Sort order. Default: name."
    }
  },
  "required": ["path"]
}`)

// ListDirectory lists the immediate children of a directory with [FILE]/[DIR]
// type prefixes, sizes for files, and modification times. Unlike list_files it
// is non-recursive — it shows one level only, like `ls -la`. Use list_files or
// find_files for recursive traversal.
//
// Concurrency: Execute is safe for concurrent use.
type ListDirectory struct{ ws WorkspaceFn }

func NewListDirectory(ws WorkspaceFn) *ListDirectory { return &ListDirectory{ws: ws} }

func (*ListDirectory) Name() string                 { return "list_directory" }
func (*ListDirectory) InputSchema() json.RawMessage { return listDirectorySchema }
func (*ListDirectory) Description() string {
	return "List the immediate contents of a directory with [FILE] and [DIR] type prefixes, " +
		"file sizes, and last-modified times. Non-recursive — shows one level only. " +
		"Accepts an absolute path, file:// URI, or workspace-relative path. " +
		"Use list_files or find_files for recursive traversal."
}

type listDirectoryArgs struct {
	Path          string `json:"path"`
	Pattern       string `json:"pattern"`
	IncludeHidden bool   `json:"include_hidden"`
	SortBy        string `json:"sort_by"`
}

type dirEntry struct {
	name     string
	isDir    bool
	size     int64
	modified int64 // UnixNano
}

func (t *ListDirectory) Execute(_ context.Context, raw json.RawMessage) (string, error) {
	a, err := parseListDirectoryArgs(raw)
	if err != nil {
		return "", err
	}
	dir := resolvePath(a.Path, t.ws)
	entries, err := collectDirEntries(dir, a)
	if err != nil {
		return "", err
	}
	sortDirEntries(entries, a.SortBy)
	return formatDirResult(dir, entries), nil
}

func parseListDirectoryArgs(raw json.RawMessage) (listDirectoryArgs, error) {
	var a listDirectoryArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("list_directory: invalid arguments: %w", err)
	}
	if a.Path == "" {
		return a, fmt.Errorf("list_directory: path is required")
	}
	return a, nil
}

func collectDirEntries(dir string, a listDirectoryArgs) ([]dirEntry, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("list_directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("list_directory: %q is not a directory — use read_file to read a file", dir)
	}
	rawEntries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("list_directory: reading %q: %w", dir, err)
	}
	return filterDirEntries(rawEntries, a)
}

func filterDirEntries(rawEntries []os.DirEntry, a listDirectoryArgs) ([]dirEntry, error) {
	var entries []dirEntry
	for _, e := range rawEntries {
		if !a.IncludeHidden && strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if a.Pattern != "" {
			ok, err := filepath.Match(a.Pattern, e.Name())
			if err != nil {
				return nil, fmt.Errorf("list_directory: bad pattern %q: %w", a.Pattern, err)
			}
			if !ok {
				continue
			}
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		entries = append(entries, dirEntry{
			name:     e.Name(),
			isDir:    e.IsDir(),
			size:     fi.Size(),
			modified: fi.ModTime().UnixNano(),
		})
	}
	return entries, nil
}

func sortDirEntries(entries []dirEntry, sortBy string) {
	switch sortBy {
	case "size":
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].isDir != entries[j].isDir {
				return entries[i].isDir // dirs first
			}
			return entries[i].size > entries[j].size // larger first
		})
	case "modified":
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].modified > entries[j].modified // newest first
		})
	default: // "name"
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].isDir != entries[j].isDir {
				return entries[i].isDir // dirs first
			}
			return entries[i].name < entries[j].name
		})
	}
}

func formatDirResult(dir string, entries []dirEntry) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s\n\n", dir)
	dirs, files := 0, 0
	for _, e := range entries {
		mt := time.Unix(0, e.modified).Format("2006-01-02 15:04")
		if e.isDir {
			dirs++
			fmt.Fprintf(&sb, "[DIR]  %-40s  %12s  %s\n", e.name, "", mt)
		} else {
			files++
			fmt.Fprintf(&sb, "[FILE] %-40s  %12s  %s\n", e.name, formatSize(e.size), mt)
		}
	}
	if len(entries) == 0 {
		sb.WriteString("(empty)\n")
	} else {
		fmt.Fprintf(&sb, "\n%d director%s, %d file%s",
			dirs, plural(dirs, "y", "ies"),
			files, plural(files, "", "s"),
		)
	}
	return sb.String()
}

func formatSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	case n < 1024*1024*1024:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	default:
		return fmt.Sprintf("%.1f GB", float64(n)/(1024*1024*1024))
	}
}

func plural(n int, singular, pluralSuffix string) string {
	if n == 1 {
		return singular
	}
	return pluralSuffix
}

// resolvePath resolves a path argument for filesystem tools. Strips a leading
// file:// scheme, then anchors relative (or empty) paths to the workspace root
// returned by ws. Falls back to os.Getwd() when ws is nil or unresolved.
func resolvePath(path string, ws WorkspaceFn) string {
	p := strings.TrimPrefix(path, "file://")
	if filepath.IsAbs(p) {
		return p
	}
	base := ""
	if ws != nil {
		base = ws()
	}
	if base == "" {
		base, _ = os.Getwd()
	}
	if p == "" {
		return base
	}
	return filepath.Join(base, p)
}
