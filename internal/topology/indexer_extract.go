package topology

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/langsupport"
)

// This file holds the indexer's per-file path: turning one changed file into
// nodes and edges (read, hash, per-grammar cap, extract) and the staleness
// check that decides whether a re-index is needed. See indexer.go for the
// worker loop, indexer_persist.go for the DB writes, and indexer_resync.go for
// the full-tree walk.

func (idx *Indexer) processUpsert(ctx context.Context, relPath string) error {
	absPath := filepath.Join(idx.workspace, relPath)
	if symlinkEscapesWorkspace(idx.workspace, absPath) {
		// Drop anything a previous (unguarded) index recorded for this path, so a
		// database poisoned before this guard existed heals on the next resync
		// rather than keeping the outside file's symbols searchable forever.
		return idx.processDelete(ctx, relPath)
	}
	info, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return idx.processDelete(ctx, relPath)
		}
		return err
	}
	if info.IsDir() || info.Size() > idx.maxSize {
		return nil
	}
	// Read and hash before the staleness check so a backup-restore that
	// resets mtime but changes content is still re-indexed; the content hash
	// genuinely needs the file read, but the expensive parse does not — so the
	// parse is deferred to extractFile and runs only once the file is stale.
	src, ex, lang, hash, err := idx.readAndHash(absPath, relPath)
	if err != nil {
		return idx.recordFileError(relPath, info, err)
	}
	stale, fileID, err := idx.isStale(relPath, info, hash)
	if err != nil {
		return err
	}
	if !stale {
		return nil
	}
	nodes, edges, err := idx.extractFile(ctx, ex, relPath, src)
	if err != nil {
		return idx.recordFileError(relPath, info, err)
	}
	return idx.persistFile(fileID, relPath, info, hash, lang, nodes, edges)
}

// symlinkEscapesWorkspace reports whether absPath is a symlink whose target
// resolves outside the workspace.
//
// The indexer must refuse those. os.Stat below follows the link and the target
// is read, parsed, and PERSISTED into .plumb/topology.db, where topology_search
// and workspace_search surface its symbols long after the call that indexed it
// — so an out-of-tree read here outlives the read. Git stores symlinks natively,
// so cloning a repository that commits one is enough to plant it.
//
// This is the same escape internal/tools' walk guards (escapesBoundary), decided
// against a different authority: the indexer has one workspace root, not the MCP
// connection's multi-root path policy, and Intelligence may not import
// Application. An unresolvable link is treated as escaping — it has no readable
// target either way, so failing closed costs nothing.
func symlinkEscapesWorkspace(workspace, absPath string) bool {
	fi, err := os.Lstat(absPath)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return false
	}
	target, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return true
	}
	root, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		root = filepath.Clean(workspace)
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return true
	}
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// isStale returns true when either the mtime or the content hash differs from
// the stored values — whichever changes triggers a re-index. This catches
// backup-restores that produce an older mtime with different content.
func (idx *Indexer) isStale(relPath string, info os.FileInfo, hash string) (stale bool, fileID int64, err error) {
	var dbMtime int64
	var dbHash string
	row := idx.db.QueryRow(`SELECT id, mtime_ns, content_hash FROM topology_files WHERE path = ?`, relPath)
	if scanErr := row.Scan(&fileID, &dbMtime, &dbHash); scanErr == sql.ErrNoRows {
		return true, 0, nil
	} else if scanErr != nil {
		return false, 0, fmt.Errorf("topology: query file: %w", scanErr)
	}
	return dbMtime != info.ModTime().UnixNano() || dbHash != hash, fileID, nil
}

// readAndHash resolves the extractor for relPath, reads the file, and hashes its
// content. The parse is deliberately not run here so processUpsert can discard an
// unchanged file (the common case on a full resync) without paying the parse. When
// no extractor matches it returns a nil extractor and an empty hash — preserving
// the behaviour where such a file is recorded with zero symbols so the staleness
// check never re-attempts it.
//
// It still names the language when the registry recognises the extension but has
// no extractor for it (Structural == EngineNone), so the row records WHY it holds
// no symbols. The empty hash is what makes this self-healing: wiring an extractor
// later makes the computed hash differ from the stored "", the file goes stale,
// and it is re-indexed with no schema change. The pairing — a language with no
// hash — is exactly what the uncovered census counts, which is why it is cheap to
// report and impossible to confuse with a parsed file.
func (idx *Indexer) readAndHash(absPath, relPath string) (src []byte, ex Extractor, lang, hash string, err error) {
	ex = findExtractor(relPath, idx.extractors)
	if ex == nil {
		return nil, nil, uncoveredLanguage(relPath), "", nil
	}
	src, err = os.ReadFile(absPath) //nolint:gosec // G304: path derived from workspace root + relative path validated by caller
	if err != nil {
		return nil, nil, "", "", err
	}
	h := sha256.Sum256(src)
	return src, ex, ex.Language(), hex.EncodeToString(h[:]), nil
}

// uncoveredLanguage names the language of a file the registry recognises but
// cannot index, and returns "" for anything it does not recognise at all
// (binaries, lockfiles, images). Guarding on EngineNone rather than on "the
// registry knows this extension" keeps the two apart: a supported language that
// reaches here would be a wiring bug, not a coverage gap, and must not be
// reported as one.
func uncoveredLanguage(relPath string) string {
	l, ok := langsupport.ByPath(relPath)
	if !ok || l.Structural != langsupport.EngineNone {
		return ""
	}
	return l.Name
}

// maxExtractTimeout is the ceiling on any one file's parse, applied even when
// the operator has configured no timeout at all.
//
// extract_timeout_seconds = 0 WAS documented as "disables the timeout", and it
// did exactly that: no deadline on the context, so extractWith set no parser
// timeout either, and safeExtract's watchdog waits on a ctx.Done() that never
// fires. A single pathological file could therefore wedge the one indexer worker
// permanently, with every later edit queued behind it — no error, no log, and no
// recovery short of restarting the daemon.
//
// An unresponsive index is a worse failure than a missed file. A file that times
// out is recorded as an error and retried on the next resync: visible and
// self-correcting. A wedged worker silently stops indexing the entire workspace
// and looks, from the outside, exactly like a workspace with nothing in it. So
// the setting may still LOWER this bound — that is ordinary tuning — but it can
// no longer remove it.
//
// Two minutes sits far above any legitimate single-file parse (the slowest
// measured real file, an 861 KB Markdown document, takes ~370 ms) and far below
// the point at which someone would fail to notice the index had stopped.
const maxExtractTimeout = 2 * time.Minute

// effectiveExtractTimeout resolves the configured timeout against the ceiling,
// treating "disabled" and "longer than the ceiling" identically.
func effectiveExtractTimeout(configured time.Duration) time.Duration {
	if configured <= 0 || configured > maxExtractTimeout {
		return maxExtractTimeout
	}
	return configured
}

// extractFile runs the extractor for a file that isStale has confirmed needs
// re-indexing. A nil extractor (no language match) yields zero nodes, matching
// the records persisted by the pre-reorder path. The parse ALWAYS runs under a
// deadline so a pathological file cannot stall the single indexer worker; on
// expiry the file is recorded as an error by the caller and the worker moves
// on.
func (idx *Indexer) extractFile(ctx context.Context, ex Extractor, relPath string, src []byte) (nodes []Node, edges []Edge, err error) {
	if ex == nil {
		return nil, nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, effectiveExtractTimeout(idx.extractTimeout))
	defer cancel()
	return safeExtract(ctx, ex, relPath, src)
}

// safeExtract wraps Extract in a recover so malformed files cannot panic the
// daemon, and abandons it when ctx expires so a parse that runs away cannot
// wedge the caller.
//
// The abandoned goroutine is NOT killed — nothing here can stop a parse already
// running inside a grammar. The gotreesitter parsers bound themselves from the
// same ctx (SetTimeoutMicros). A wasm parse is NOT ctx-interruptible — the
// interruptible wazero mode measured 4.8x slower and was rejected (see
// wasmts.newRuntime) — so on deadline the wasm extractor stops waiting, drops
// the abandoned goroutine's late result, and discards the runtime WITHOUT
// closing it (the stuck goroutine is still executing inside), so later files
// get a fresh runtime rather than serialising behind the stuck parse's lock.
// This watchdog is the backstop for an engine that honours neither: it frees
// the indexer worker to keep going while the orphan winds down on its own.
//
// A context that is already dead starts no extract at all — the same contract
// both engines below enforce (wasmts.Extract, treesitter.extractWith). Without
// the guard the select below races an already-closed ctx.Done() against a
// goroutine this call itself spawned, so a fast extractor could win and return
// a result where the caller was promised ctx.Err().
func safeExtract(ctx context.Context, ex Extractor, relPath string, src []byte) (nodes []Node, edges []Edge, err error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, nil, fmt.Errorf("extract %s: %w", relPath, ctxErr)
	}
	type result struct {
		nodes []Node
		edges []Edge
		err   error
	}
	// Buffered so an abandoned extract can always send and exit rather than block
	// on a receiver that has already given up.
	done := make(chan result, 1)
	started := time.Now()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- result{err: fmt.Errorf("extractor panic: %v", r)}
			}
		}()
		n, e, xerr := ex.Extract(ctx, relPath, src)
		done <- result{nodes: n, edges: e, err: xerr}
	}()

	select {
	case r := <-done:
		return r.nodes, r.edges, r.err
	case <-ctx.Done():
		slog.Warn("topology: abandoning slow extract",
			"path", relPath, "lang", ex.Language(),
			"bytes", len(src), "elapsed", time.Since(started))
		return nil, nil, fmt.Errorf("extract %s: %w", relPath, ctx.Err())
	}
}
