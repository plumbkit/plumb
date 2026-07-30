package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/session"
)

// newRefreshSession builds an unattached connection over the enable-lsp test
// pool (go active, html configured-but-disabled), so a workspace whose only
// language is html attaches as LanguageNone until `enable-lsp html` runs.
func newRefreshSession(t *testing.T, pool *workspacePool) *connSession {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	s := newConnSession(context.Background(), pool, nil, config.NewStore(config.Defaults()), nil, nil, newSharedBudgets())
	t.Cleanup(s.close)
	return s
}

// TestRefreshPrimary_ResolvesAfterEnableLSP is the PLAN-258 regression pin: a
// session that attached while its language was inactive reported "no language
// server attached" for the whole life of the connection, even after
// `plumb enable-lsp` made one available and per-file routing began serving it.
// One tool call after the enable must now heal the primary — no daemon restart,
// no workspace switch, no explicit session_start({language: …}).
func TestRefreshPrimary_ResolvesAfterEnableLSP(t *testing.T) {
	pool := enableTestPool()
	root := freshTempDir(t)
	// A declared plumb workspace whose only language marker is html: Detect
	// resolves the root but names no language while html is disabled.
	mustWrite(t, filepath.Join(root, ".plumb", "config.toml"), "")
	mustWrite(t, filepath.Join(root, "index.html"), "<html></html>\n")
	installEntryLang(pool, root, "html", &stubClient{id: "html"})

	s := newRefreshSession(t, pool)
	s.attachWorkspace(context.Background(), "file://"+root)

	if got := s.workspace(); got != root {
		t.Fatalf("attached workspace = %q, want %q", got, root)
	}
	if got := s.acquiredLanguageName(); got != "" {
		t.Fatalf("precondition: language %q attached before enable-lsp, want none", got)
	}

	if already, err := pool.enableLanguage("html"); err != nil || already {
		t.Fatalf("enableLanguage(html) = (already=%v, err=%v), want (false, nil)", already, err)
	}

	// Any tool call is enough — the agent must not have to know to re-orient.
	s.onBeforeTool(context.Background(), "read_file", json.RawMessage(`{}`))

	if got := s.acquiredLanguageName(); got != "html" {
		t.Fatalf("language after enable-lsp + one tool call = %q, want html", got)
	}
	if got := s.workspace(); got != root {
		t.Errorf("the refresh moved the pinned workspace to %q; it must only resolve the language", got)
	}
	info := sessionRecord(t, s.sessID)
	if info.Language != "html" {
		t.Errorf("session record Language = %q, want html", info.Language)
	}
	if want := "vscode-html-language-server"; info.Adapter != want {
		t.Errorf("session record Adapter = %q, want %q", info.Adapter, want)
	}
}

// sessionRecord returns the persisted session record for id — what the TUI and
// workspace_sessions read.
func sessionRecord(t *testing.T, id string) session.Info {
	t.Helper()
	infos, err := session.List()
	if err != nil {
		t.Fatalf("session.List: %v", err)
	}
	for _, in := range infos {
		if in.ID == id {
			return in
		}
	}
	t.Fatalf("no session record for %s", id)
	return session.Info{}
}

// TestRefreshPrimary_PreservesSessionState guards the line between this refresh
// and the re-pin path it deliberately does not reuse: repinWorkspaceFrom resets
// the read/write/undo trackers, which mid-conversation would silently drop
// strict-mode read state and undo history. A generation bump must move the
// language facet and nothing else.
func TestRefreshPrimary_PreservesSessionState(t *testing.T) {
	pool := enableTestPool()
	root := freshTempDir(t)
	mustWrite(t, filepath.Join(root, "go.mod"), "module x\n")
	installEntryLang(pool, root, "go", &stubClient{id: "go"})

	s := newRefreshSession(t, pool)
	s.attachWorkspace(context.Background(), "file://"+root)
	if got := s.acquiredLanguageName(); got != "go" {
		t.Fatalf("precondition: language = %q, want go", got)
	}

	src := filepath.Join(root, "main.go")
	readAt := time.Now()
	s.readTracker.Record(src, readAt, "sha")

	if _, err := pool.enableLanguage("html"); err != nil {
		t.Fatalf("enableLanguage(html): %v", err)
	}
	s.onBeforeTool(context.Background(), "read_file", json.RawMessage(`{}`))

	if got := s.acquiredLanguageName(); got != "go" {
		t.Errorf("language = %q after an unrelated enable-lsp; an attached primary must never be re-resolved", got)
	}
	if got := s.readTracker.Mtime(src); !got.Equal(readAt) {
		t.Errorf("read tracking for %s lost (mtime %v, want %v) — the refresh must not reset trackers", src, got, readAt)
	}
}

// TestRefreshPrimary_GenerationGate pins the hot-path contract: with no change
// to the pool's language set, a tool call does no detection work at all, and a
// widening is acted on at most once per connection.
func TestRefreshPrimary_GenerationGate(t *testing.T) {
	pool := enableTestPool()
	// A declared workspace with no language marker at all: the post-enable
	// re-detect still finds no language, which is the fruitless-refresh path.
	root := freshTempDir(t)
	mustWrite(t, filepath.Join(root, ".plumb", "config.toml"), "")

	s := newRefreshSession(t, pool)
	s.attachWorkspace(context.Background(), "file://"+root)

	gen := s.lspGenSeen.Load()
	s.refreshPrimaryIfStale(context.Background())
	if got := s.lspGenSeen.Load(); got != gen {
		t.Errorf("generation moved from %d to %d with no enable-lsp in between", gen, got)
	}

	if _, err := pool.enableLanguage("html"); err != nil {
		t.Fatalf("enableLanguage(html): %v", err)
	}
	if got := s.lspGenSeen.Load(); got != gen {
		t.Fatalf("connection observed the new generation before its next tool call (%d)", got)
	}
	// The re-detect finds nothing, so the primary stays unresolved — but the
	// generation must still be claimed, or every later tool call would re-pay the
	// detect.
	s.refreshPrimaryIfStale(context.Background())
	if got := s.lspGenSeen.Load(); got != pool.langsGeneration() {
		t.Errorf("generation after a fruitless refresh = %d, want %d (claimed up front)", got, pool.langsGeneration())
	}
	if got := s.acquiredLanguageName(); got != "" {
		t.Errorf("language = %q on a workspace with no language marker", got)
	}
}

// TestRefreshPrimary_UnattachedIsNoop guards the auto-attach path: a connection
// with no workspace must be left alone for onBeforeTool's seeding logic, not
// half-attached by the language refresh.
func TestRefreshPrimary_UnattachedIsNoop(t *testing.T) {
	pool := enableTestPool()
	s := newRefreshSession(t, pool)

	if _, err := pool.enableLanguage("html"); err != nil {
		t.Fatalf("enableLanguage(html): %v", err)
	}
	s.refreshPrimaryIfStale(context.Background())

	if got := s.workspace(); got != "" {
		t.Errorf("refresh attached an unpinned connection to %q", got)
	}
	if got := s.acquiredLanguageName(); got != "" {
		t.Errorf("refresh resolved language %q on an unpinned connection", got)
	}
}
