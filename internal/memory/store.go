// Package memory provides a per-workspace markdown memory store.
//
// Memories live at <workspace>/.plumb/memories/<name>.md. Each file may
// optionally carry YAML-style frontmatter with `name` and `description`
// fields, used by List to summarise without reading the full body.
//
// Memory names are restricted to [A-Za-z0-9_-]+ to prevent path traversal.
package memory

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var nameRegexp = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// Memory describes one memory's metadata.
type Memory struct {
	Name        string
	Description string
	Paths       []string // glob patterns from frontmatter `paths:` for auto-attach
	Path        string
	SizeBytes   int64
}

// Dir returns the memories directory path for workspace.
func Dir(workspace string) string {
	return filepath.Join(workspace, ".plumb", "memories")
}

// Path returns the file path for a named memory.
// Returns an error if name is not a safe identifier.
func Path(workspace, name string) (string, error) {
	if !nameRegexp.MatchString(name) {
		return "", fmt.Errorf("invalid memory name %q (must match [A-Za-z0-9_-]+)", name)
	}
	return filepath.Join(Dir(workspace), name+".md"), nil
}

// List returns all memories in workspace, sorted by name.
// Returns nil (not an error) if the memories directory does not exist.
func List(workspace string) ([]Memory, error) {
	dir := Dir(workspace)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading memories dir: %w", err)
	}
	var out []Memory
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		if !nameRegexp.MatchString(name) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		info, err := e.Info()
		if err != nil {
			continue
		}
		m := Memory{Name: name, Path: path, SizeBytes: info.Size()}
		if data, err := os.ReadFile(path); err == nil {
			m.Description, m.Paths = parseFrontmatterFull(data)
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Read returns the full content of a memory.
func Read(workspace, name string) (string, error) {
	path, err := Path(workspace, name)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("memory %q not found", name)
	}
	if err != nil {
		return "", fmt.Errorf("reading memory: %w", err)
	}
	return string(data), nil
}

// Write atomically writes a memory. If description is non-empty, frontmatter
// is prepended (replacing any existing frontmatter in content).
func Write(workspace, name, content, description string) error {
	path, err := Path(workspace, name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating memories dir: %w", err)
	}
	if description != "" {
		_, body := splitFrontmatter([]byte(content))
		var sb strings.Builder
		sb.WriteString("---\n")
		sb.WriteString("name: ")
		sb.WriteString(name)
		sb.WriteString("\n")
		sb.WriteString("description: ")
		sb.WriteString(strings.ReplaceAll(description, "\n", " "))
		sb.WriteString("\n---\n\n")
		sb.Write(body)
		content = sb.String()
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return fmt.Errorf("writing memory: %w", err)
	}
	return os.Rename(tmp, path)
}

// WriteIndexed writes a memory and updates the FTS index. The file write is the
// source of truth: an index error is logged and swallowed (the next Reindex or
// Fresh check repairs it) and never fails the write. A nil index degrades to a
// plain Write.
func WriteIndexed(ix *Index, workspace, name, content, description string) error {
	if err := Write(workspace, name, content, description); err != nil {
		return err
	}
	if ix == nil {
		return nil
	}
	rec, err := recordFromFile(workspace, name)
	if err != nil {
		slog.Warn("memory: index record build failed", "name", name, "err", err)
		return nil
	}
	if err := ix.Upsert(rec); err != nil {
		slog.Warn("memory: index upsert failed", "name", name, "err", err)
	}
	return nil
}

// DeleteIndexed deletes a memory and removes it from the FTS index. A nil index
// degrades to a plain Delete; an index error is logged, not fatal.
func DeleteIndexed(ix *Index, workspace, name string) error {
	if err := Delete(workspace, name); err != nil {
		return err
	}
	if ix == nil {
		return nil
	}
	if err := ix.Remove(name); err != nil {
		slog.Warn("memory: index remove failed", "name", name, "err", err)
	}
	return nil
}

// Relevant returns memories whose `paths:` frontmatter contains a glob
// matching the given relative path (relative to workspace). Memories
// without a `paths:` entry are excluded.
func Relevant(workspace, relPath string) ([]Memory, error) {
	mems, err := List(workspace)
	if err != nil {
		return nil, err
	}
	relPath = filepath.ToSlash(relPath)
	var out []Memory
	for _, m := range mems {
		for _, glob := range m.Paths {
			if matchGlob(glob, relPath) {
				out = append(out, m)
				break
			}
		}
	}
	return out, nil
}

// matchGlob handles a glob with optional ** segments against a slash path.
func matchGlob(glob, path string) bool {
	// First try exact filepath.Match for simple patterns.
	if ok, _ := filepath.Match(glob, filepath.Base(path)); ok {
		return true
	}
	// Fall back to ** support.
	if strings.Contains(glob, "**") {
		// Convert glob to regex-ish: ** → .*, * → [^/]*
		// Simple approach: split on /** and check segments.
		return doubleStarPathMatch(glob, path)
	}
	if ok, _ := filepath.Match(glob, path); ok {
		return true
	}
	return false
}

func doubleStarPathMatch(glob, path string) bool {
	// "x/**/y" matches "x/.../y"; "x/**" matches anything under "x/"; "**/x" matches "x" or "*/x" etc.
	switch {
	case strings.HasPrefix(glob, "**/"):
		tail := strings.TrimPrefix(glob, "**/")
		if ok, _ := filepath.Match(tail, filepath.Base(path)); ok {
			return true
		}
		// Try matching against every suffix.
		segs := strings.Split(path, "/")
		for i := range segs {
			if ok, _ := filepath.Match(tail, strings.Join(segs[i:], "/")); ok {
				return true
			}
		}
		return false
	case strings.HasSuffix(glob, "/**"):
		head := strings.TrimSuffix(glob, "/**")
		return strings.HasPrefix(path, head+"/") || path == head
	case strings.Contains(glob, "/**/"):
		head, tail, _ := strings.Cut(glob, "/**/")
		if !strings.HasPrefix(path, head+"/") {
			return false
		}
		rest := strings.TrimPrefix(path, head+"/")
		if ok, _ := filepath.Match(tail, filepath.Base(rest)); ok {
			return true
		}
		segs := strings.Split(rest, "/")
		for i := range segs {
			if ok, _ := filepath.Match(tail, strings.Join(segs[i:], "/")); ok {
				return true
			}
		}
		return false
	}
	return false
}

// Delete removes a memory.
func Delete(workspace, name string) error {
	path, err := Path(workspace, name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("memory %q not found", name)
		}
		return fmt.Errorf("deleting memory: %w", err)
	}
	return nil
}

// parseFrontmatterFull also extracts `paths:` — a comma-separated or
// flow-list style list of glob patterns indicating where this memory is
// relevant. Examples:
//
//	paths: internal/auth/**, cmd/server/*.go
//	paths: [internal/auth/**, cmd/server/*.go]
func parseFrontmatterFull(data []byte) (description string, paths []string) {
	fm, _ := splitFrontmatter(data)
	if len(fm) == 0 {
		return "", nil
	}
	for line := range strings.SplitSeq(string(fm), "\n") {
		line = strings.TrimSpace(line)
		if k, v, ok := strings.Cut(line, ":"); ok {
			v = strings.TrimSpace(v)
			switch strings.TrimSpace(k) {
			case "description":
				description = v
			case "paths":
				paths = parseList(v)
			}
		}
	}
	return description, paths
}

func parseList(v string) []string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "[")
	v = strings.TrimSuffix(v, "]")
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.Trim(p, `"'`)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// splitFrontmatter returns (frontmatter-body, body). If the file
// does not begin with `---\n`, frontmatter is empty and body is the full input.
func splitFrontmatter(data []byte) (fm, body []byte) {
	if !bytes.HasPrefix(data, []byte("---\n")) {
		return nil, data
	}
	rest := data[4:]
	before, after, ok := bytes.Cut(rest, []byte("\n---\n"))
	if !ok {
		return nil, data
	}
	return before, after
}
