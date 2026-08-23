package topology

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/tokenise"
)

// This file holds the indexer's SQLite persistence layer: writing a file's
// extracted nodes and edges into the topology tables within a single
// transaction. These helpers are pure DB operations with no concurrency or
// extraction concerns — see indexer.go for the worker loop, indexer_extract.go
// for extraction, and indexer_resync.go for the full-tree walk.

func (idx *Indexer) persistFile(fileID int64, relPath string, info os.FileInfo, hash, lang string, out extractOutput) error {
	tx, err := idx.db.Begin()
	if err != nil {
		return fmt.Errorf("topology: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit succeeded; on the failure path the error is already being returned

	newFileID, err := upsertFileRecord(tx, fileID, relPath, info, hash, lang)
	if err != nil {
		return err
	}
	if err := deleteFileNodes(tx, newFileID); err != nil {
		return err
	}
	nodeIDs, err := insertNodes(tx, newFileID, relPath, out.nodes)
	if err != nil {
		return err
	}
	if err := insertEdges(tx, nodeIDs, out.edges); err != nil {
		return err
	}
	if err := insertCallSites(tx, newFileID, lang, nodeIDs, out.sites); err != nil {
		return err
	}
	return tx.Commit()
}

// recordFileError stores the failure with the file's mtime but no content
// hash, so the staleness check re-attempts the file on every resync — a
// pathological file whose parse keeps timing out re-pays its full extract
// timeout each cycle (intended: a transient engine fault should be retried).
//
// The conflict clause has to clear content_hash explicitly, not just leave it
// out: on a file that HAD indexed cleanly, the hash from that success survives
// the update, and a re-index triggered by a touch that changed no bytes then
// matches both the refreshed mtime and the stale hash — so one transient
// failure would retire the file from indexing until its content changed.
func (idx *Indexer) recordFileError(relPath string, info os.FileInfo, extractErr error) error {
	_, err := idx.db.Exec(
		`INSERT INTO topology_files(path, mtime_ns, error_msg) VALUES (?, ?, ?)
         ON CONFLICT(path) DO UPDATE SET mtime_ns=excluded.mtime_ns, content_hash='', error_msg=excluded.error_msg`,
		relPath, info.ModTime().UnixNano(), extractErr.Error())
	return err
}

func upsertFileRecord(tx *sql.Tx, fileID int64, relPath string, info os.FileInfo, hash, lang string) (int64, error) {
	if fileID == 0 {
		res, err := tx.Exec(
			`INSERT INTO topology_files(path, language, mtime_ns, content_hash, indexed_at, error_msg)
             VALUES (?, ?, ?, ?, ?, '')`,
			relPath, lang, info.ModTime().UnixNano(), hash, time.Now().UnixNano())
		if err != nil {
			return 0, fmt.Errorf("topology: insert file: %w", err)
		}
		id, _ := res.LastInsertId()
		return id, nil
	}
	_, err := tx.Exec(
		`UPDATE topology_files SET language=?, mtime_ns=?, content_hash=?, indexed_at=?, error_msg='' WHERE id=?`,
		lang, info.ModTime().UnixNano(), hash, time.Now().UnixNano(), fileID)
	if err != nil {
		return 0, fmt.Errorf("topology: update file: %w", err)
	}
	return fileID, nil
}

// deleteFileNodes clears a file's existing rows ahead of a re-index.
// topology_fts is an external-content-free FTS5 table whose rowid is the
// topology_nodes.id assigned in insertNodes, so its rows are removed with a
// single set-based DELETE keyed on that subquery rather than one statement per
// node — this runs on the hot write path (every upsert) and per stale file in
// prune/delete, where a per-node loop costs M FTS5 round-trips for M symbols.
func deleteFileNodes(tx *sql.Tx, fileID int64) error {
	// Call sites go first and by file_id, not by cascade. The reason is that
	// file_id is the key that is always right: a site belongs to the file that was
	// re-parsed whether or not it has an enclosing node, whereas the cascade
	// reaches only sites whose enclosing_id points at a node being deleted, so it
	// is correct only while every site has one.
	//
	// Measured on plumb's own tree, every Go site currently does: a top-level call
	// sits inside a ValueSpec, and extractValueSpec emits a node per declared name
	// (`_` included), so `var _ = mux.HandleFunc(…)` is attributed to that
	// variable's node rather than to NULL — zero rows have a NULL enclosing_id.
	// The schema permits NULL and insertCallSites writes it deliberately, so the
	// cascade is one extractor away from being wrong; deleting by file_id is not.
	if _, err := tx.Exec(`DELETE FROM topology_call_sites WHERE file_id = ?`, fileID); err != nil {
		return fmt.Errorf("topology: delete call sites: %w", err)
	}
	if _, err := tx.Exec(
		`DELETE FROM topology_fts WHERE rowid IN (SELECT id FROM topology_nodes WHERE file_id = ?)`,
		fileID); err != nil {
		return fmt.Errorf("topology: delete fts: %w", err)
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
			`INSERT INTO topology_nodes(file_id, kind, name, qualified, signature, start_line, end_line, docstring, language,
                has_bytes, start_byte, end_byte, start_col, end_col, doc_start_byte, doc_end_byte)
             VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			fileID, string(n.Kind), n.Name, n.Qualified, n.Signature, n.StartLine, n.EndLine, n.Docstring, n.Language,
			boolToInt(n.HasBytes), n.StartByte, n.EndByte, n.StartCol, n.EndCol, n.DocStartByte, n.DocEndByte)
		if err != nil {
			return nil, fmt.Errorf("topology: insert node: %w", err)
		}
		id, _ := res.LastInsertId()
		n.ID = id
		ids = append(ids, id)
		tokens := tokenise.SplitIdentifier(n.Name)
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

// insertCallSites persists a file's raw call sites. The enclosing declaration is
// remapped from an extractor-local node index to a rowid the same way edges are;
// a site with no enclosing declaration is stored with a NULL enclosing_id rather
// than dropped, because a package-level registration call is one of the two
// shapes this table exists to capture.
func insertCallSites(tx *sql.Tx, fileID int64, lang string, nodeIDs []int64, sites []CallSite) error {
	if len(sites) == 0 {
		return nil
	}
	stmt, err := tx.Prepare(
		`INSERT INTO topology_call_sites(file_id, enclosing_id, language, site_kind, callee, qualifier,
            start_byte, start_line, first_string_arg, arg_idents, arg_count, arg_spread)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("topology: prepare call site: %w", err)
	}
	defer stmt.Close()
	for _, s := range sites {
		var enclosing any
		if id := remapNodeID(int64(s.EnclosingIdx), nodeIDs); id != 0 {
			enclosing = id
		}
		var qualifier any
		if s.Qualifier != "" {
			qualifier = s.Qualifier
		}
		var firstString any
		if s.HasStringArg {
			firstString = s.FirstStringArg
		}
		if _, err := stmt.Exec(fileID, enclosing, lang, string(s.Kind), s.Callee, qualifier,
			s.StartByte, s.StartLine, firstString, strings.Join(s.ArgIdents, ","),
			s.ArgCount, boolToInt(s.ArgSpread)); err != nil {
			return fmt.Errorf("topology: insert call site: %w", err)
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

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
