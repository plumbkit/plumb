package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var fileStatusSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "paths": {
      "type": "array",
      "items": { "type": "string" },
      "minItems": 1,
      "maxItems": 50,
      "description": "Files to report on. Each is an absolute path, file:// URI, or workspace-relative path."
    }
  },
  "required": ["paths"],
  "additionalProperties": false
}`)

const maxFileStatusPaths = 50

// FileStatus reports a lightweight "did this change under me?" signal for each
// input path, without reading file content. It answers three questions an agent
// asks before trusting a stale view of a file: is it dirty versus git, has it
// changed on disk since plumb last wrote it this session, and who wrote it last
// (plumb itself, or an external process / peer agent).
//
// It is a pure read tool — it never writes, locks, or notifies the LSP. The
// signals are assembled from components the write tools already maintain: the
// per-connection WriteTracker (last-writer + recorded write mtime), the git
// dirty check (the same `git status --porcelain` used by the write dirty-guard),
// and os.Stat (mtime + size). No new infrastructure.
//
// Concurrency: Execute is safe for concurrent use.
type FileStatus struct {
	writes *WriteTracker // may be nil; without it last_writer is always "unknown"
	guard  BoundaryGuard
	ws     WorkspaceFn
}

// NewFileStatus constructs the tool. The WriteTracker is the per-connection
// session tracker; pass nil in tests for a tracker-less tool (last_writer then
// reports "unknown" for every path).
func NewFileStatus(writes *WriteTracker) *FileStatus {
	return &FileStatus{writes: writes}
}

// WithBoundary wires the per-connection boundary guard so a path outside the
// pinned workspace is refused rather than stat'd. Nil-safe.
func (t *FileStatus) WithBoundary(guard BoundaryGuard) *FileStatus {
	t.guard = guard
	return t
}

// WithWorkspace wires the pinned-workspace accessor so a workspace-relative
// path resolves against the workspace root. Nil-safe.
func (t *FileStatus) WithWorkspace(ws WorkspaceFn) *FileStatus {
	t.ws = ws
	return t
}

func (t *FileStatus) Name() string                 { return "file_status" }
func (t *FileStatus) InputSchema() json.RawMessage { return fileStatusSchema }

func (t *FileStatus) Description() string {
	return "Lightweight, read-only \"did this file change under me?\" check. For each path " +
		"reports, without reading content: git_dirty (uncommitted changes vs git HEAD/index — " +
		"untracked counts as dirty), changed_since_plumb_wrote (the on-disk mtime advanced since " +
		"plumb last wrote it this session — a peer or external process edited it), last_writer " +
		"(plumb = plumb wrote it last this session and it is unchanged; external = plumb wrote it " +
		"but it has since changed on disk; unknown = plumb has not written it this session), plus " +
		"mtime and size. " +
		"Use before re-editing a file you read or wrote earlier to confirm your view is still " +
		"current, instead of a blind re-read; pair changed_since_plumb_wrote / last_writer with a " +
		"read_file to refresh when it reports drift. Missing files are reported, not an error. " +
		"This is a status probe, not a content read — it does not satisfy strict mode's " +
		"read-before-edit requirement."
}

type fileStatusArgs struct {
	Paths []string `json:"paths"`
}

// fileStatusResult is the per-path status. error is non-empty when the path
// could not be inspected (boundary violation); exists is false when the file is
// absent, in which case the git/writer/size fields are zero-valued.
type fileStatusResult struct {
	path         string
	exists       bool
	gitDirty     bool
	changedSince bool
	lastWriter   string
	mtime        time.Time
	size         int64
	err          string
}

func (t *FileStatus) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	args, err := parseFileStatusArgs(raw)
	if err != nil {
		return "", err
	}
	if err := args.validate(); err != nil {
		return "", err
	}
	results := t.run(ctx, args)
	return formatFileStatusResult(results), nil
}

func parseFileStatusArgs(raw json.RawMessage) (fileStatusArgs, error) {
	var a fileStatusArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("file_status: invalid arguments: %w", err)
	}
	return a, nil
}

func (a fileStatusArgs) validate() error {
	if len(a.Paths) == 0 {
		return fmt.Errorf("file_status: paths must contain at least one path")
	}
	if len(a.Paths) > maxFileStatusPaths {
		return fmt.Errorf("file_status: too many paths (%d); maximum is %d", len(a.Paths), maxFileStatusPaths)
	}
	return nil
}

func (t *FileStatus) run(ctx context.Context, args fileStatusArgs) []fileStatusResult {
	results := make([]fileStatusResult, 0, len(args.Paths))
	for _, p := range args.Paths {
		results = append(results, t.inspect(ctx, p))
	}
	return results
}

// inspect assembles the status for a single path. A boundary violation short-
// circuits to an error entry; an absent file yields exists=false; otherwise the
// git, last-writer and stat signals are combined.
func (t *FileStatus) inspect(ctx context.Context, raw string) fileStatusResult {
	resolved := filepath.Clean(resolvePath(raw, t.ws))
	res := fileStatusResult{path: resolved}
	if err := t.guard.check(resolved); err != nil {
		res.err = err.Error()
		return res
	}
	info, err := os.Stat(resolved)
	if err != nil {
		// Absent (or unreadable) file: report it as not-existing rather than
		// failing the whole call, so a caller probing several paths still learns
		// about the others.
		return res
	}
	res.exists = true
	res.mtime = info.ModTime()
	res.size = info.Size()
	res.gitDirty = pathIsDirty(ctx, resolved)
	res.changedSince, res.lastWriter = t.writerStatus(resolved, info.ModTime())
	return res
}

// writerStatus derives changed_since_plumb_wrote and last_writer from the
// per-session WriteTracker. A file plumb never wrote this session is "unknown";
// one whose on-disk mtime advanced past plumb's recorded write was changed by
// someone else ("external"); otherwise plumb is the last writer.
func (t *FileStatus) writerStatus(path string, mtime time.Time) (changed bool, writer string) {
	recorded, wrote := t.writes.WroteMtime(path)
	if !wrote {
		return false, "unknown"
	}
	if recorded != 0 && mtime.UnixNano() > recorded {
		return true, "external"
	}
	return false, "plumb"
}

func formatFileStatusResult(results []fileStatusResult) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "file_status — %d path(s)\n\n", len(results))
	for _, r := range results {
		writeFileStatusEntry(&sb, r)
	}
	return strings.TrimRight(sb.String(), "\n") + "\n"
}

func writeFileStatusEntry(sb *strings.Builder, r fileStatusResult) {
	fmt.Fprintf(sb, "%s\n", r.path)
	if r.err != "" {
		fmt.Fprintf(sb, "  error: %s\n\n", r.err)
		return
	}
	if !r.exists {
		sb.WriteString("  exists: false\n\n")
		return
	}
	fmt.Fprintf(sb, "  exists: true\n")
	fmt.Fprintf(sb, "  git_dirty: %t\n", r.gitDirty)
	fmt.Fprintf(sb, "  changed_since_plumb_wrote: %t\n", r.changedSince)
	fmt.Fprintf(sb, "  last_writer: %s\n", r.lastWriter)
	fmt.Fprintf(sb, "  mtime: %s\n", r.mtime.Format(time.RFC3339Nano))
	fmt.Fprintf(sb, "  size: %d\n\n", r.size)
}
