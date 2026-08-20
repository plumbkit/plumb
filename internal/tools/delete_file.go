package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// maxDeletePaths bounds a single batch. Deletion is destructive and
// irreversible from plumb's side, so the batch exists to remove round-trips, not
// to enable a sweep: a caller wanting more must say so in another call.
const maxDeletePaths = 100

var deleteFileSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "file_path": {
      "type": "string",
      "description": "Absolute path, file:// URI, or workspace-relative path of the file or empty directory to delete. Use paths instead to delete several in one call."
    },
    "paths": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Several files and/or empty directories to delete in one call (max 100). Same per-path rules as file_path — this batches round-trips, it does NOT delete recursively. Every path is validated before any is removed, and directories are removed after files, deepest first, so naming a tree's files and its directories together works in one call."
    },
    "dirty_ok": {
      "type": "boolean",
      "description": "Allow deleting a file that has uncommitted changes in its git repository. Default false — deletion is refused if the file is dirty. Pass true to proceed anyway."
    },
    "allow_dir": {
      "type": "boolean",
      "description": "Allow deleting an empty directory. Default false — deletion is refused for any directory. The directory must be empty; non-empty directories are rejected even with allow_dir: true."
    }
  },
  "additionalProperties": false
}`)

// DeleteFile removes a single file (not a directory) and notifies the LSP
// server via workspace/didChangeWatchedFiles with FileDeleted so symbol
// indexes drop the file's contents immediately.
//
// Concurrency: Execute is safe for concurrent use.
type DeleteFile struct{ deps WriteDeps }

func NewDeleteFile(deps WriteDeps) *DeleteFile { return &DeleteFile{deps: deps} }

func (*DeleteFile) Name() string                 { return "delete_file" }
func (*DeleteFile) InputSchema() json.RawMessage { return deleteFileSchema }
func (*DeleteFile) Description() string {
	return "Delete files and empty directories. Pass file_path for one, or paths for several in a single call " +
		"(max 100). Refuses to delete directories unless allow_dir: true is set — and even then only an EMPTY " +
		"directory is accepted (non-empty directories are always rejected; there is no recursive delete). " +
		"To remove a whole tree, list its files with find_files and pass them plus their directories in one " +
		"paths batch with allow_dir: true — every path is validated before anything is removed, and " +
		"directories go last, deepest first, so they are empty by the time their turn comes. The LSP server " +
		"is notified with FileDeleted so symbol indexes and diagnostics update immediately. Per-path locking " +
		"serialises against any concurrent write_file/edit_file targeting the same path. The response reports " +
		"the line and byte count removed (bytes only for a binary or oversized file)."
}

type deleteFileArgs struct {
	Path     string   `json:"file_path"`
	Paths    []string `json:"paths"`
	DirtyOk  bool     `json:"dirty_ok"`
	AllowDir bool     `json:"allow_dir"`
}

// deleteTarget is one validated path, resolved and classified, ready to remove.
type deleteTarget struct {
	path  string
	isDir bool
	size  int64
}

func (t *DeleteFile) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a deleteFileArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("delete_file: invalid arguments: %w", err)
	}
	requested, err := deleteRequestedPaths(a)
	if err != nil {
		return "", err
	}
	// One rate-limit token per path, all taken up front: a batch must not be a
	// way to spend one token on a hundred deletions, and refusing before the
	// first removal keeps a throttled batch from landing half-applied.
	for range requested {
		if !t.deps.limiter(ctx).Allow() {
			return "", rateLimitError("delete_file", t.deps.limiter(ctx))
		}
	}

	targets, err := t.validateDeleteTargets(ctx, a, requested)
	if err != nil {
		return "", err
	}

	var removed []string
	for _, tgt := range targets {
		line, err := t.removeTarget(ctx, tgt)
		if err != nil {
			// Report what already went, so a partial batch is never silent.
			if len(removed) > 0 {
				return deleteReport(removed), fmt.Errorf("%w (stopped after %d successful deletion(s))", err, len(removed))
			}
			return "", err
		}
		removed = append(removed, line)
	}
	return deleteReport(removed), nil
}

// deleteRequestedPaths reads the requested paths from either shape, rejecting
// an empty request and one past the batch cap.
func deleteRequestedPaths(a deleteFileArgs) ([]string, error) {
	switch {
	case a.Path != "" && len(a.Paths) > 0:
		return nil, errors.New("delete_file: pass file_path or paths, not both")
	case a.Path != "":
		return []string{a.Path}, nil
	case len(a.Paths) == 0:
		return nil, errors.New("delete_file: file_path (or paths) is required")
	case len(a.Paths) > maxDeletePaths:
		return nil, fmt.Errorf("delete_file: too many paths (%d); maximum is %d", len(a.Paths), maxDeletePaths)
	}
	return a.Paths, nil
}

// validateDeleteTargets resolves and checks EVERY path before any is removed, so
// a batch that is going to be refused is refused whole rather than part-applied.
// The returned order puts files first, then directories deepest-first, so a
// caller can name a tree's files and its directories in one call and each
// directory is empty by the time its turn comes.
func (t *DeleteFile) validateDeleteTargets(ctx context.Context, a deleteFileArgs, requested []string) ([]deleteTarget, error) {
	seen := make(map[string]bool, len(requested))
	targets := make([]deleteTarget, 0, len(requested))
	for _, p := range requested {
		if p == "" {
			return nil, errors.New("delete_file: paths must not contain an empty string")
		}
		path := t.deps.resolvePath(ctx, p)
		if err := t.deps.checkBoundary(ctx, path); err != nil {
			return nil, fmt.Errorf("delete_file: %w", err)
		}
		if seen[path] {
			continue // the same path named twice is not an error, just redundant
		}
		seen[path] = true

		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("delete_file: %w", err)
		}
		if info.IsDir() && !a.AllowDir {
			return nil, fmt.Errorf("delete_file: %q is a directory — pass allow_dir: true to delete an empty directory", path)
		}
		if !info.IsDir() && !a.DirtyOk && dirtyBlocksWrite(ctx, t.deps, path) {
			return nil, dirtyWrite(fmt.Errorf("delete_file: %q has uncommitted changes; "+
				"review and commit first, or pass dirty_ok: true to proceed", path))
		}
		targets = append(targets, deleteTarget{path: path, isDir: info.IsDir(), size: info.Size()})
	}

	sort.SliceStable(targets, func(i, j int) bool {
		if targets[i].isDir != targets[j].isDir {
			return !targets[i].isDir // files before directories
		}
		if !targets[i].isDir {
			return false // keep the caller's order among files
		}
		// Deepest directory first, so a parent is emptied before it is removed.
		return strings.Count(targets[i].path, string(filepath.Separator)) >
			strings.Count(targets[j].path, string(filepath.Separator))
	})
	return targets, nil
}

// removeTarget deletes one already-validated path and runs the post-delete
// notifications. Per-path locking is taken and released per path rather than
// held across the batch, so a large batch cannot stall every concurrent writer.
func (t *DeleteFile) removeTarget(ctx context.Context, tgt deleteTarget) (string, error) {
	unlock := lockPath(tgt.path)
	defer unlock()

	if tgt.isDir {
		if err := os.Remove(tgt.path); err != nil {
			return "", fmt.Errorf("delete_file: %w (directory must be empty)", err)
		}
		syncDirBestEffort("delete_file", filepath.Dir(tgt.path))
		t.deps.notifyTopology(tgt.path)
		return "deleted directory " + tgt.path, nil
	}

	// Summarise what is about to be removed (line + byte count) before deleting,
	// so the agent can report the scope of the change. Best-effort: a read error
	// degrades to the byte count from Stat.
	summary := deleteSummary(tgt.path, tgt.size)

	if err := os.Remove(tgt.path); err != nil {
		return "", fmt.Errorf("delete_file: %w", err)
	}
	syncDirBestEffort("delete_file", filepath.Dir(tgt.path))

	if err := notifyLSP(ctx, t.deps.Client, tgt.path, protocol.FileDeleted); err != nil {
		slog.Warn("delete_file: LSP notification failed", "path", tgt.path, "err", err)
	}
	invalidateCache(t.deps.Cache, "file://"+tgt.path)
	// processUpsert detects the missing file and routes to processDelete automatically.
	t.deps.notifyTopology(tgt.path)

	return fmt.Sprintf("deleted %s — %s", tgt.path, summary), nil
}

// deleteReport renders one line per removal, keeping the single-path response
// byte-identical to what it was before batching existed.
func deleteReport(removed []string) string {
	if len(removed) == 1 {
		return removed[0]
	}
	return fmt.Sprintf("deleted %d path(s):\n  ", len(removed)) + strings.Join(removed, "\n  ")
}

// deleteSummary describes the content removed by a delete: a line + byte count
// for a readable text file, falling back to bytes only for a binary file, one
// over maxReadFileBytes, or any that can't be read. size is the Stat size, used
// for the byte count and to skip reading oversized files.
func deleteSummary(path string, size int64) string {
	if size > maxReadFileBytes {
		return fmt.Sprintf("%d bytes removed", size)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("%d bytes removed", size)
	}
	sniff := data
	if len(sniff) > binarySniffBytes {
		sniff = sniff[:binarySniffBytes]
	}
	if bytes.IndexByte(sniff, 0) >= 0 {
		return fmt.Sprintf("%d bytes removed (binary)", len(data))
	}
	return fmt.Sprintf("%d lines, %d bytes removed", countTextLines(data), len(data))
}

// countTextLines counts lines the way an editor would: the number of newlines,
// plus one for a final line with no trailing newline. Empty content is 0 lines.
func countTextLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	n := bytes.Count(data, []byte{'\n'})
	if data[len(data)-1] != '\n' {
		n++
	}
	return n
}
