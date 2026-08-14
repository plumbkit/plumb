package collab

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // register the SQLite driver

	"github.com/plumbkit/plumb/internal/sqlitex"
)

// minTTL is the floor applied to an intent/note TTL. A non-positive or tiny TTL
// (a misconfigured intent_ttl_minutes, or a caller passing 0) would otherwise
// store a row that is already expired and thus never delivered; clamp it so a
// row always lives at least this long.
const minTTL = time.Minute

const schema = `
CREATE TABLE IF NOT EXISTS collab_rows (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    kind           TEXT    NOT NULL,
    author_session TEXT    NOT NULL DEFAULT '',
    author_id      TEXT    NOT NULL DEFAULT '',
    body           TEXT    NOT NULL DEFAULT '',
    path_globs     TEXT    NOT NULL DEFAULT '',
    addressee      TEXT    NOT NULL DEFAULT '',
    created_at     INTEGER NOT NULL DEFAULT 0,
    expires_at     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_collab_kind    ON collab_rows(kind);
CREATE INDEX IF NOT EXISTS idx_collab_expires ON collab_rows(expires_at);
CREATE INDEX IF NOT EXISTS idx_collab_author  ON collab_rows(author_id);
`

// chatColumns are the additive schema changes that turn a one-way note into a
// threaded, read-tracked message. They are applied by ALTER TABLE rather than
// folded into the CREATE above so that an existing v1 collab.db — which is NOT a
// rebuildable index; its rows are the only copy of the data — migrates in place
// instead of being dropped and recreated. Adding a column with a NOT NULL
// DEFAULT backfills every existing row, so a legacy note lands with an empty
// conversation, a zero delivered_at (i.e. unread), no workspace, and an
// original_bytes value of zero — exactly the values a note written before
// threading or byte accounting existed should carry.
var chatColumns = []struct{ name, ddl string }{
	{"conversation_id", `ALTER TABLE collab_rows ADD COLUMN conversation_id TEXT NOT NULL DEFAULT ''`},
	{"delivered_at", `ALTER TABLE collab_rows ADD COLUMN delivered_at INTEGER NOT NULL DEFAULT 0`},
	{"delivered_to", `ALTER TABLE collab_rows ADD COLUMN delivered_to TEXT NOT NULL DEFAULT ''`},
	{"delivered_to_id", `ALTER TABLE collab_rows ADD COLUMN delivered_to_id TEXT NOT NULL DEFAULT ''`},
	{"origin_workspace", `ALTER TABLE collab_rows ADD COLUMN origin_workspace TEXT NOT NULL DEFAULT ''`},
	{"target_workspace", `ALTER TABLE collab_rows ADD COLUMN target_workspace TEXT NOT NULL DEFAULT ''`},
	{"original_bytes", `ALTER TABLE collab_rows ADD COLUMN original_bytes INTEGER NOT NULL DEFAULT 0`},
	{"target_id", `ALTER TABLE collab_rows ADD COLUMN target_id TEXT NOT NULL DEFAULT ''`},
}

// chatIndexes accelerate the two hot chat queries: unread mail and conversation
// volume. Created after the columns exist, so they are separate from the base
// schema above.
const chatIndexes = `
CREATE INDEX IF NOT EXISTS idx_collab_inbox  ON collab_rows(addressee, delivered_at, expires_at);
CREATE INDEX IF NOT EXISTS idx_collab_target ON collab_rows(target_id, delivered_at, expires_at);
CREATE INDEX IF NOT EXISTS idx_collab_conv   ON collab_rows(conversation_id);
`

// schemaVersion is the current on-disk collab schema version, stamped in PRAGMA
// user_version. Unlike topology.db, collab.db is NOT a rebuildable index — its
// rows are the only copy of expiring advisory data — so a schema change must
// migrate additively rather than dropping the table. v1 was the initial shape;
// v2 adds threaded delivery; v3 records the claiming recipient identity and a
// note's pre-window byte count; v4 records the intended stable recipient so a
// rename or name reuse between send and claim cannot retarget queued mail.
const schemaVersion = 4

// Store is the per-workspace collab.db handle.
//
// Concurrency: safe for concurrent use — every method runs a self-contained
// query or a short transaction against the WAL-mode SQLite handle, which
// serialises writers internally. One Store is shared across all connections to a
// workspace (see the cli collabPool).
type Store struct {
	db *sql.DB
	ws string
}

// DBPath returns the canonical collab.db path for a workspace.
func DBPath(workspace string) string {
	return filepath.Join(workspace, ".plumb", "collab.db")
}

// Exists reports whether a collab.db already exists for the workspace, without
// creating one. Read and prune paths call this first so they never materialise a
// collab.db for a workspace that has never used an intents/mailbox feature.
func Exists(workspace string) bool {
	if workspace == "" {
		return false
	}
	_, err := os.Stat(DBPath(workspace))
	return err == nil
}

// Open opens or creates collab.db for the workspace and applies the schema. The
// enclosing .plumb/ directory and a .gitignore entry are created as needed. Only
// the write path (share_intent / leave_note) should Open unconditionally; read
// and prune paths guard with Exists first.
func Open(workspace string) (*Store, error) {
	if workspace == "" {
		return nil, errors.New("collab: empty workspace")
	}
	path := DBPath(workspace)
	// Open first: sqlitex creates the .plumb/ directory, which ensureGitignore
	// then writes into.
	db, err := sqlitex.Open(path, sqlitex.Options{})
	if err != nil {
		return nil, fmt.Errorf("collab: open db: %w", err)
	}
	if err := ensureGitignore(filepath.Dir(path)); err != nil {
		slog.Warn("collab: ensure .gitignore", "dir", filepath.Dir(path), "err", err)
	}
	if err := initDB(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, ws: workspace}, nil
}

func initDB(db *sql.DB) error {
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("collab: apply schema: %w", err)
	}
	if err := migrateChatColumns(db); err != nil {
		return err
	}
	if _, err := db.Exec(chatIndexes); err != nil {
		return fmt.Errorf("collab: apply chat indexes: %w", err)
	}
	// Stamped unconditionally: it records what wrote the file. The migration above
	// is driven by which columns actually exist rather than by this stamp, so a
	// file written by a future plumb and reopened by an older one cannot be
	// mis-migrated on the strength of a version number alone.
	return sqlitex.StampVersion(db, schemaVersion)
}

// migrateChatColumns adds any chat column through schema v3 that the table is
// missing. It is driven by PRAGMA table_info rather than the stamped version so
// it is idempotent and safe to run on every open, including on a v1 file that a
// crashed migration left half-way.
func migrateChatColumns(db *sql.DB) error {
	have, err := collabRowsColumns(db)
	if err != nil {
		return err
	}
	for _, c := range chatColumns {
		if have[c.name] {
			continue
		}
		if _, err := db.Exec(c.ddl); err != nil {
			// SQLite has no ADD COLUMN IF NOT EXISTS, so the inspection above and
			// this ALTER are two steps another process opening the same collab.db can
			// slip between — the loser gets "duplicate column name" and, without this,
			// its whole Open fails on work that has in fact been done. Re-inspect
			// rather than matching the driver's message text: if the column is present
			// now, a peer added it and there is nothing left to do; if it is still
			// absent, the failure is genuine and propagates.
			if after, qErr := collabRowsColumns(db); qErr == nil && after[c.name] {
				continue
			}
			return fmt.Errorf("collab: add column %s: %w", c.name, err)
		}
	}
	return nil
}

// collabRowsColumns returns the set of column names currently on collab_rows.
// It drives the migration, which must be decided by what the table actually has
// rather than by the stamped schema version.
func collabRowsColumns(db *sql.DB) (map[string]bool, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info('collab_rows')`)
	if err != nil {
		return nil, fmt.Errorf("collab: inspect collab_rows: %w", err)
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("collab: inspect collab_rows: %w", err)
		}
		out[name] = true
	}
	return out, rows.Err()
}

// Close releases the database handle.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Workspace returns the workspace root this store serves.
func (s *Store) Workspace() string { return s.ws }

// PutIntent replaces the author session's live intent with a new one — one live
// intent per session keeps the model self-cleaning. The body is stored verbatim
// (callers redact before persisting). TTL is clamped to at least minTTL.
func (s *Store) PutIntent(ctx context.Context, in IntentInput, now time.Time) error {
	if s == nil || s.db == nil {
		return errors.New("collab: nil store")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("collab: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM collab_rows WHERE kind = ? AND author_id = ?`,
		string(KindIntent), in.AuthorID); err != nil {
		return fmt.Errorf("collab: clear prior intent: %w", err)
	}
	expires := now.Add(clampTTL(in.TTL))
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO collab_rows (kind, author_session, author_id, body, path_globs, addressee, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, '', ?, ?)`,
		string(KindIntent), in.AuthorSession, in.AuthorID, in.Body,
		joinGlobs(in.PathGlobs), now.UnixNano(), expires.UnixNano()); err != nil {
		return fmt.Errorf("collab: insert intent: %w", err)
	}
	return tx.Commit()
}

// insertNote is the single write path for notes. Message-count limits deliberately
// do not exist: a conversation is observable, but never severed by the store.
const insertNote = `INSERT INTO collab_rows (kind, author_session, author_id, body, path_globs,
                          addressee, target_id, created_at, expires_at, conversation_id,
                          origin_workspace, target_workspace, original_bytes)
	 VALUES (?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?, ?, ?)`

// PutNote stores a note addressed to a peer session name or AddresseeNext and
// returns the conversation it belongs to — the caller's ConversationID when it
// threads onto an existing exchange, otherwise a freshly minted one, which the
// sender quotes to continue the thread. Callers redact and apply the configured
// byte window before storage; OriginalBytes preserves the exact redacted size.
// TTL is clamped to minTTL.
func (s *Store) PutNote(ctx context.Context, in NoteInput, now time.Time) (string, error) {
	if s == nil || s.db == nil {
		return "", errors.New("collab: nil store")
	}
	addr := strings.TrimSpace(in.Addressee)
	if addr == "" {
		addr = AddresseeNext
	}
	conv := strings.TrimSpace(in.ConversationID)
	if conv == "" {
		conv = newConversationID()
	}
	expires := now.Add(clampTTL(in.TTL))
	if in.OriginalBytes <= 0 {
		in.OriginalBytes = len(in.Body)
	}
	if s.IsGlobal() {
		// The daemon-level store is shared by every project on the machine, so a
		// row there must name the workspace allowed to claim it. Refusing here
		// rather than defaulting keeps the addressing invariant a property of the
		// store instead of a convention its callers are trusted to follow.
		if in.TargetWorkspace == "" {
			return "", errors.New("collab: a cross-project note requires a target workspace")
		}
		if addr == AddresseeNext {
			return "", errors.New(`collab: "next" has no meaning across projects`)
		}
		if strings.TrimSpace(in.TargetID) == "" {
			return "", errors.New("collab: a cross-project note requires a stable target session ID")
		}
	}
	if _, err := s.db.ExecContext(ctx, insertNote,
		string(KindNote), in.AuthorSession, in.AuthorID, in.Body,
		addr, strings.TrimSpace(in.TargetID), now.UnixNano(), expires.UnixNano(), conv,
		in.OriginWorkspace, in.TargetWorkspace, in.OriginalBytes); err != nil {
		return "", fmt.Errorf("collab: insert note: %w", err)
	}
	return conv, nil
}

// newConversationID mints an opaque thread identifier. Short enough to quote
// back in a tool argument without noise, wide enough that two conversations
// started in the same instant across projects cannot collide in practice.
func newConversationID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is not a reason to lose the message; fall back to a
		// clock-derived id, which is unique enough for an expiring advisory row.
		return fmt.Sprintf("c%013x", time.Now().UnixNano())
	}
	return "c" + hex.EncodeToString(b[:])
}

// LiveIntents returns every unexpired intent, newest first. Expired rows are
// filtered here regardless of pruning, so a missed prune never surfaces a stale
// intent.
func (s *Store) LiveIntents(ctx context.Context, now time.Time) ([]Row, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+rowColumns+`
		 FROM collab_rows WHERE kind = ? AND expires_at > ? ORDER BY created_at DESC`,
		string(KindIntent), now.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("collab: query intents: %w", err)
	}
	defer rows.Close()
	return scanRows(rows)
}

// PendingNotes returns the unexpired, NOT YET DELIVERED notes addressed to
// sessionName, newest first, without claiming them. It preserves the historical
// name-only API for legacy or explicitly unplaced rows; stable-target rows are
// visible only through PendingNotesForSession.
func (s *Store) PendingNotes(ctx context.Context, sessionName, workspace string, now time.Time) ([]Row, error) {
	return s.PendingNotesForSession(ctx, sessionName, "", workspace, now)
}

// PendingNotesForSession is PendingNotes with the recipient's stable session ID.
// A session never sees a note it authored, including one addressed to "next".
func (s *Store) PendingNotesForSession(
	ctx context.Context,
	sessionName, sessionID, workspace string,
	now time.Time,
) ([]Row, error) {
	if s == nil || s.db == nil || sessionName == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+rowColumns+`
		 FROM collab_rows
		 WHERE kind = ? AND delivered_at = 0 AND expires_at > ?
		   AND ((target_id <> '' AND target_id = ?) OR (target_id = '' AND addressee = ?))
		   AND (target_workspace = '' OR target_workspace = ?)
		   AND (? = '' OR author_id <> ?)
		 ORDER BY created_at DESC, id DESC`,
		string(KindNote), now.UnixNano(), sessionID, sessionName, workspace, sessionID, sessionID)
	if err != nil {
		return nil, fmt.Errorf("collab: query notes: %w", err)
	}
	defer rows.Close()
	return scanRows(rows)
}

// ClaimNotes hands the caller every unexpired note addressed to sessionName or
// to "next" that nobody has claimed yet, OLDEST FIRST so a conversation reads in
// order, and marks each one delivered to sessionName in the same statement.
//
// Claiming replaced the original delete-on-delivery: a delivered row stays until
// its TTL, which is what gives a conversation a readable transcript and a
// countable number of exchanges. The read watermark — not the row's existence —
// is what stops a message being handed over twice. Because the claim is a single
// atomic UPDATE matching only `delivered_at = 0`, two sessions racing for the
// same "next" note cannot both win: the second one's UPDATE matches no rows.
//
// Targeted rows are claimed by stable session ID, independent of the caller's
// current display name. Legacy, `next`, and explicitly unplaced rows retain the
// name path. workspace is the caller's pinned root: a cross-project row is
// claimable only from its target workspace, while same-project rows are scoped by
// the database they live in.
//
// limit caps how many are claimed (non-positive means no cap). The cap is
// applied by the STATEMENT, not by the caller trimming the result: a claimed row
// is marked delivered and will never be offered again, so anything the caller
// then dropped would be silently lost. Over-limit messages stay unclaimed.
//
// It is deliberately ONE `UPDATE … RETURNING`, not a transaction that reads and
// then writes. In WAL mode a DEFERRED transaction takes a read snapshot on its
// first SELECT and cannot upgrade it to a write if another connection has
// committed meanwhile: SQLite fails it with SQLITE_BUSY_SNAPSHOT, which
// busy_timeout deliberately does NOT retry. Delivery is exactly the path where
// several sessions wake at once (one send bumps every watcher), so that shape
// failed on essentially every concurrent burst. A single UPDATE takes the write
// lock up front, so contention is handled by busy_timeout as normal.
func (s *Store) ClaimNotes(ctx context.Context, sessionName, workspace string, now time.Time, limit int) ([]Row, error) {
	return s.ClaimNotesForSession(ctx, sessionName, "", workspace, now, limit)
}

// ClaimNotesForSession is ClaimNotes with the recipient's stable session ID.
// The author exclusion is part of the atomic UPDATE, so a self-addressed note is
// left pending for another eligible peer rather than consumed by its sender.
func (s *Store) ClaimNotesForSession(
	ctx context.Context,
	sessionName, sessionID, workspace string,
	now time.Time,
	limit int,
) ([]Row, error) {
	if s == nil || s.db == nil || sessionName == "" {
		return nil, nil
	}
	// A cross-project row is claimable only by a session pinned to its target;
	// a same-project row carries no target and is scoped by this database.
	select_ := `SELECT id FROM collab_rows
			 WHERE kind = ? AND delivered_at = 0 AND expires_at > ?
			   AND ((target_id <> '' AND target_id = ?) OR
			        (target_id = '' AND (addressee = ? OR addressee = ?)))
			   AND (target_workspace = '' OR target_workspace = ?)
			   AND (? = '' OR author_id <> ?)
			 ORDER BY created_at ASC, id ASC`
	args := []any{
		now.UnixNano(), sessionName, sessionID, // SET delivered_at, delivered_to, delivered_to_id
		string(KindNote), now.UnixNano(), sessionID, sessionName, AddresseeNext, workspace,
		sessionID, sessionID,
	}
	if limit > 0 {
		select_ += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx,
		`UPDATE collab_rows SET delivered_at = ?, delivered_to = ?, delivered_to_id = ?
		 WHERE id IN (`+select_+`)
		 RETURNING `+rowColumns, args...)
	if err != nil {
		return nil, fmt.Errorf("collab: claim notes: %w", err)
	}
	defer rows.Close()
	claimed, err := scanRows(rows)
	if err != nil {
		return nil, err
	}
	// SQLite does not promise UPDATE ... RETURNING order. Re-establish the API's
	// oldest-first contract after the atomic claim, with id as the deterministic
	// tie-break for notes written at the same timestamp.
	sort.Slice(claimed, func(i, j int) bool {
		if claimed[i].CreatedAt.Equal(claimed[j].CreatedAt) {
			return claimed[i].ID < claimed[j].ID
		}
		return claimed[i].CreatedAt.Before(claimed[j].CreatedAt)
	})
	return claimed, nil
}

// ConversationCount returns how many UNEXPIRED notes a conversation holds.
// Delivered rows count — an exchange that has been read still happened — but
// expired ones do not. It is purely observational: note volume is surfaced to
// humans, never used to sever a conversation.
func (s *Store) ConversationCount(ctx context.Context, conversationID string) (int, error) {
	if s == nil || s.db == nil || conversationID == "" {
		return 0, nil
	}
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM collab_rows
		 WHERE kind = ? AND conversation_id = ? AND expires_at > ?`,
		string(KindNote), conversationID, time.Now().UnixNano()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("collab: count conversation: %w", err)
	}
	return n, nil
}

// Conversation returns a thread's unexpired notes oldest first, for rendering a
// transcript. Delivered rows are included — that is the point of a transcript.
func (s *Store) Conversation(ctx context.Context, conversationID string, now time.Time) ([]Row, error) {
	if s == nil || s.db == nil || conversationID == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+rowColumns+`
		 FROM collab_rows
		 WHERE kind = ? AND conversation_id = ? AND expires_at > ?
		 ORDER BY created_at ASC`,
		string(KindNote), conversationID, now.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("collab: query conversation: %w", err)
	}
	defer rows.Close()
	return scanRows(rows)
}

// ClearSessionIntents removes every intent authored by authorID — an intent must
// not outlive its session, so the daemon calls this when the connection closes
// or is evicted. Notes are left untouched (they survive their author).
func (s *Store) ClearSessionIntents(ctx context.Context, authorID string) error {
	if s == nil || s.db == nil || authorID == "" {
		return nil
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM collab_rows WHERE kind = ? AND author_id = ?`,
		string(KindIntent), authorID); err != nil {
		return fmt.Errorf("collab: clear session intents: %w", err)
	}
	return nil
}

// Prune deletes every row past its expiry and returns how many were removed. Run
// on the daemon session-reaper tick; reads filter expired rows regardless, so
// pruning is a space reclaim, not a correctness requirement.
func (s *Store) Prune(ctx context.Context, now time.Time) (int, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM collab_rows WHERE expires_at <= ?`, now.UnixNano())
	if err != nil {
		return 0, fmt.Errorf("collab: prune: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// rowColumns is the SELECT list every row query shares, in the order scanRows
// expects. Kept in one place so adding a column cannot desynchronise a query
// from the scanner.
const rowColumns = `id, kind, author_session, author_id, body, path_globs, addressee, target_id,
	 created_at, expires_at, conversation_id, delivered_at, delivered_to, delivered_to_id,
	 origin_workspace, target_workspace, original_bytes`

func scanRows(rows *sql.Rows) ([]Row, error) {
	var out []Row
	for rows.Next() {
		var (
			r                Row
			kind, globs      string
			createdNs, expNs int64
			deliveredNs      int64
		)
		if err := rows.Scan(&r.ID, &kind, &r.AuthorSession, &r.AuthorID, &r.Body,
			&globs, &r.Addressee, &r.TargetID, &createdNs, &expNs,
			&r.ConversationID, &deliveredNs, &r.DeliveredTo, &r.DeliveredToID,
			&r.OriginWorkspace, &r.TargetWorkspace, &r.OriginalBytes); err != nil {
			return nil, fmt.Errorf("collab: scan: %w", err)
		}
		r.Kind = Kind(kind)
		r.PathGlobs = splitGlobs(globs)
		r.CreatedAt = time.Unix(0, createdNs)
		r.ExpiresAt = time.Unix(0, expNs)
		if deliveredNs != 0 {
			r.DeliveredAt = time.Unix(0, deliveredNs)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func clampTTL(ttl time.Duration) time.Duration {
	if ttl < minTTL {
		return minTTL
	}
	return ttl
}

// joinGlobs / splitGlobs serialise the path-glob slice. A newline separator is
// safe because a glob never contains one; empty globs collapse to "".
func joinGlobs(globs []string) string {
	var kept []string
	for _, g := range globs {
		if g = strings.TrimSpace(g); g != "" {
			kept = append(kept, g)
		}
	}
	return strings.Join(kept, "\n")
}

func splitGlobs(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
