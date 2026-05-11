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
			_, m.Description, m.Paths = parseFrontmatterFull(data)
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
		_, _, body := splitFrontmatter([]byte(content))
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
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return fmt.Errorf("writing memory: %w", err)
	}
	return os.Rename(tmp, path)
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

// parseFrontmatter returns (name, description) from YAML frontmatter, if any.
func parseFrontmatter(data []byte) (name, description string) {
	name, description, _ = parseFrontmatterFull(data)
	return name, description
}

// parseFrontmatterFull also extracts `paths:` — a comma-separated or
// flow-list style list of glob patterns indicating where this memory is
// relevant. Examples:
//
//	paths: internal/auth/**, cmd/server/*.go
//	paths: [internal/auth/**, cmd/server/*.go]
func parseFrontmatterFull(data []byte) (name, description string, paths []string) {
	_, fm, _ := splitFrontmatter(data)
	if len(fm) == 0 {
		return "", "", nil
	}
	for _, line := range strings.Split(string(fm), "\n") {
		line = strings.TrimSpace(line)
		if k, v, ok := strings.Cut(line, ":"); ok {
			v = strings.TrimSpace(v)
			switch strings.TrimSpace(k) {
			case "name":
				name = v
			case "description":
				description = v
			case "paths":
				paths = parseList(v)
			}
		}
	}
	return name, description, paths
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

// splitFrontmatter returns (delimiter, frontmatter-body, body). If the file
// does not begin with `---\n`, frontmatter is empty and body is the full input.
func splitFrontmatter(data []byte) (delim, fm, body []byte) {
	if !bytes.HasPrefix(data, []byte("---\n")) {
		return nil, nil, data
	}
	rest := data[4:]
	end := bytes.Index(rest, []byte("\n---\n"))
	if end < 0 {
		return nil, nil, data
	}
	return data[:4], rest[:end], rest[end+5:]
}
