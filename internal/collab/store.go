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

// chatColumns are the schema-v2 additions that turn a one-way note into a
// threaded, read-tracked message. They are applied by ALTER TABLE rather than
// folded into the CREATE above so that an existing v1 collab.db — which is NOT a
// rebuildable index; its rows are the only copy of the data — migrates in place
// instead of being dropped and recreated. Adding a column with a NOT NULL
// DEFAULT backfills every existing row, so a legacy note lands with an empty
// conversation, a zero delivered_at (i.e. unread) and no origin workspace —
// exactly the values a note written before threading existed should carry.
var chatColumns = []struct{ name, ddl string }{
	{"conversation_id", `ALTER TABLE collab_rows ADD COLUMN conversation_id TEXT NOT NULL DEFAULT ''`},
	{"delivered_at", `ALTER TABLE collab_rows ADD COLUMN delivered_at INTEGER NOT NULL DEFAULT 0`},
	{"delivered_to", `ALTER TABLE collab_rows ADD COLUMN delivered_to TEXT NOT NULL DEFAULT ''`},
	{"origin_workspace", `ALTER TABLE collab_rows ADD COLUMN origin_workspace TEXT NOT NULL DEFAULT ''`},
	{"target_workspace", `ALTER TABLE collab_rows ADD COLUMN target_workspace TEXT NOT NULL DEFAULT ''`},
}

// chatIndexes accelerate the two hot chat queries: "what is unread for me" and
// "how many exchanges has this conversation had". Created after the columns
// exist, so they are separate from the base schema above.
const chatIndexes = `
CREATE INDEX IF NOT EXISTS idx_collab_inbox ON collab_rows(addressee, delivered_at, expires_at);
CREATE INDEX IF NOT EXISTS idx_collab_conv  ON collab_rows(conversation_id);
`

// schemaVersion is the current on-disk collab schema version, stamped in PRAGMA
// user_version. Unlike topology.db, collab.db is NOT a rebuildable index — its
// rows are the only copy of expiring advisory data — so a schema change must
// migrate additively rather than dropping the table. v1 was the initial shape;
// v2 adds the chat columns (see chatColumns), applied in place.
const schemaVersion = 2

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

// migrateChatColumns adds any schema-v2 chat column the table is missing. It is
// driven by PRAGMA table_info rather than the stamped version so it is
// idempotent and safe to run on every open, including on a v1 file that a
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

// ErrConversationFull reports that a note was refused because its conversation
// had already spent the exchange budget the caller asked to be held to. Callers
// turn it into their own user-facing refusal; it is a policy outcome, not a
// storage failure.
var ErrConversationFull = errors.New("collab: conversation has reached its exchange limit")

// insertNote writes the row through a SELECT rather than a VALUES list so the
// exchange budget can be appended to the SAME statement as a WHERE clause.
const insertNote = `INSERT INTO collab_rows (kind, author_session, author_id, body, path_globs,
                          addressee, created_at, expires_at, conversation_id, origin_workspace,
                          target_workspace)
	 SELECT ?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?`

// budgetGuard suppresses the insert when the thread has already spent its
// budget. It counts only UNEXPIRED rows, so the budget reads the same whether or
// not the reaper has run — pruning deletes exactly the rows this excludes, and
// counting them would let a prune silently hand an exhausted thread a fresh
// allowance.
const budgetGuard = `
	 WHERE (SELECT COUNT(*) FROM collab_rows
	        WHERE kind = ? AND conversation_id = ? AND expires_at > ?) < ?`

// PutNote stores a note addressed to a peer session name or AddresseeNext and
// returns the conversation it belongs to — the caller's ConversationID when it
// threads onto an existing exchange, otherwise a freshly minted one, which the
// sender quotes to continue the thread. The body is stored verbatim (callers
// redact first). TTL is clamped to minTTL.
//
// A positive in.MaxExchanges caps the conversation, and a note that would exceed
// it is refused with ErrConversationFull. The cap is enforced HERE, in one
// statement, because counting in the caller and then inserting is two steps: two
// agents replying at the same instant both read one-below-the-limit and both
// land, so the budget over-runs precisely when the exchange is running away.
// SQLite's write lock serialises a single statement's count against its insert,
// and a rule kept in the store cannot be forgotten by a future caller. A fresh
// thread is never refused — its live count is zero.
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
	}
	stmt := insertNote
	args := []any{
		string(KindNote), in.AuthorSession, in.AuthorID, in.Body,
		addr, now.UnixNano(), expires.UnixNano(), conv, in.OriginWorkspace,
		in.TargetWorkspace,
	}
	capped := in.MaxExchanges > 0
	if capped {
		stmt += budgetGuard
		args = append(args, string(KindNote), conv, now.UnixNano(), in.MaxExchanges)
	}
	res, err := s.db.ExecContext(ctx, stmt, args...)
	if err != nil {
		return "", fmt.Errorf("collab: insert note: %w", err)
	}
	if capped {
		// The guard is a WHERE clause, so a refusal is an insert that matched
		// nothing rather than an error the driver reports. A RowsAffected error is
		// surfaced rather than ignored: treating "cannot tell" as "accepted" would
		// report a note as sent that may never have been written.
		n, err := res.RowsAffected()
		if err != nil {
			return "", fmt.Errorf("collab: insert note: rows affected: %w", err)
		}
		if n == 0 {
			return "", ErrConversationFull
		}
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
// sessionName, newest first, without claiming them. Used by the listing path
// (workspace_sessions), which reports what is waiting rather than handing it
// over. It never returns "next" notes — those are claimed only by ClaimNotes,
// so listing them would advertise a message the caller may lose the race for.
func (s *Store) PendingNotes(ctx context.Context, sessionName, workspace string, now time.Time) ([]Row, error) {
	if s == nil || s.db == nil || sessionName == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+rowColumns+`
		 FROM collab_rows
		 WHERE kind = ? AND addressee = ? AND delivered_at = 0 AND expires_at > ?
		   AND (target_workspace = '' OR target_workspace = ?)
		 ORDER BY created_at DESC`,
		string(KindNote), sessionName, now.UnixNano(), workspace)
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
// workspace is the caller's pinned workspace root, and is what makes a session
// NAME safe to address by. A row carrying a target_workspace is claimable only
// by a session pinned there; same-project rows carry none and are scoped by the
// database they live in. Without this a session could claim another project's
// cross-project mail just by adopting the right name — names come from a small
// pool with no uniqueness check, and rename_session lets a session pick one.
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
	if s == nil || s.db == nil || sessionName == "" {
		return nil, nil
	}
	// A cross-project row is claimable only by a session pinned to its target;
	// a same-project row carries no target and is scoped by this database.
	scope := `AND (target_workspace = '' OR target_workspace = ?)`
	select_ := `SELECT id FROM collab_rows
			 WHERE kind = ? AND delivered_at = 0 AND expires_at > ?
			   AND (addressee = ? OR addressee = ?) ` + scope + `
			 ORDER BY created_at ASC`
	args := []any{
		now.UnixNano(), sessionName, // SET delivered_at, delivered_to
		string(KindNote), now.UnixNano(), sessionName, AddresseeNext, workspace,
	}
	if limit > 0 {
		select_ += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx,
		`UPDATE collab_rows SET delivered_at = ?, delivered_to = ?
		 WHERE id IN (`+select_+`)
		 RETURNING `+rowColumns, args...)
	if err != nil {
		return nil, fmt.Errorf("collab: claim notes: %w", err)
	}
	defer rows.Close()
	return scanRows(rows)
}

// ConversationPeerWorkspace returns the workspace a named peer was writing from
// within a conversation, so a reply can still be routed correctly after that
// peer's session has gone away. Without it, routing would fall back to "is a
// session with that name live right now", which silently misdelivers a
// cross-project reply into the sender's own mailbox the moment the peer
// disconnects between turns.
func (s *Store) ConversationPeerWorkspace(ctx context.Context, conversationID, peerName string) (string, bool) {
	if s == nil || s.db == nil || conversationID == "" || peerName == "" {
		return "", false
	}
	var ws string
	err := s.db.QueryRowContext(ctx,
		`SELECT origin_workspace FROM collab_rows
		 WHERE kind = ? AND conversation_id = ? AND author_session = ? AND origin_workspace != ''
		 ORDER BY created_at DESC LIMIT 1`,
		string(KindNote), conversationID, peerName).Scan(&ws)
	if err != nil || ws == "" {
		return "", false
	}
	return ws, true
}

// ConversationCount returns how many UNEXPIRED notes a conversation holds.
// Delivered rows count — an exchange that has been read still happened — but
// expired ones do not, matching both the transcript Conversation renders and the
// budget PutNote enforces.
//
// It is purely observational: the exchange budget is enforced inside PutNote's
// guarded insert, not here, because counting and then inserting is two steps that
// two simultaneous senders can interleave. It reads the wall clock rather than
// taking a `now` because nothing depends on its answer being consistent with a
// particular instant.
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
const rowColumns = `id, kind, author_session, author_id, body, path_globs, addressee,
	 created_at, expires_at, conversation_id, delivered_at, delivered_to, origin_workspace,
	 target_workspace`

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
			&globs, &r.Addressee, &createdNs, &expNs,
			&r.ConversationID, &deliveredNs, &r.DeliveredTo, &r.OriginWorkspace,
			&r.TargetWorkspace); err != nil {
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
