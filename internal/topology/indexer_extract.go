package topology

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/plumbkit/plumb/internal/langsupport"
)

// This file holds the indexer's per-file path: turning one changed file into
// nodes and edges (read, hash, per-grammar cap, extract) and the staleness
// check that decides whether a re-index is needed. See indexer.go for the
// worker loop, indexer_persist.go for the DB writes, and indexer_resync.go for
// the full-tree walk.

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
// no extractor matches it returns a nil extractor, empty language and empty hash —
// preserving the prior behaviour where such a file is recorded with zero symbols so
// the staleness check never re-attempts it.
func (idx *Indexer) readAndHash(absPath, relPath string) (src []byte, ex Extractor, lang, hash string, err error) {
	ex = findExtractor(relPath, idx.extractors)
	if ex == nil {
		return nil, nil, "", "", nil
	}
	src, err = os.ReadFile(absPath) //nolint:gosec // G304: path derived from workspace root + relative path validated by caller
	if err != nil {
		return nil, nil, "", "", err
	}
	h := sha256.Sum256(src)
	return src, ex, ex.Language(), fmt.Sprintf("%x", h), nil
}

// extractFile runs the extractor for a file that isStale has confirmed needs
// re-indexing. A nil extractor (no language match) or an oversized GLR grammar
// yields zero nodes, matching the records persisted by the pre-reorder path.
func (idx *Indexer) extractFile(ctx context.Context, ex Extractor, relPath string, src []byte) (nodes []Node, edges []Edge, err error) {
	if ex == nil {
		return nil, nil, nil
	}
	if skipOversizedGrammar(relPath, ex.Language(), len(src)) {
		return nil, nil, nil
	}
	return safeExtract(ctx, ex, relPath, src)
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
