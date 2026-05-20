package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/golimpio/plumb/internal/lsp/protocol"
)

type findReplaceTool struct {
	deps WriteDeps
}

type fileChange struct {
	path  string
	count int
	err   error
}

func NewFindReplace(deps ...WriteDeps) *findReplaceTool {
	var d WriteDeps
	if len(deps) > 0 {
		d = deps[0]
	}
	return &findReplaceTool{deps: d}
}

func (*findReplaceTool) Name() string { return "find_replace" }

func (*findReplaceTool) Description() string {
	return `Grep-equivalent: find text across files with optional replacement. Search and replace text across files in a directory tree.

Defaults to dry_run=true so you can preview the diff before committing. Set dry_run=false to write changes.

Skips binary files (detected via null-byte sniff of the first 8 KB). Skips files larger than max_file_bytes (50 MiB default). Honours .gitignore. Use 'glob' to limit which files to touch (e.g. "*.go", "**/*.md"); a glob with a literal directory prefix (e.g. "src/**/*.go") prunes sibling directories from the walk entirely. Files are processed in parallel; output is sorted by path.

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
			"dirty_ok":{"type":"boolean","default":false,"description":"Allow editing files that have uncommitted changes in their git repository. Default false — the replacement is refused if any target file is dirty. Pass true to proceed anyway."},
			"max_files":{"type":"integer","default":100,"description":"Cap on number of files modified."},
			"max_file_bytes":{"type":"integer","default":52428800,"description":"Skip files larger than this many bytes. Default 50 MiB."},
			"format_after":{"type":"boolean","default":false,"description":"After writing changes, run the workspace formatter (gofumpt for Go, ruff format for Python) on each modified file. Formatter errors are reported as warnings and do not fail the call."}
		},
		"required":["path","pattern","replacement"]
	}`)
}

const defaultMaxFileBytes int64 = 50 * 1024 * 1024

type findReplaceArgs struct {
	Path          string `json:"path"`
	Pattern       string `json:"pattern"`
	Replacement   string `json:"replacement"`
	UseRegex      bool   `json:"use_regex"`
	Glob          string `json:"glob"`
	CaseSensitive *bool  `json:"case_sensitive,omitempty"`
	DryRun        *bool  `json:"dry_run,omitempty"`
	DirtyOk       bool   `json:"dirty_ok"`
	FormatAfter   bool   `json:"format_after"`
	MaxFiles      int    `json:"max_files"`
	MaxFileBytes  int64  `json:"max_file_bytes"`
	// Resolved fields set by applyFindReplaceDefaults (not from JSON).
	dryRun        bool
	caseSensitive bool
}

func (t *findReplaceTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	a, err := parseFindReplaceArgs(args)
	if err != nil {
		return "", err
	}
	applyFindReplaceDefaults(&a)

	re, err := compileFindReplaceRegex(a)
	if err != nil {
		return "", err
	}

	files, err := findReplaceCollectFiles(ctx, a)
	if err != nil {
		return "", err
	}

	changes, writeErrs, totalReplacements := t.findReplaceRunWorkers(ctx, files, a, re)

	var formatted int
	var formatErrs []error
	if !a.dryRun && a.FormatAfter && len(changes) > 0 {
		formatted, formatErrs = runFormatterOnFiles(ctx, changes)
	}

	out := formatFindReplaceOutput(changes, a, totalReplacements, formatted, formatErrs)
	if len(writeErrs) > 0 {
		return out, errors.Join(writeErrs...)
	}
	return out, nil
}

func parseFindReplaceArgs(raw json.RawMessage) (findReplaceArgs, error) {
	var a findReplaceArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("invalid args: %w", err)
	}
	if a.Path == "" || a.Pattern == "" {
		return a, fmt.Errorf("`path` and `pattern` are required")
	}
	return a, nil
}

func applyFindReplaceDefaults(a *findReplaceArgs) {
	a.dryRun = true
	if a.DryRun != nil {
		a.dryRun = *a.DryRun
	}
	a.caseSensitive = !allLower(a.Pattern)
	if a.CaseSensitive != nil {
		a.caseSensitive = *a.CaseSensitive
	}
	if a.MaxFiles == 0 {
		a.MaxFiles = 100
	}
	if a.MaxFileBytes == 0 {
		a.MaxFileBytes = defaultMaxFileBytes
	}
}

func compileFindReplaceRegex(a findReplaceArgs) (*regexp.Regexp, error) {
	if a.UseRegex {
		flags := ""
		if !a.caseSensitive {
			flags = "(?i)"
		}
		re, err := regexp.Compile(flags + a.Pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid regex %q: %w", a.Pattern, err)
		}
		return re, nil
	}
	if !a.caseSensitive {
		return regexp.MustCompile("(?i)" + regexp.QuoteMeta(a.Pattern)), nil
	}
	return nil, nil
}

func findReplaceCollectFiles(ctx context.Context, a findReplaceArgs) ([]string, error) {
	info, err := os.Stat(a.Path)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", a.Path, err)
	}
	if !info.IsDir() {
		return []string{a.Path}, nil
	}
	globPrefix := globLiteralPrefix(a.Glob)
	var files []string
	opts := walkOptions{root: a.Path, respectIgnore: true}
	_ = walk(ctx, opts, func(path string, d fs.DirEntry, _ int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			if globPrefix != "" && path != a.Path {
				rel, _ := filepath.Rel(a.Path, path)
				if !dirCompatibleWithPrefix(filepath.ToSlash(rel), globPrefix) {
					return fs.SkipDir
				}
			}
			return nil
		}
		if a.Glob != "" {
			matched, _ := filepath.Match(a.Glob, d.Name())
			if !matched {
				rel, _ := filepath.Rel(a.Path, path)
				if m, _ := doubleStarMatchFile(a.Glob, filepath.ToSlash(rel)); !m {
					return nil
				}
			}
		}
		files = append(files, path)
		return nil
	})
	return files, nil
}

// findReplaceScanFile reads path, applies the pattern, and returns the match
// count and replacement bytes. Returns (0, nil) for binary, oversized, or
// zero-match files.
func findReplaceScanFile(path string, a findReplaceArgs, re *regexp.Regexp) (int, []byte) {
	if fi, err := os.Stat(path); err != nil || fi.Size() > a.MaxFileBytes {
		return 0, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, nil
	}
	head := make([]byte, binarySniffBytes)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	if bytes.IndexByte(head, 0) >= 0 {
		f.Close()
		return 0, nil
	}
	rest, err := io.ReadAll(f)
	f.Close()
	if err != nil {
		return 0, nil
	}
	data := append(head, rest...)
	switch {
	case a.UseRegex:
		count := len(re.FindAll(data, -1))
		if count == 0 {
			return 0, nil
		}
		return count, re.ReplaceAll(data, []byte(a.Replacement))
	case !a.caseSensitive:
		count := len(re.FindAll(data, -1))
		if count == 0 {
			return 0, nil
		}
		return count, re.ReplaceAllLiteral(data, []byte(a.Replacement))
	default:
		count := strings.Count(string(data), a.Pattern)
		if count == 0 {
			return 0, nil
		}
		return count, []byte(strings.ReplaceAll(string(data), a.Pattern, a.Replacement))
	}
}

// findReplaceProcessFile writes newData to path, checking the rate limiter,
// dirty state, and notifying the LSP after a successful write.
func (t *findReplaceTool) findReplaceProcessFile(ctx context.Context, path string, newData []byte, a findReplaceArgs) error {
	if !t.deps.Limiter.Allow() {
		return rateLimitError("find_replace", t.deps.Limiter)
	}
	unlock := lockPath(path)
	if !a.DirtyOk && pathIsDirty(ctx, path) {
		unlock()
		return fmt.Errorf("find_replace: %q has uncommitted changes; review and commit first, or pass dirty_ok: true to proceed", path)
	}
	_, writeErr := safeWrite(path, newData, 0o644)
	unlock()
	if writeErr != nil {
		return fmt.Errorf("find_replace: writing %s: %w", path, writeErr)
	}
	if err := notifyLSP(ctx, t.deps.Client, path, protocol.FileChanged); err != nil {
		slog.Warn("find_replace: LSP notification failed", "path", path, "err", err)
	}
	invalidateCache(t.deps.Cache, "file://"+path)
	return nil
}

func (t *findReplaceTool) findReplaceRunWorkers(ctx context.Context, files []string, a findReplaceArgs, re *regexp.Regexp) ([]fileChange, []error, int) {
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()

	workers := max(1, min(runtime.NumCPU(), len(files)))
	paths := make(chan string)
	results := make(chan fileChange, workers)
	var claimed atomic.Int64
	maxFiles := int64(a.MaxFiles)

	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for path := range paths {
				if wctx.Err() != nil {
					continue
				}
				count, newData := findReplaceScanFile(path, a, re)
				if count == 0 {
					continue
				}
				if claimed.Add(1) > maxFiles {
					cancel()
					continue
				}
				if !a.dryRun {
					if err := t.findReplaceProcessFile(wctx, path, newData, a); err != nil {
						results <- fileChange{path: path, count: count, err: err}
						cancel()
						continue
					}
				}
				select {
				case results <- fileChange{path: path, count: count}:
				case <-wctx.Done():
				}
			}
		})
	}

	go func() {
		defer close(paths)
		for _, p := range files {
			select {
			case <-wctx.Done():
				return
			case paths <- p:
			}
		}
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	var changes []fileChange
	var writeErrs []error
	totalReplacements := 0
	for r := range results {
		if r.err != nil {
			writeErrs = append(writeErrs, r.err)
			continue
		}
		changes = append(changes, r)
		totalReplacements += r.count
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].path < changes[j].path })
	return changes, writeErrs, totalReplacements
}

func formatFindReplaceOutput(changes []fileChange, a findReplaceArgs, totalReplacements, formatted int, formatErrs []error) string {
	var sb strings.Builder
	if a.dryRun {
		sb.WriteString("DRY RUN — no files modified.\n\n")
	}
	verb := "would change"
	if !a.dryRun {
		verb = "changed"
	}
	fmt.Fprintf(&sb, "%d file(s), %d replacement(s) %s\n\n", len(changes), totalReplacements, verb)
	for _, c := range changes {
		rel := c.path
		if r, err := filepath.Rel(a.Path, c.path); err == nil && !strings.HasPrefix(r, "..") {
			rel = r
		}
		fmt.Fprintf(&sb, "  %s  (%d)\n", rel, c.count)
	}
	if a.dryRun && len(changes) > 0 {
		sb.WriteString("\nTo apply, re-run with dry_run=false.")
	}
	if formatted > 0 {
		fmt.Fprintf(&sb, "\nformatted %d file(s)", formatted)
	}
	for _, fe := range formatErrs {
		sb.WriteString("\nformat warning: ")
		sb.WriteString(fe.Error())
	}
	return sb.String()
}

// runFormatterOnFiles runs the appropriate source formatter on each changed
// file. Returns the count of successfully formatted files and any warnings.
func runFormatterOnFiles(ctx context.Context, changes []fileChange) (int, []error) {
	formatted := 0
	var errs []error
	for _, c := range changes {
		cmd, ok := formatterCmd(c.path)
		if !ok {
			continue
		}
		out, err := exec.CommandContext(ctx, cmd[0], cmd[1:]...).CombinedOutput() //nolint:gosec // G204: cmd[0] is a hardcoded formatter binary name (gofumpt/gofmt/ruff/black) resolved via LookPath; path arg is workspace-validated
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w: %s", filepath.Base(c.path), err, strings.TrimSpace(string(out))))
			slog.Warn("find_replace: formatter failed", "path", c.path, "err", err)
			continue
		}
		formatted++
	}
	return formatted, errs
}

// formatterCmd returns the command to format path based on its extension.
// Returns (cmd, true) when a formatter is available, (nil, false) otherwise.
func formatterCmd(path string) ([]string, bool) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		if _, err := exec.LookPath("gofumpt"); err == nil {
			return []string{"gofumpt", "-w", path}, true
		}
		if _, err := exec.LookPath("gofmt"); err == nil {
			return []string{"gofmt", "-w", path}, true
		}
	case ".py":
		if _, err := exec.LookPath("ruff"); err == nil {
			return []string{"ruff", "format", path}, true
		}
		if _, err := exec.LookPath("black"); err == nil {
			return []string{"black", "--quiet", path}, true
		}
	}
	return nil, false
}
