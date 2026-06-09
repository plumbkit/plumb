package topology

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"

	tsg "github.com/odvcencio/gotreesitter"

	"github.com/plumbkit/plumb/internal/langsupport"
)

// Indexer manages background extraction and persistence for one workspace.
// It owns a queue channel and a single background goroutine.
//
// Concurrency: all exported methods are safe for concurrent use.
type Indexer struct {
	workspace   string
	db          *sql.DB
	extractors  []Extractor
	maxSize     int64
	resyncMins  int           // 0 disables periodic resync
	resyncBatch int           // files per pause during a full resync; 0 disables pacing
	resyncPause time.Duration // pause between resync batches; 0 disables pacing

	queue chan indexOp
	done  chan struct{}
	wg    sync.WaitGroup

	mu            sync.RWMutex
	state         string
	lastSync      time.Time
	lastErr       string
	resyncPending bool // set when Enqueue overflows; triggers a recovery resync
}

// newIndexer creates an Indexer. Call Start() before enqueuing operations.
// resyncMins controls the optional periodic full-resync interval; 0 disables it.
func newIndexer(workspace string, db *sql.DB, exts []Extractor, maxSize int64, resyncMins int) *Indexer {
	if maxSize <= 0 {
		maxSize = 512 * 1024
	}
	return &Indexer{
		workspace:  workspace,
		db:         db,
		extractors: exts,
		maxSize:    maxSize,
		resyncMins: resyncMins,
		queue:      make(chan indexOp, 256),
		done:       make(chan struct{}),
		state:      "idle",
	}
}

// Start launches the background worker and enqueues an initial full resync.
func (idx *Indexer) Start() {
	idx.wg.Go(func() {
		idx.backgroundWorker()
	})
	idx.Enqueue("", opResync)
}

// Stop signals the background worker to exit and waits for it to drain its
// current operation before returning. The wg.Wait() ensures any in-progress
// transaction completes before the caller may close the database.
// Safe to call more than once; subsequent calls are no-ops.
func (idx *Indexer) Stop() {
	select {
	case <-idx.done:
	default:
		close(idx.done)
	}
	idx.wg.Wait()
}

// Enqueue adds a file operation to the background queue. Non-blocking; drops
// silently if the queue is full (capacity 256 is generous for typical usage).
func (idx *Indexer) Enqueue(path string, kind opKind) {
	select {
	case idx.queue <- indexOp{kind: kind, path: path}:
	default:
		// Queue full: rather than silently lose this change, flag a full resync
		// so the next worker cycle reconciles the whole tree and the index does
		// not drift out of sync with the filesystem.
		idx.mu.Lock()
		idx.resyncPending = true
		idx.mu.Unlock()
	}
}

// State returns the current indexer state string.
func (idx *Indexer) State() string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.state
}

// LastSync returns the time of the most recent completed resync.
func (idx *Indexer) LastSync() time.Time {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.lastSync
}

// LastError returns the most recent indexing error, or "".
func (idx *Indexer) LastError() string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.lastErr
}

func (idx *Indexer) setState(state, errMsg string) {
	idx.mu.Lock()
	idx.state = state
	if errMsg != "" {
		idx.lastErr = errMsg
	}
	if state == "idle" {
		idx.lastSync = time.Now()
	}
	idx.mu.Unlock()
}

func (idx *Indexer) backgroundWorker() {
	// Set up an optional periodic-resync ticker. A nil channel blocks forever,
	// so the select case is never chosen when resync is disabled.
	var tickC <-chan time.Time
	if idx.resyncMins > 0 {
		ticker := time.NewTicker(time.Duration(idx.resyncMins) * time.Minute)
		defer ticker.Stop()
		tickC = ticker.C
	}

	for {
		select {
		case <-idx.done:
			return
		case <-tickC:
			// Only enqueue when idle — don't pile up resyncs if a previous one
			// is still running.
			if idx.State() == "idle" {
				idx.Enqueue("", opResync)
			}
		case op := <-idx.queue:
			idx.runQueueCycle(op)
		}
	}
}

// runQueueCycle drains and processes all buffered ops, then runs a full resync
// when one was flagged by Enqueue after a queue overflow, so dropped per-file
// updates cannot leave the index permanently stale. The indexer state is set to
// error or idle based on the combined outcome.
func (idx *Indexer) runQueueCycle(initial indexOp) {
	ops := idx.drain(initial)
	idx.setState("running", "")
	var lastErr error
	for _, o := range ops {
		if err := idx.dispatch(context.Background(), o); err != nil {
			slog.Warn("topology: indexer error", "op", o.kind, "path", o.path, "err", err)
			lastErr = err
		}
	}
	if idx.takeResyncPending() {
		if err := idx.processResync(context.Background()); err != nil {
			slog.Warn("topology: recovery resync error", "err", err)
			lastErr = err
		}
	}
	if lastErr != nil {
		idx.setState("error", lastErr.Error())
	} else {
		idx.setState("idle", "")
	}
	if shouldReclaimAfterBurst(len(ops)) {
		// A coalesced burst (git checkout, a formatter) left a large transient
		// parse working set. A single small edit must NOT pay a stop-the-world GC,
		// so this is gated on the burst size.
		tsg.DrainArenaPools()
		debug.FreeOSMemory()
	}
}

// reclaimAfterOps is the burst size at which runQueueCycle reclaims transient
// parse memory. Below it the cost of a forced GC + FreeOSMemory outweighs the
// at-most-one pooled arena a small edit leaves behind.
const reclaimAfterOps = 64

// shouldReclaimAfterBurst reports whether a queue cycle processed enough files to
// warrant draining the parse-arena pool and returning pages to the OS.
func shouldReclaimAfterBurst(n int) bool {
	return n >= reclaimAfterOps
}

// takeResyncPending atomically reads and clears the pending-resync flag.
func (idx *Indexer) takeResyncPending() bool {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.resyncPending {
		idx.resyncPending = false
		return true
	}
	return false
}

// drain coalesces all buffered ops into a slice keeping the last op per unique
// path. This ensures every distinct file gets processed, while still collapsing
// rapid successive writes to the same file into a single operation.
func (idx *Indexer) drain(initial indexOp) []indexOp {
	seen := map[string]indexOp{initial.path: initial}
	for {
		select {
		case op := <-idx.queue:
			seen[op.path] = op // last write per path wins
		default:
			ops := make([]indexOp, 0, len(seen))
			for _, op := range seen {
				ops = append(ops, op)
			}
			return ops
		}
	}
}

func (idx *Indexer) dispatch(ctx context.Context, op indexOp) error {
	switch op.kind {
	case opUpsert:
		return idx.processUpsert(ctx, op.path)
	case opDelete:
		return idx.processDelete(ctx, op.path)
	default:
		return idx.processResync(ctx)
	}
}

func (idx *Indexer) processUpsert(ctx context.Context, relPath string) error {
	absPath := filepath.Join(idx.workspace, relPath)
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
	// resets mtime but changes content is still re-indexed.
	nodes, edges, hash, err := idx.extractPath(ctx, absPath, relPath)
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
	return idx.persistFile(fileID, relPath, info, hash, nodes, edges)
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

func (idx *Indexer) extractPath(ctx context.Context, absPath, relPath string) (nodes []Node, edges []Edge, hash string, err error) {
	ex := findExtractor(relPath, idx.extractors)
	if ex == nil {
		return nil, nil, "", nil
	}
	src, readErr := os.ReadFile(absPath) //nolint:gosec // G304: path derived from workspace root + relative path validated by caller
	if readErr != nil {
		return nil, nil, "", readErr
	}
	h := sha256.Sum256(src)
	hash = fmt.Sprintf("%x", h)
	if skipOversizedGrammar(relPath, ex.Language(), len(src)) {
		// Recorded with this hash and zero symbols, so isStale won't re-attempt it.
		return nil, nil, hash, nil
	}
	nodes, edges, err = safeExtract(ctx, ex, relPath, src)
	return nodes, edges, hash, err
}

// skipOversizedGrammar reports whether a file should be recorded without parsing
// because its grammar carries a per-grammar source-size cap (langsupport
// MaxParseBytes) that this file exceeds. GLR-heavy markup grammars (Markdown,
// HTML, YAML) can drive a pathological parse on a few-hundred-KB file for little
// outline value; the global max_file_size_bytes stays the outer bound.
func skipOversizedGrammar(relPath, lang string, srcLen int) bool {
	l, ok := langsupport.ByName(lang)
	if !ok || l.MaxParseBytes <= 0 || int64(srcLen) <= l.MaxParseBytes {
		return false
	}
	slog.Debug("topology: skipping oversized GLR grammar parse",
		"path", relPath, "lang", lang, "bytes", srcLen, "cap", l.MaxParseBytes)
	return true
}

// safeExtract wraps Extract in a recover so malformed files cannot panic the daemon.
func safeExtract(ctx context.Context, ex Extractor, relPath string, src []byte) (nodes []Node, edges []Edge, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("extractor panic: %v", r)
		}
	}()
	return ex.Extract(ctx, relPath, src)
}

func (idx *Indexer) persistFile(fileID int64, relPath string, info os.FileInfo, hash string, nodes []Node, edges []Edge) error {
	tx, err := idx.db.Begin()
	if err != nil {
		return fmt.Errorf("topology: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	newFileID, err := upsertFileRecord(tx, fileID, relPath, info, hash)
	if err != nil {
		return err
	}
	if err := deleteFileNodes(tx, newFileID); err != nil {
		return err
	}
	nodeIDs, err := insertNodes(tx, newFileID, relPath, nodes)
	if err != nil {
		return err
	}
	if err := insertEdges(tx, nodeIDs, edges); err != nil {
		return err
	}
	return tx.Commit()
}

func (idx *Indexer) recordFileError(relPath string, info os.FileInfo, extractErr error) error {
	_, err := idx.db.Exec(
		`INSERT INTO topology_files(path, mtime_ns, error_msg) VALUES (?, ?, ?)
         ON CONFLICT(path) DO UPDATE SET mtime_ns=excluded.mtime_ns, error_msg=excluded.error_msg`,
		relPath, info.ModTime().UnixNano(), extractErr.Error())
	return err
}

func upsertFileRecord(tx *sql.Tx, fileID int64, relPath string, info os.FileInfo, hash string) (int64, error) {
	if fileID == 0 {
		res, err := tx.Exec(
			`INSERT INTO topology_files(path, mtime_ns, content_hash, indexed_at, error_msg)
             VALUES (?, ?, ?, ?, '')`,
			relPath, info.ModTime().UnixNano(), hash, time.Now().UnixNano())
		if err != nil {
			return 0, fmt.Errorf("topology: insert file: %w", err)
		}
		id, _ := res.LastInsertId()
		return id, nil
	}
	_, err := tx.Exec(
		`UPDATE topology_files SET mtime_ns=?, content_hash=?, indexed_at=?, error_msg='' WHERE id=?`,
		info.ModTime().UnixNano(), hash, time.Now().UnixNano(), fileID)
	if err != nil {
		return 0, fmt.Errorf("topology: update file: %w", err)
	}
	return fileID, nil
}

func deleteFileNodes(tx *sql.Tx, fileID int64) error {
	rows, err := tx.Query(`SELECT id FROM topology_nodes WHERE file_id = ?`, fileID)
	if err != nil {
		return fmt.Errorf("topology: list nodes: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		if _, err := tx.Exec(`DELETE FROM topology_fts WHERE rowid = ?`, id); err != nil {
			return fmt.Errorf("topology: delete fts: %w", err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM topology_nodes WHERE file_id = ?`, fileID); err != nil {
		return fmt.Errorf("topology: delete nodes: %w", err)
	}
	return nil
}

func insertNodes(tx *sql.Tx, fileID int64, relPath string, nodes []Node) ([]int64, error) {
	ids := make([]int64, 0, len(nodes))
	for i := range nodes {
		n := &nodes[i]
		n.FileID = fileID
		res, err := tx.Exec(
			`INSERT INTO topology_nodes(file_id, kind, name, qualified, signature, start_line, end_line, docstring, language)
             VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			fileID, string(n.Kind), n.Name, n.Qualified, n.Signature, n.StartLine, n.EndLine, n.Docstring, n.Language)
		if err != nil {
			return nil, fmt.Errorf("topology: insert node: %w", err)
		}
		id, _ := res.LastInsertId()
		n.ID = id
		ids = append(ids, id)
		tokens := splitIdentifier(n.Name)
		if _, err := tx.Exec(
			`INSERT INTO topology_fts(rowid, name, name_tokens, qualified, signature, docstring, path, kind)
             VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			id, n.Name, tokens, n.Qualified, n.Signature, n.Docstring, relPath, string(n.Kind)); err != nil {
			return nil, fmt.Errorf("topology: insert fts: %w", err)
		}
	}
	return ids, nil
}

// insertEdges persists edges, remapping extractor-local node indices to DB rowIDs.
// Extractors set FromID/ToID as 0-based indices into the returned nodes slice.
// The indexer remaps these to actual DB rowIDs using the nodeIDs slice.
func insertEdges(tx *sql.Tx, nodeIDs []int64, edges []Edge) error {
	if len(nodeIDs) == 0 || len(edges) == 0 {
		return nil
	}
	for _, e := range edges {
		fromID := remapNodeID(e.FromID, nodeIDs)
		toID := remapNodeID(e.ToID, nodeIDs)
		if fromID == 0 || toID == 0 {
			continue
		}
		if _, err := tx.Exec(
			`INSERT INTO topology_edges(from_id, to_id, kind, confidence, source)
             VALUES (?, ?, ?, ?, ?)`,
			fromID, toID, string(e.Kind), e.Confidence, e.Source); err != nil {
			return fmt.Errorf("topology: insert edge: %w", err)
		}
	}
	return nil
}

// remapNodeID translates a 0-based extractor node index to a DB rowID.
// Returns 0 (skip) when the index is out of range.
func remapNodeID(idx int64, nodeIDs []int64) int64 {
	if idx < 0 || int(idx) >= len(nodeIDs) {
		return 0
	}
	return nodeIDs[idx]
}

func (idx *Indexer) processDelete(ctx context.Context, relPath string) error {
	_ = ctx
	var fileID int64
	row := idx.db.QueryRow(`SELECT id FROM topology_files WHERE path = ?`, relPath)
	if err := row.Scan(&fileID); err == sql.ErrNoRows {
		return nil
	} else if err != nil {
		return err
	}
	tx, err := idx.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := deleteFileNodes(tx, fileID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM topology_files WHERE id = ?`, fileID); err != nil {
		return err
	}
	return tx.Commit()
}

// errResyncAborted is returned through filepath.Walk when the indexer is
// stopping (or its context is cancelled) mid-resync, so processResync can skip
// pruning — a partial walk must not delete files it simply hasn't visited yet.
var errResyncAborted = errors.New("topology: resync aborted")

func (idx *Indexer) processResync(ctx context.Context) error {
	present := make(map[string]bool)
	processed := 0
	err := filepath.Walk(idx.workspace, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			// Surface permission errors and other walk failures in the last-error field
			// rather than silently swallowing them.
			slog.Warn("topology: resync walk error", "path", path, "err", walkErr)
			return nil
		}
		if info.IsDir() {
			if shouldSkipDir(info.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(idx.workspace, path)
		if relErr != nil {
			return nil
		}
		present[rel] = true
		if upErr := idx.processUpsert(ctx, rel); upErr != nil {
			return upErr
		}
		processed++
		return idx.pace(ctx, processed)
	})
	if errors.Is(err, errResyncAborted) {
		// Shutting down: skip prune so a partial walk cannot delete live files.
		return nil
	}
	if err != nil {
		return fmt.Errorf("topology: resync walk: %w", err)
	}
	if err := idx.pruneDeleted(present); err != nil {
		return err
	}
	// A full resync builds a large transient working set (file reads, parse
	// trees, node/edge slices). Release the pooled parse arena to the GC, then
	// hand the freed pages back to the OS so RSS and HeapSys settle to steady
	// state instead of lingering at the walk's peak.
	tsg.DrainArenaPools()
	debug.FreeOSMemory()
	return nil
}

// pace throttles the full resync walk: after every resyncBatch files it pauses
// for resyncPause, yielding CPU to live tool calls and other workspaces sharing
// the daemon. It returns errResyncAborted when the indexer is stopping or the
// context is cancelled. A zero batch or pause disables pacing entirely.
func (idx *Indexer) pace(ctx context.Context, processed int) error {
	if idx.resyncBatch <= 0 || idx.resyncPause <= 0 || processed%idx.resyncBatch != 0 {
		return nil
	}
	select {
	case <-idx.done:
		return errResyncAborted
	case <-ctx.Done():
		return errResyncAborted
	case <-time.After(idx.resyncPause):
		return nil
	}
}

func (idx *Indexer) pruneDeleted(present map[string]bool) error {
	rows, err := idx.db.Query(`SELECT id, path FROM topology_files`)
	if err != nil {
		return err
	}
	type entry struct {
		id   int64
		path string
	}
	var stale []entry
	for rows.Next() {
		var e entry
		if rows.Scan(&e.id, &e.path) == nil && !present[e.path] {
			stale = append(stale, e)
		}
	}
	rows.Close()
	var firstErr error
	for _, e := range stale {
		tx, txErr := idx.db.Begin()
		if txErr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("topology: prune begin tx for %q: %w", e.path, txErr)
			}
			continue
		}
		if err := deleteFileNodes(tx, e.id); err != nil {
			_ = tx.Rollback()
			if firstErr == nil {
				firstErr = fmt.Errorf("topology: prune nodes for %q: %w", e.path, err)
			}
			continue
		}
		if _, err := tx.Exec(`DELETE FROM topology_files WHERE id = ?`, e.id); err != nil {
			_ = tx.Rollback()
			if firstErr == nil {
				firstErr = fmt.Errorf("topology: prune file %q: %w", e.path, err)
			}
			continue
		}
		if err := tx.Commit(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("topology: prune commit for %q: %w", e.path, err)
		}
	}
	return firstErr
}

// shouldSkipDir returns true for directories that should never be indexed.
// Dot-prefixed directories (hidden dirs like .vscode, .idea, .venv) are always
// skipped to avoid indexing editor artefacts and virtual environments.
func shouldSkipDir(name string) bool {
	if len(name) > 1 && name[0] == '.' {
		return true
	}
	switch name {
	case "vendor", "node_modules", "testdata", "dist", "build", "__pycache__":
		return true
	}
	return false
}
