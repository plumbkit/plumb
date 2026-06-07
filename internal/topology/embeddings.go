package topology

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
)

// EmbedDoc is the text embedded for a node — name, signature, and docstring,
// the fields that carry meaning for semantic search. Capped so a giant
// docstring cannot blow the embedder's per-input token limit.
func EmbedDoc(n Node) string {
	parts := []string{n.Name}
	if n.Signature != "" {
		parts = append(parts, n.Signature)
	}
	if n.Docstring != "" {
		parts = append(parts, n.Docstring)
	}
	d := strings.Join(parts, " | ")
	if len(d) > 2000 {
		d = d[:2000]
	}
	return d
}

// ContentHash hashes an embed doc; with the model name it is the cache key for a
// vector. Content-keyed (not node-id-keyed) so the cache survives re-indexing.
func ContentHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:16])
}

// GetEmbeddings returns the cached vectors for the given content hashes under
// model, keyed by hash. Missing hashes are simply absent from the result.
func (s *Store) GetEmbeddings(ctx context.Context, model string, hashes []string) (map[string][]float32, error) {
	out := make(map[string][]float32, len(hashes))
	if len(hashes) == 0 {
		return out, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(hashes)), ",")
	args := make([]any, 0, len(hashes)+1)
	args = append(args, model)
	for _, h := range hashes {
		args = append(args, h)
	}
	//nolint:gosec // G202: only the "?,?,…" placeholder list is concatenated; values are bound.
	query := `SELECT content_hash, vector FROM topology_embeddings WHERE model = ? AND content_hash IN (` + ph + `)`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("topology: get embeddings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var h string
		var blob []byte
		if err := rows.Scan(&h, &blob); err != nil {
			continue
		}
		out[h] = decodeVec(blob)
	}
	return out, rows.Err()
}

// PutEmbeddings upserts vectors for model, keyed by content hash, in one
// transaction.
func (s *Store) PutEmbeddings(ctx context.Context, model string, byHash map[string][]float32) error {
	if len(byHash) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("topology: put embeddings: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT OR REPLACE INTO topology_embeddings(model, content_hash, dim, vector) VALUES (?,?,?,?)`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("topology: prepare put embeddings: %w", err)
	}
	defer stmt.Close()
	for h, v := range byHash {
		if len(v) == 0 {
			continue
		}
		if _, err := stmt.ExecContext(ctx, model, h, len(v), encodeVec(v)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("topology: put embedding: %w", err)
		}
	}
	return tx.Commit()
}

// encodeVec packs a float32 vector as little-endian bytes.
func encodeVec(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

// decodeVec unpacks little-endian bytes into a float32 vector.
func decodeVec(b []byte) []float32 {
	n := len(b) / 4
	v := make([]float32, n)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}
