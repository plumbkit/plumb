package collab

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/plumbkit/plumb/internal/paths"
)

// scope.go is the one place that decides which rows of the daemon-level store a
// reader may see. Every read of that store — the claim, the probe, the preview,
// the wake snapshot, the roster's volume and a thread's participants — resolves a
// readScope here first, and the predicates in mailbox.go and observe.go accept
// nothing else, so a reader cannot reach a global row without passing through it.
//
// The daemon-level store holds two kinds of traffic, and the reader's consent
// separates them:
//
//   - cross-project mail, between unrelated repositories, which a recipient reads
//     only when its own project sets [collab] cross_project;
//   - same-project mail between two checkouts of ONE repository family — a
//     worktree and its main checkout, a submodule and its superproject — which
//     needs no opt-in, because nobody's boundary is being crossed (#635).
//
// The second kind is recognised from origin_git_common_dir: the sender's git
// common directory, which PutNote stamps from the sender's VERIFIED checkout (see
// paths.CheckoutAt). It is re-verified here, against the reader's own checkout, on
// the read side: the sender may be a worktree deleted minutes after sending, so its
// stamp is the only half of the question that survives, and the reader's half is
// whatever is on disk now. Without consent, a reader sees a row only when it was
// aimed at the reader's workspace AND that stamp is one project with it.
//
// Concurrency: the ownership cache is guarded by its own mutex; readScope is a
// value type built per call.

// readScope is a Claimant resolved against one store: the target-workspace term
// every read of that store applies. Only Store.scope makes one, which is what makes
// the scope impossible to skip — the predicates take a readScope, not a Claimant.
type readScope struct {
	who Claimant
	// term is the SQL that limits rows to this reader's workspace, beginning with
	// AND; args are its arguments.
	term string
	args []any
	// none reports that the scope admits no row at all — the global store, read
	// without consent, holding nothing this workspace's project wrote — so the
	// caller can answer without running its query. term is then "AND 0", so a
	// read that forgot to check none still matches nothing.
	none bool
}

// scope resolves who against this store. unreadOnly narrows the stamp lookup to
// undelivered rows, which is all a delivery path can claim; observation paths
// (summaries, threads) count delivered rows too.
func (s *Store) scope(ctx context.Context, who Claimant, now time.Time, unreadOnly bool) (readScope, error) {
	if !s.IsGlobal() || who.CrossProject {
		return workspaceScope(who), nil
	}
	sc := readScope{who: who}
	origins, err := s.ownedOrigins(ctx, who.Workspace, now, unreadOnly)
	if err != nil {
		return sc, err
	}
	if len(origins) == 0 {
		sc.none, sc.term = true, `AND 0`
		return sc, nil
	}
	marks := make([]string, len(origins))
	sc.args = make([]any, 0, 1+len(origins))
	sc.args = append(sc.args, who.Workspace)
	for i, o := range origins {
		marks[i] = "?"
		sc.args = append(sc.args, o)
	}
	sc.term = `AND target_workspace = ? AND origin_git_common_dir IN (` + strings.Join(marks, ", ") + `)`
	return sc, nil
}

// workspaceScope is the scope with no family restriction: every row aimed at the
// reader's workspace. A workspace's own collab.db is scoped by the file it lives
// in, and its rows carry no target; a consenting reader of the global store reads
// every row aimed at its workspace, from any project — the historical rule.
func workspaceScope(who Claimant) readScope {
	return readScope{
		who:  who,
		term: `AND (target_workspace = '' OR target_workspace = ?)`,
		args: []any{who.Workspace},
	}
}

// ownedOrigins returns the stamped common directories of the rows aimed at
// workspace that verify as one project with it. The lookup is a DISTINCT over
// idx_collab_target, so it is bounded by the number of SENDING repositories, not
// by the number of rows — a handful at most.
func (s *Store) ownedOrigins(ctx context.Context, workspace string, now time.Time, unreadOnly bool) ([]string, error) {
	if workspace == "" {
		return nil, nil
	}
	query := `SELECT DISTINCT origin_git_common_dir FROM collab_rows
		 WHERE target_workspace = ? AND origin_git_common_dir != '' AND kind = ? AND expires_at > ?`
	if unreadOnly {
		query += ` AND delivered_at = 0`
	}
	stmt, err := s.probeStmt(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("collab: prepare origin lookup: %w", err)
	}
	rows, err := stmt.QueryContext(ctx, workspace, string(KindNote), now.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("collab: look up origins: %w", err)
	}
	defer rows.Close()
	var stamps []string
	for rows.Next() {
		var stamp string
		if err := rows.Scan(&stamp); err != nil {
			return nil, fmt.Errorf("collab: scan origin: %w", err)
		}
		stamps = append(stamps, stamp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("collab: look up origins: %w", err)
	}
	var owned []string
	for _, stamp := range stamps {
		if s.owns(stamp, workspace, now) {
			owned = append(owned, stamp)
		}
	}
	return owned, nil
}

// ownedRow reports whether a global row is visible to a reader without consent:
// its stamped origin verifies as one project with workspace. It is the per-row
// form of the scope, for a read that filters rows after fetching them (a thread's
// participants), and asks exactly the question ownedOrigins asks.
func (s *Store) ownedRow(r Row, workspace string, now time.Time) bool {
	return r.OriginGitCommonDir != "" && s.owns(r.OriginGitCommonDir, workspace, now)
}

// ownershipTTL is how long a verified answer is reused. The question is asked on
// every tool call's delivery probe and at every turn end, and answering it reads a
// handful of small files; a repository's layout changes on the scale of minutes
// (a worktree added or removed), so a short reuse costs no correctness worth
// having and saves the reads on the path that runs most.
const ownershipTTL = 30 * time.Second

// ownershipCacheMax bounds the cache. Entries are (stamp, workspace) pairs, so a
// daemon serving a few projects holds a few dozen; overflowing clears it rather
// than evicting, because a full clear is correct and the next call refills it.
const ownershipCacheMax = 256

type ownershipKey struct{ stamp, workspace string }

type ownershipAnswer struct {
	same bool
	at   time.Time
}

// ownership caches paths.SameCommonDir answers for one store.
//
// Concurrency: mu guards answers; safe for concurrent use. The zero value is
// ready to use.
type ownership struct {
	mu      sync.Mutex
	answers map[ownershipKey]ownershipAnswer
}

// owns reports whether stamp (a sender's verified common directory) is one
// project with the checkout at workspace, reusing a fresh answer when it has one.
func (s *Store) owns(stamp, workspace string, now time.Time) bool {
	key := ownershipKey{stamp, workspace}
	s.owned.mu.Lock()
	if a, ok := s.owned.answers[key]; ok && now.Sub(a.at) < ownershipTTL && !now.Before(a.at) {
		s.owned.mu.Unlock()
		return a.same
	}
	s.owned.mu.Unlock()

	// Verified outside the lock: it reads the disk, and two callers verifying the
	// same pair at once cost one redundant check, never a wrong answer.
	same := paths.SameCommonDir(stamp, workspace)

	s.owned.mu.Lock()
	defer s.owned.mu.Unlock()
	if s.owned.answers == nil || len(s.owned.answers) >= ownershipCacheMax {
		s.owned.answers = make(map[ownershipKey]ownershipAnswer, 8)
	}
	s.owned.answers[key] = ownershipAnswer{same: same, at: now}
	return same
}

// stampOrigin returns the verified git common directory of the sender's checkout,
// or "" when the sender's workspace is not a checkout this probe can verify. An
// unstamped row is ordinary cross-project mail: only a consenting reader sees it.
func stampOrigin(originWorkspace string) string {
	if originWorkspace == "" {
		return ""
	}
	c, ok := paths.CheckoutAt(originWorkspace)
	if !ok {
		return ""
	}
	return c.CommonDir
}
