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

func (t *findReplaceTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
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
	if a.MaxFileBytes == 0 {
		a.MaxFileBytes = defaultMaxFileBytes
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

	globPrefix := globLiteralPrefix(a.Glob)

	var files []string
	if info.IsDir() {
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

	scan := func(path string) (int, []byte) {
		// Size guard before reading: huge files (logs, dumps, generated code)
		// can stall a 4 min MCP timeout even if they're plain text.
		if fi, err := os.Stat(path); err != nil || fi.Size() > a.MaxFileBytes {
			return 0, nil
		}
		// Sniff first so we don't buffer huge binary files just to discard them.
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
		case !caseSensitive:
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
					continue // drain remaining sends so the dispatcher unblocks
				}
				count, newData := scan(path)
				if count == 0 {
					continue
				}
				if claimed.Add(1) > maxFiles {
					cancel()
					continue
				}
				if !dryRun {
					if !t.deps.Limiter.Allow() {
						results <- fileChange{path: path, count: count, err: rateLimitError("find_replace", t.deps.Limiter)}
						cancel()
						continue
					}
					unlock := lockPath(path)
					if !a.DirtyOk && pathIsDirty(ctx, path) {
						unlock()
						results <- fileChange{path: path, count: count, err: fmt.Errorf("find_replace: %q has uncommitted changes; review and commit first, or pass dirty_ok: true to proceed", path)}
						cancel()
						continue
					}
					_, err := safeWrite(path, newData, 0o644)
					unlock()
					if err != nil {
						results <- fileChange{path: path, count: count, err: fmt.Errorf("find_replace: writing %s: %w", path, err)}
						cancel()
						continue
					}
					if err := notifyLSP(ctx, t.deps.Client, path, protocol.FileChanged); err != nil {
						slog.Warn("find_replace: LSP notification failed", "path", path, "err", err)
					}
					invalidateCache(t.deps.Cache, "file://"+path)
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

	sort.Slice(changes, func(i, j int) bool {
		return changes[i].path < changes[j].path
	})

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

	if !dryRun && a.FormatAfter && len(changes) > 0 {
		formatted, formatErrs := runFormatterOnFiles(ctx, changes)
		if formatted > 0 {
			fmt.Fprintf(&sb, "\nformatted %d file(s)", formatted)
		}
		for _, fe := range formatErrs {
			sb.WriteString("\nformat warning: ")
			sb.WriteString(fe.Error())
		}
	}

	if len(writeErrs) > 0 {
		return sb.String(), errors.Join(writeErrs...)
	}

	return sb.String(), nil
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
		out, err := exec.CommandContext(ctx, cmd[0], cmd[1:]...).CombinedOutput()
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
