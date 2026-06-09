package memory

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Upsert indexes one memory, replacing any existing row for the same name. The
// file write is the source of truth, so callers treat an Upsert error as
// non-fatal (the next Reindex repairs it). created_at and last_used_at are
// preserved across a re-index of an existing memory.
func (ix *Index) Upsert(rec Record) error {
	ix.mu.Lock()
	defer ix.mu.Unlock()

	tx, err := ix.db.Begin()
	if err != nil {
		return fmt.Errorf("memory: begin upsert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	createdNS, lastUsedNS := ix.priorTimes(tx, rec)
	if err := deleteByName(tx, rec.Name); err != nil {
		return err
	}
	fileID, err := upsertFile(tx, rec)
	if err != nil {
		return err
	}
	id, err := insertRecord(tx, fileID, rec, createdNS, lastUsedNS)
	if err != nil {
		return err
	}
	if err := insertFTS(tx, id, rec); err != nil {
		return err
	}
	return tx.Commit()
}

// priorTimes returns the existing created_at / last_used_at for rec.Name so a
// re-index preserves them. A zero rec.CreatedAt falls back to the stored value,
// then to now.
func (ix *Index) priorTimes(tx *sql.Tx, rec Record) (createdNS, lastUsedNS int64) {
	_ = tx.QueryRow(`SELECT created_at, last_used_at FROM memory_records WHERE name = ?`, rec.Name).
		Scan(&createdNS, &lastUsedNS)
	if !rec.CreatedAt.IsZero() {
		createdNS = rec.CreatedAt.UnixNano()
	}
	if createdNS == 0 {
		createdNS = time.Now().UnixNano()
	}
	return createdNS, lastUsedNS
}

func deleteByName(tx *sql.Tx, name string) error {
	var id int64
	err := tx.QueryRow(`SELECT id FROM memory_records WHERE name = ?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("memory: lookup existing %q: %w", name, err)
	}
	if _, err := tx.Exec(`DELETE FROM memory_fts WHERE rowid = ?`, id); err != nil {
		return fmt.Errorf("memory: delete fts %q: %w", name, err)
	}
	if _, err := tx.Exec(`DELETE FROM memory_records WHERE id = ?`, id); err != nil {
		return fmt.Errorf("memory: delete record %q: %w", name, err)
	}
	return nil
}

func upsertFile(tx *sql.Tx, rec Record) (int64, error) {
	if _, err := tx.Exec(`
		INSERT INTO memory_files(name, content_sha, mtime_ns, size_bytes, indexed_at)
		VALUES (?,?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET
			content_sha = excluded.content_sha,
			mtime_ns    = excluded.mtime_ns,
			size_bytes  = excluded.size_bytes,
			indexed_at  = excluded.indexed_at`,
		rec.Name, rec.ContentSHA, rec.MTimeNS, rec.SizeBytes, time.Now().UnixNano()); err != nil {
		return 0, fmt.Errorf("memory: upsert file %q: %w", rec.Name, err)
	}
	var fileID int64
	if err := tx.QueryRow(`SELECT id FROM memory_files WHERE name = ?`, rec.Name).Scan(&fileID); err != nil {
		return 0, fmt.Errorf("memory: file id %q: %w", rec.Name, err)
	}
	return fileID, nil
}

func insertRecord(tx *sql.Tx, fileID int64, rec Record, createdNS, lastUsedNS int64) (int64, error) {
	conf := rec.Confidence
	if conf == "" {
		conf = ConfidenceUser
	}
	res, err := tx.Exec(`
		INSERT INTO memory_records(
			file_id, name, description, paths_json, source_paths, source_symbols,
			source_session, source_calls, confidence, content_sha,
			created_at, updated_at, last_used_at, stale_after)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		fileID, rec.Name, rec.Description, jsonArray(rec.Paths), jsonArray(rec.SourcePaths),
		jsonArray(rec.SourceSymbols), rec.SourceSession, jsonArray(rec.SourceCalls),
		string(conf), rec.ContentSHA, createdNS, time.Now().UnixNano(), lastUsedNS, nanosOrZero(rec.StaleAfter))
	if err != nil {
		return 0, fmt.Errorf("memory: insert record %q: %w", rec.Name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("memory: record id %q: %w", rec.Name, err)
	}
	return id, nil
}

func insertFTS(tx *sql.Tx, id int64, rec Record) error {
	provenance := string(rec.Confidence)
	if rec.SourceSession != "" {
		provenance += " " + rec.SourceSession
	}
	if _, err := tx.Exec(`
		INSERT INTO memory_fts(rowid, name, name_tokens, description, body,
			path_globs, source_paths, source_symbols, provenance)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		id, rec.Name, splitIdentifier(rec.Name), rec.Description, rec.Body,
		strings.Join(rec.Paths, " "), strings.Join(rec.SourcePaths, " "),
		strings.Join(rec.SourceSymbols, " "), provenance); err != nil {
		return fmt.Errorf("memory: insert fts %q: %w", rec.Name, err)
	}
	return nil
}

// Remove deletes a memory's index rows. A missing memory is a no-op.
func (ix *Index) Remove(name string) error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	tx, err := ix.db.Begin()
	if err != nil {
		return fmt.Errorf("memory: begin remove: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := deleteByName(tx, name); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM memory_files WHERE name = ?`, name); err != nil {
		return fmt.Errorf("memory: delete file %q: %w", name, err)
	}
	return tx.Commit()
}

// TouchUsed bumps a memory's last_used_at so recency nudges its search ranking.
// Best-effort: a missing memory is a no-op.
func (ix *Index) TouchUsed(name string) error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	_, err := ix.db.Exec(`UPDATE memory_records SET last_used_at = ? WHERE name = ?`,
		time.Now().UnixNano(), name)
	if err != nil {
		return fmt.Errorf("memory: touch %q: %w", name, err)
	}
	return nil
}

func jsonArray(ss []string) string {
	if len(ss) == 0 {
		return "[]"
	}
	b, err := json.Marshal(ss)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func nanosOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// applyProvenanceFrontmatter fills the provenance/lifecycle fields of rec from a
// generated memory's frontmatter. A plain user memory has none of these keys and
// keeps confidence=user. Tolerant: unrecognised or malformed values are ignored.
func applyProvenanceFrontmatter(rec *Record, fm []byte) {
	for line := range strings.SplitSeq(string(fm), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		switch k {
		case "confidence":
			if v != "" {
				rec.Confidence = Confidence(v)
			}
		case "source_session":
			rec.SourceSession = v
		case "source_paths":
			rec.SourcePaths = parseList(v)
		case "source_symbols":
			rec.SourceSymbols = parseList(v)
		case "source_calls":
			rec.SourceCalls = parseList(v)
		case "created_at":
			if ts, err := time.Parse(time.RFC3339, v); err == nil {
				rec.CreatedAt = ts
			}
		case "stale_after":
			if ts, err := time.Parse(time.RFC3339, v); err == nil {
				rec.StaleAfter = ts
			}
		}
	}
}
