package topology

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Indexer manages background extraction and persistence for one workspace.
// It owns a queue channel and a single background goroutine.
//
// Concurrency: all exported methods are safe for concurrent use.
type Indexer struct {
	workspace  string
	db         *sql.DB
	extractors []Extractor
	maxSize    int64

	queue chan indexOp
	done  chan struct{}

	mu       sync.RWMutex
	state    string
	lastSync time.Time
	lastErr  string
}

// newIndexer creates an Indexer. Call Start() before enqueuing operations.
func newIndexer(workspace string, db *sql.DB, exts []Extractor, maxSize int64) *Indexer {
	if maxSize <= 0 {
		maxSize = 512 * 1024
	}
	return &Indexer{
		workspace:  workspace,
		db:         db,
		extractors: exts,
		maxSize:    maxSize,
		queue:      make(chan indexOp, 256),
		done:       make(chan struct{}),
		state:      "idle",
	}
}

// Start launches the background worker and enqueues an initial full resync.
func (idx *Indexer) Start() {
	go idx.backgroundWorker()
	idx.Enqueue("", opResync)
}

// Stop shuts down the background worker. Safe to call more than once.
func (idx *Indexer) Stop() {
	select {
	case <-idx.done:
	default:
		close(idx.done)
	}
}

// Enqueue adds a file operation to the background queue. Non-blocking; drops
// silently if the queue is full (capacity 256 is generous for typical usage).
func (idx *Indexer) Enqueue(path string, kind opKind) {
	select {
	case idx.queue <- indexOp{kind: kind, path: path}:
	default:
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
	for {
		select {
		case <-idx.done:
			return
		case op := <-idx.queue:
			op = idx.drain(op)
			idx.setState("running", "")
			if err := idx.dispatch(context.Background(), op); err != nil {
				slog.Warn("topology: indexer error", "op", op.kind, "path", op.path, "err", err)
				idx.setState("error", err.Error())
			} else {
				idx.setState("idle", "")
			}
		}
	}
}

// drain reads all pending ops from the queue and returns the last one.
// This coalesces bursts of changes into a single operation.
func (idx *Indexer) drain(initial indexOp) indexOp {
	last := initial
	for {
		select {
		case op := <-idx.queue:
			last = op
		default:
			return last
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
	stale, fileID, err := idx.isStale(relPath, info)
	if err != nil {
		return err
	}
	if !stale {
		return nil
	}
	nodes, edges, hash, err := idx.extractPath(ctx, absPath, relPath)
	if err != nil {
		return idx.recordFileError(relPath, info, err)
	}
	return idx.persistFile(fileID, relPath, info, hash, nodes, edges)
}

func (idx *Indexer) isStale(relPath string, info os.FileInfo) (stale bool, fileID int64, err error) {
	var dbMtime int64
	var dbHash string
	row := idx.db.QueryRow(`SELECT id, mtime_ns, content_hash FROM topology_files WHERE path = ?`, relPath)
	if scanErr := row.Scan(&fileID, &dbMtime, &dbHash); scanErr == sql.ErrNoRows {
		return true, 0, nil
	} else if scanErr != nil {
		return false, 0, fmt.Errorf("topology: query file: %w", scanErr)
	}
	return dbMtime != info.ModTime().UnixNano(), fileID, nil
}

func (idx *Indexer) extractPath(ctx context.Context, absPath, relPath string) (nodes []Node, edges []Edge, hash string, err error) {
	ext := strings.ToLower(filepath.Ext(relPath))
	ex := findExtractor(ext, idx.extractors)
	if ex == nil {
		return nil, nil, "", nil
	}
	src, readErr := os.ReadFile(absPath) //nolint:gosec // G304: path derived from workspace root + relative path validated by caller
	if readErr != nil {
		return nil, nil, "", readErr
	}
	h := sha256.Sum256(src)
	hash = fmt.Sprintf("%x", h)
	nodes, edges, err = safeExtract(ctx, ex, relPath, src)
	return nodes, edges, hash, err
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

func (idx *Indexer) processResync(ctx context.Context) error {
	present := make(map[string]bool)
	err := filepath.Walk(idx.workspace, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
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
		return idx.processUpsert(ctx, rel)
	})
	if err != nil {
		return fmt.Errorf("topology: resync walk: %w", err)
	}
	return idx.pruneDeleted(present)
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
	for _, e := range stale {
		tx, txErr := idx.db.Begin()
		if txErr != nil {
			continue
		}
		_ = deleteFileNodes(tx, e.id)
		_, _ = tx.Exec(`DELETE FROM topology_files WHERE id = ?`, e.id)
		_ = tx.Commit()
	}
	return nil
}

// shouldSkipDir returns true for directories that should never be indexed.
func shouldSkipDir(name string) bool {
	switch name {
	case "vendor", "node_modules", ".git", "testdata", ".plumb", "dist", "build", "__pycache__":
		return true
	}
	return false
}
