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
	"sync"
	"time"

	_ "modernc.org/sqlite" // register the SQLite driver

	"github.com/plumbkit/plumb/internal/sqlitex"
)

// minTTL is the floor applied to an intent/note TTL. A non-positive or tiny TTL
// (a misconfigured intent_ttl_minutes, or a caller passing 0) would otherwise
// store a row that is already expired and thus never delivered; clamp it so a
// row always lives at least this long.
const minTTL = time.Minute

// Store is the per-workspace collab.db handle.
//
// Concurrency: safe for concurrent use — every method runs a self-contained
// query or a short transaction against the WAL-mode SQLite handle, which
// serialises writers internally. One Store is shared across all connections to a
// workspace (see the cli collabPool).
type Store struct {
	db *sql.DB
	ws string

	// probeMu guards probeStmts, which caches the prepared form of the
	// HasPendingNotes probe. See probeStmt in mailbox.go for why the cache is
	// keyed by SQL text rather than being a single statement, and Close for the
	// lifecycle that keeps it from outliving the store.
	probeMu    sync.Mutex
	probeStmts map[string]*sql.Stmt
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

// Close releases the database handle, closing any cached prepared statements
// first.
//
// The statements must be closed explicitly rather than left to the handle.
// database/sql prepares a *sql.Stmt lazily on EACH pooled connection it is used
// from, so one cached statement holds a driver statement per connection in the
// pool for as long as the Store lives. Dropping the Store without closing them
// is how that becomes a leak rather than an optimisation — the point of caching
// the probe at all (see probeStmt) is bounded reuse, not unbounded retention.
//
// A statement that fails to close is logged and the rest still close: the
// handle's own Close is what actually frees the file, and skipping it because a
// statement complained would trade a small leak for a large one.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	s.probeMu.Lock()
	for query, stmt := range s.probeStmts {
		if err := stmt.Close(); err != nil {
			slog.Warn("collab: close cached probe statement", "err", err, "query", query)
		}
	}
	s.probeStmts = nil
	s.probeMu.Unlock()
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
                          target_workspace, addressee_id)
	 SELECT ?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?, ?`

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
// in.AddresseeID, when set, BINDS the note to that one session: only a claimant
// presenting the same ID may read it. It is the caller's job to set it only for
// a live, unambiguously resolved peer; an empty value keeps the historical
// name-only addressing, which is what an unattached peer and every pre-v3 row
// rely on. AddresseeNext is forced back to unbound here rather than trusted to
// arrive that way, because "whoever attaches next" is a race by definition and a
// bound one would be a race only its winner-in-advance could win.
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
	addrID := strings.TrimSpace(in.AddresseeID)
	if addr == AddresseeNext {
		addrID = ""
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
		in.TargetWorkspace, addrID,
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
	 target_workspace, addressee_id`

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
			&r.TargetWorkspace, &r.AddresseeID); err != nil {
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
