package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/golimpio/plumb/internal/lsp/protocol"
	"github.com/golimpio/plumb/internal/tools/txlog"
)

var transactionApplySchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "dirty_ok": {
      "type": "boolean",
      "description": "Allow editing files that have uncommitted changes in their git repository. Default false — the transaction is refused if any target file is dirty. Pass true to proceed anyway."
    },
    "operations": {
      "type": "array",
      "description": "Ordered list of per-file edit groups. Every file is validated first; only if all validate do any writes happen.",
      "items": {
        "type": "object",
        "properties": {
          "file_path": {
            "type": "string",
            "description": "Absolute path or file:// URI of the file to edit."
          },
          "edits": {
            "type": "array",
            "description": "str_replace edits applied in order. Same semantics as edit_file: each old_string must appear EXACTLY ONCE.",
            "items": {
              "type": "object",
              "properties": {
                "old_string": {"type": "string"},
                "new_string": {"type": "string"}
              },
              "required": ["old_string", "new_string"],
              "additionalProperties": false
            },
            "minItems": 1
          },
          "expected_mtime": {
            "type": "string",
            "description": "Optional RFC3339Nano mtime previously returned by read_file. If provided, the operation is rejected if the file's current mtime differs."
          },
          "expected_sha": {
            "type": "string",
            "description": "Optional hex-encoded SHA-256 previously returned by read_file. If provided, the operation is rejected if the file's current content hash differs."
          }
        },
        "required": ["file_path", "edits"],
        "additionalProperties": false
      },
      "minItems": 1,
      "maxItems": 50
    }
  },
  "required": ["operations"],
  "additionalProperties": false
}`)

// TransactionApply applies multi-file edits atomically:
//
//  1. Acquire per-path locks for every target in lexical order (deadlock-safe).
//  2. Snapshot every target's content + mtime; validate every edit in memory.
//     If any edit fails (old_string missing/ambiguous, expected_mtime mismatch),
//     no writes happen.
//  3. Apply writes via safeWrite. If any write fails partway, the
//     already-written files are restored to their pre-transaction content.
//  4. notifyLSP + invalidateCache per file on success.
//
// Concurrency: per-path locks serialise with concurrent write_file / edit_file
// / delete_file / rename_file calls to the same paths. The transaction holds
// all locks for its duration — keep transactions small.
//
// Limits: up to 50 operations per call. Rate limit applies once per operation.
type TransactionApply struct{ deps WriteDeps }

func NewTransactionApply(deps WriteDeps) *TransactionApply {
	return &TransactionApply{deps: deps}
}

func (*TransactionApply) Name() string                 { return "transaction_apply" }
func (*TransactionApply) InputSchema() json.RawMessage { return transactionApplySchema }
func (*TransactionApply) Description() string {
	return "No native Claude Code equivalent. " +
		"Apply str_replace edits across multiple files atomically. Every operation is " +
		"validated against the on-disk content first; if any old_string is missing or " +
		"ambiguous, NO files are written. If writes start succeeding but one fails partway, " +
		"the already-written files are rolled back to their pre-transaction content. " +
		"Per-path locks prevent interleaving with other write tools. Use for refactors " +
		"that must land as one unit (cross-file rename of a string, coordinated config + " +
		"caller updates, etc.). Up to 50 operations per call."
}

type txOperation struct {
	Path          string    `json:"file_path"`
	Edits         []strEdit `json:"edits"`
	ExpectedMtime string    `json:"expected_mtime"`
	ExpectedSha   string    `json:"expected_sha"`
}

type transactionApplyArgs struct {
	DirtyOk    bool          `json:"dirty_ok"`
	Operations []txOperation `json:"operations"`
}

// txPrepared is the in-memory result of validating one operation: the
// pre-edit content, the post-edit content, the file's pre-write mtime, and
// the file mode for the eventual safeWrite.
type txPrepared struct {
	path     string
	before   string
	after    string
	preMtime time.Time
	perm     os.FileMode
}

func (t *TransactionApply) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	a, err := parseTransactionArgs(raw)
	if err != nil {
		return "", err
	}
	if err := t.txCheckRateLimits(a); err != nil {
		return "", err
	}

	paths, err := txCanonicalPaths(a.Operations)
	if err != nil {
		return "", err
	}

	// Acquire per-path locks in lexical order (deadlock-safe with parallel txs).
	unlocks := make([]func(), 0, len(paths))
	for _, p := range paths {
		unlocks = append(unlocks, lockPath(p))
	}
	defer func() {
		for _, u := range unlocks {
			u()
		}
	}()

	if err := txDirtyCheck(ctx, t.deps.Writes, paths, a.DirtyOk); err != nil {
		return "", err
	}

	prepared, err := txPhase1Validate(a.Operations)
	if err != nil {
		return "", err
	}

	written, err := t.txPhase2Write(prepared)
	if err != nil {
		return "", err
	}

	t.txPhase3Notify(ctx, written)
	var result strings.Builder
	result.WriteString(formatTransactionResult(written))
	for _, w := range written {
		t.deps.notifyTopology(w.path)
		result.WriteString(t.deps.reportQuality(ctx, w.path))
	}
	return result.String(), nil
}

func parseTransactionArgs(raw json.RawMessage) (transactionApplyArgs, error) {
	var a transactionApplyArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("transaction_apply: invalid arguments: %w", err)
	}
	if len(a.Operations) == 0 {
		return a, fmt.Errorf("transaction_apply: at least one operation required")
	}
	if len(a.Operations) > 50 {
		return a, fmt.Errorf("transaction_apply: at most 50 operations per call, got %d", len(a.Operations))
	}
	return a, nil
}

func (t *TransactionApply) txCheckRateLimits(a transactionApplyArgs) error {
	for i := range a.Operations {
		if !t.deps.Limiter.Allow() {
			return rateLimitError(fmt.Sprintf("transaction_apply (op %d/%d)", i+1, len(a.Operations)), t.deps.Limiter)
		}
	}
	return nil
}

// txCanonicalPaths deduplicates and lexically sorts the operation paths.
func txCanonicalPaths(ops []txOperation) ([]string, error) {
	paths := make([]string, 0, len(ops))
	seen := make(map[string]struct{}, len(ops))
	for _, op := range ops {
		p := strings.TrimPrefix(op.Path, "file://")
		if _, dup := seen[p]; dup {
			return nil, &editLogicErr{fmt.Errorf(
				"transaction_apply: path %q appears in multiple operations — combine them into one operation with multiple edits", p,
			)}
		}
		seen[p] = struct{}{}
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths, nil
}

// txDirtyCheck batches paths by directory and refuses if any are dirty and not
// written by plumb this session (writes).
func txDirtyCheck(ctx context.Context, writes *WriteTracker, paths []string, dirtyOk bool) error {
	if dirtyOk {
		return nil
	}
	type dirBatch struct {
		bases []string
		fulls []string
	}
	batches := make(map[string]*dirBatch, len(paths))
	for _, p := range paths {
		dir := filepath.Dir(p)
		if batches[dir] == nil {
			batches[dir] = &dirBatch{}
		}
		batches[dir].bases = append(batches[dir].bases, filepath.Base(p))
		batches[dir].fulls = append(batches[dir].fulls, p)
	}
	var dirtyPaths []string
	for dir, batch := range batches {
		dirty := dirtyBasenamesInDir(ctx, dir, batch.bases, false)
		for i, base := range batch.bases {
			if dirty[base] && !writes.Wrote(batch.fulls[i]) {
				dirtyPaths = append(dirtyPaths, batch.fulls[i])
			}
		}
	}
	if len(dirtyPaths) > 0 {
		sort.Strings(dirtyPaths)
		return &editLogicErr{fmt.Errorf(
			"transaction_apply: %d file(s) have uncommitted changes; "+
				"review and commit first, or pass dirty_ok: true to overwrite:\n  %s",
			len(dirtyPaths), strings.Join(dirtyPaths, "\n  "),
		)}
	}
	return nil
}

// txPhase1Validate validates every operation in memory. No writes happen.
func txPhase1Validate(ops []txOperation) ([]txPrepared, error) {
	prepared := make([]txPrepared, 0, len(ops))
	for i, op := range ops {
		p, err := txValidateOp(i, op, strings.TrimPrefix(op.Path, "file://"))
		if err != nil {
			return nil, err
		}
		prepared = append(prepared, p)
	}
	return prepared, nil
}

// txValidateOp validates a single operation against the current on-disk state
// and returns the prepared in-memory result.
func txValidateOp(i int, op txOperation, path string) (txPrepared, error) {
	info, err := os.Stat(path)
	if err != nil {
		return txPrepared{}, &editLogicErr{fmt.Errorf("transaction_apply: op[%d]: stat %q: %w", i, path, err)}
	}
	if op.ExpectedMtime != "" {
		want, perr := time.Parse(time.RFC3339Nano, op.ExpectedMtime)
		if perr != nil {
			return txPrepared{}, &editLogicErr{fmt.Errorf("transaction_apply: op[%d]: expected_mtime not RFC3339Nano: %w", i, perr)}
		}
		if !info.ModTime().Equal(want) {
			return txPrepared{}, &editLogicErr{fmt.Errorf(
				"transaction_apply: op[%d]: %q changed since you read it (expected %s, got %s)",
				i, path, want.Format(time.RFC3339Nano), info.ModTime().Format(time.RFC3339Nano),
			)}
		}
	}
	if op.ExpectedSha != "" {
		current, err := fileSHA256(path)
		if err != nil {
			return txPrepared{}, &editLogicErr{fmt.Errorf("transaction_apply: op[%d]: computing sha256 of %q: %w", i, path, err)}
		}
		if current != op.ExpectedSha {
			return txPrepared{}, &editLogicErr{fmt.Errorf(
				"transaction_apply: op[%d]: %q content has changed since you read it\n"+
					"  expected sha256: %s\n"+
					"  current  sha256: %s",
				i, path, op.ExpectedSha, current,
			)}
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return txPrepared{}, &editLogicErr{fmt.Errorf("transaction_apply: op[%d]: read %q: %w", i, path, err)}
	}
	before := string(data)
	content := before
	for j, edit := range op.Edits {
		if edit.OldStr == "" {
			return txPrepared{}, &editLogicErr{fmt.Errorf("transaction_apply: op[%d].edits[%d]: old_string must not be empty", i, j)}
		}
		oldStr := matchLineEndings(edit.OldStr, content)
		newStr := matchLineEndings(edit.NewStr, content)
		count := strings.Count(content, oldStr)
		switch count {
		case 0:
			return txPrepared{}, &editLogicErr{fmt.Errorf(
				"transaction_apply: op[%d].edits[%d]: old_string not found in %q",
				i, j, path,
			)}
		case 1:
			content = strings.Replace(content, oldStr, newStr, 1)
		default:
			return txPrepared{}, &editLogicErr{fmt.Errorf(
				"transaction_apply: op[%d].edits[%d]: old_string appears %d times in %q — must be unique",
				i, j, count, path,
			)}
		}
	}
	return txPrepared{
		path:     path,
		before:   before,
		after:    content,
		preMtime: info.ModTime(),
		perm:     info.Mode().Perm(),
	}, nil
}

// txPhase2Write writes all prepared operations with an in-memory mtime guard
// and a durable rollback log. Rolls back already-written files on failure.
func (t *TransactionApply) txPhase2Write(prepared []txPrepared) ([]txPrepared, error) {
	workspace := ""
	if t.deps.WorkspaceFn != nil {
		workspace = t.deps.WorkspaceFn()
	}
	txl, txErr := txlog.Begin(workspace)
	if txErr != nil {
		slog.Warn("transaction_apply: txlog unavailable — rollback not durable", "err", txErr)
		txl, _ = txlog.Begin("")
	}

	written := make([]txPrepared, 0, len(prepared))
	for _, p := range prepared {
		if info, err := os.Stat(p.path); err == nil {
			if !info.ModTime().Equal(p.preMtime) {
				rollback(written)
				txl.Rollback()
				return nil, fmt.Errorf(
					"transaction_apply: %q changed during transaction (mtime moved); rolled back %d writes",
					p.path, len(written),
				)
			}
		}
		if err := txl.Record(p.path, []byte(p.before), p.perm); err != nil {
			slog.Warn("transaction_apply: txlog record failed — this write is not durable",
				"path", p.path, "err", err)
		}
		if _, err := safeWrite(p.path, []byte(p.after), p.perm); err != nil {
			rollback(written)
			txl.Rollback()
			return nil, fmt.Errorf("transaction_apply: write %q failed: %w; rolled back %d writes",
				p.path, err, len(written))
		}
		written = append(written, p)
	}
	txl.Commit()
	return written, nil
}

// txPhase3Notify sends LSP notifications and invalidates the symbol cache for
// every successfully written file.
func (t *TransactionApply) txPhase3Notify(ctx context.Context, written []txPrepared) {
	for _, p := range written {
		uri := "file://" + p.path
		if err := notifyLSP(ctx, t.deps.Client, p.path, protocol.FileChanged); err != nil {
			slog.Warn("transaction_apply: LSP notification failed", "path", p.path, "err", err)
		}
		if t.deps.PostWriteNotifyFn != nil {
			if err := t.deps.PostWriteNotifyFn(ctx, p.path); err != nil {
				slog.Warn("transaction_apply: post-write adapter notification failed", "path", p.path, "err", err)
			}
		}
		invalidateCache(t.deps.Cache, uri)
		t.deps.Writes.Record(p.path)
	}
}

func formatTransactionResult(written []txPrepared) string {
	var sb strings.Builder
	totalBytes := 0
	for _, p := range written {
		totalBytes += len(p.after)
	}
	fmt.Fprintf(&sb, "transaction applied: %d files updated (%d bytes total)\n", len(written), totalBytes)
	for _, p := range written {
		summary := summariseLineChanges(p.before, p.after)
		fmt.Fprintf(&sb, "  %s", p.path)
		if summary != "" {
			fmt.Fprintf(&sb, " — %s", summary)
		}
		sb.WriteByte('\n')
	}
	return strings.TrimRight(sb.String(), "\n")
}

// rollback restores each entry in written to its pre-transaction content.
// Best-effort: failures are logged and proceed. If a rollback write itself
// fails, the file is left in the post-write state and the caller has lost
// atomicity — but a partial application is the only outcome possible at
// that point.
func rollback(written []txPrepared) {
	for _, p := range written {
		if _, err := safeWrite(p.path, []byte(p.before), p.perm); err != nil {
			slog.Error("transaction_apply: rollback failed", "path", p.path, "err", err)
		}
	}
}
