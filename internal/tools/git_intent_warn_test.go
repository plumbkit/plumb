package tools

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
)

// intentGitTool builds a git tool with a session identity and the peer-intent
// warning wired to a real collab store, the way conn_register does for a live
// connection.
func intentGitTool(ws, id, name string, store *collab.Store, intentsOn bool) *Git {
	return NewGit(
		WriteDeps{WorkspaceFn: func() string { return ws }},
		func() GitPolicy { return GitPolicy{AllowWrites: true, AllowDestructive: true} },
	).WithSession(id, func() string { return name }).
		WithPeerIntents(func() bool { return intentsOn }, func() *collab.Store { return store })
}

// openIntentStore opens a real collab store for ws (creating collab.db, as the
// share_intent write path would).
func openIntentStore(t *testing.T, ws string) *collab.Store {
	t.Helper()
	store, err := collab.Open(ws)
	if err != nil {
		t.Fatalf("open collab store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func putPeerIntent(t *testing.T, store *collab.Store, id, name, body string, globs []string) {
	t.Helper()
	in := collab.IntentInput{AuthorSession: name, AuthorID: id, Body: body, PathGlobs: globs, TTL: time.Hour}
	if err := store.PutIntent(context.Background(), in, time.Now()); err != nil {
		t.Fatalf("put intent: %v", err)
	}
}

// TestGitPeerIntentWarning_StateVerbsWarn is the acceptance scenario: with a
// peer intent covering the repository, repo-state verbs (commit, switch) lead
// their responses with a warning naming the peer and the claim, while reads,
// index-only writes, and a no-intent baseline never warn.
func TestGitPeerIntentWarning_StateVerbsWarn(t *testing.T) {
	requireGit(t)
	repo := initTestRepo(t)
	store := openIntentStore(t, repo)
	sessA := intentGitTool(repo, "sess-a", "amber-fox", store, true)

	// No intents at all: a state verb stays silent.
	stageFile(t, sessA, repo, "one.txt", "one\n", false)
	out, err := callGit(t, sessA, map[string]any{"subcommand": "commit", "message": "one"})
	if err != nil {
		t.Fatalf("commit one: %v", err)
	}
	if strings.Contains(out, "plumb-warning") {
		t.Errorf("no intents, but commit warned:\n%s", out)
	}

	putPeerIntent(t, store, "sess-b", "blue-heron", "rebasing ops main", nil)

	// A read and an index-only write never warn.
	if out, err := callGit(t, sessA, map[string]any{"subcommand": "status"}); err != nil {
		t.Fatalf("status: %v", err)
	} else if strings.Contains(out, "plumb-warning") {
		t.Errorf("read-tier status must never warn, got:\n%s", out)
	}
	stageFile(t, sessA, repo, "two.txt", "two\n", false) // stageFile runs `add` and asserts success
	if out, err := callGit(t, sessA, map[string]any{"subcommand": "add", "files": []string{"two.txt"}}); err != nil {
		t.Fatalf("add: %v", err)
	} else if strings.Contains(out, "peer intent") {
		t.Errorf("index-only add must not warn about repo intents, got:\n%s", out)
	}

	// commit and switch are repo-state verbs: the warning names the peer and
	// the claim, and never blocks the op.
	out, err = callGit(t, sessA, map[string]any{"subcommand": "commit", "message": "two"})
	if err != nil {
		t.Fatalf("commit two: %v", err)
	}
	for _, want := range []string{"peer intent claims cover this repository", `peer blue-heron claimed: "rebasing ops main"`, "expires in"} {
		if !strings.Contains(out, want) {
			t.Errorf("commit warning missing %q:\n%s", want, out)
		}
	}
	out, err = callGit(t, sessA, map[string]any{"subcommand": "switch", "args": []string{"--create", "topic"}})
	if err != nil {
		t.Fatalf("switch: %v", err)
	}
	if !strings.Contains(out, `peer blue-heron claimed: "rebasing ops main"`) {
		t.Errorf("switch response missing the peer intent warning:\n%s", out)
	}
}

// TestGitPeerIntentWarning_OwnIntentExcluded: a session never warns about its
// own intent.
func TestGitPeerIntentWarning_OwnIntentExcluded(t *testing.T) {
	requireGit(t)
	repo := initTestRepo(t)
	store := openIntentStore(t, repo)
	sessA := intentGitTool(repo, "sess-a", "amber-fox", store, true)

	putPeerIntent(t, store, "sess-a", "amber-fox", "rebasing ops main", nil)
	stageFile(t, sessA, repo, "a.txt", "from a\n", false)
	out, err := callGit(t, sessA, map[string]any{"subcommand": "commit", "message": "a"})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if strings.Contains(out, "peer intent") {
		t.Errorf("own intent must never warn, got:\n%s", out)
	}
}

// TestGitPeerIntentWarning_UnwiredAndDisabled: without WithPeerIntents (or
// with the [collab] intents flag off) a state verb stays silent even with a
// peer intent stored.
func TestGitPeerIntentWarning_UnwiredAndDisabled(t *testing.T) {
	requireGit(t)
	repo := initTestRepo(t)
	store := openIntentStore(t, repo)
	putPeerIntent(t, store, "sess-b", "blue-heron", "rebasing ops main", nil)

	unwired := sessionGitTool(repo, "sess-a", "amber-fox")
	stageFile(t, unwired, repo, "a.txt", "from a\n", false)
	out, err := callGit(t, unwired, map[string]any{"subcommand": "commit", "message": "a"})
	if err != nil {
		t.Fatalf("unwired commit: %v", err)
	}
	if strings.Contains(out, "peer intent") {
		t.Errorf("unwired tool must never warn, got:\n%s", out)
	}

	disabled := intentGitTool(repo, "sess-c", "calm-crow", store, false)
	stageFile(t, disabled, repo, "c.txt", "from c\n", false)
	out, err = callGit(t, disabled, map[string]any{"subcommand": "commit", "message": "c"})
	if err != nil {
		t.Fatalf("disabled commit: %v", err)
	}
	if strings.Contains(out, "peer intent") {
		t.Errorf("intents-off tool must never warn, got:\n%s", out)
	}
}

// TestGitPeerIntentWarning_NestedRepoGlobs: for a repo nested under the
// workspace, a scoped intent warns only when a glob covers the repo's
// workspace-relative path; an unscoped broadcast always covers it.
func TestGitPeerIntentWarning_NestedRepoGlobs(t *testing.T) {
	requireGit(t)
	ws := t.TempDir()
	runGitDirect(t, ws, "init", "sub")
	repo := ws + "/sub"
	runGitDirect(t, repo, "config", "user.email", "test@example.com")
	runGitDirect(t, repo, "config", "user.name", "Test User")
	runGitDirect(t, repo, "commit", "--allow-empty", "-m", "initial")
	store := openIntentStore(t, ws)
	sessA := intentGitTool(ws, "sess-a", "amber-fox", store, true)

	commit := func(msg string) string {
		t.Helper()
		if err := os.WriteFile(repo+"/"+msg+".txt", []byte(msg+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := callGit(t, sessA, map[string]any{"subcommand": "add", "files": []string{msg + ".txt"}, "repo": repo}); err != nil {
			t.Fatalf("add %s: %v", msg, err)
		}
		out, err := callGit(t, sessA, map[string]any{"subcommand": "commit", "message": msg, "repo": repo})
		if err != nil {
			t.Fatalf("commit %s: %v", msg, err)
		}
		return out
	}

	putPeerIntent(t, store, "sess-b", "blue-heron", "working elsewhere", []string{"other/**"})
	if out := commit("one"); strings.Contains(out, "peer intent") {
		t.Errorf("intent scoped away from the repo must not warn, got:\n%s", out)
	}

	putPeerIntent(t, store, "sess-b", "blue-heron", "touching the submodule", []string{"sub/**"})
	if out := commit("two"); !strings.Contains(out, `peer blue-heron claimed: "touching the submodule"`) {
		t.Errorf("intent covering the repo path should warn, got:\n%s", out)
	}
}

// TestFormatRepoIntentWarning pins the filtering and rendering without a git
// binary: own and expired rows are excluded, non-covering scopes are excluded,
// and matches render as advisory claims.
func TestFormatRepoIntentWarning(t *testing.T) {
	now := time.Now()
	row := func(id, name, body string, globs []string, expires time.Time) collab.Row {
		return collab.Row{
			Kind: collab.KindIntent, AuthorID: id, AuthorSession: name,
			Body: body, PathGlobs: globs, CreatedAt: now, ExpiresAt: expires,
		}
	}
	live := now.Add(40 * time.Minute)
	ws := t.TempDir()
	repo := ws + "/plumb"

	// Only the live, covering peer rows survive: own session, expired, and
	// mis-scoped claims are all excluded.
	intents := []collab.Row{
		row("sess-b", "blue-heron", "rebasing ops main", nil, live),
		row("self", "amber-fox", "my own claim", nil, live),
		row("sess-c", "calm-crow", "stale claim", nil, now.Add(-time.Minute)),
		row("sess-d", "dim-dove", "elsewhere", []string{"other/**"}, live),
		row("sess-e", "eager-emu", "covering glob", []string{"plumb/**"}, live),
	}
	out := formatRepoIntentWarning(intents, ws, repo, "self", now)
	for _, want := range []string{`peer blue-heron claimed: "rebasing ops main"`, `peer eager-emu claimed: "covering glob"`, "expires in 40 min"} {
		if !strings.Contains(out, want) {
			t.Errorf("warning missing %q:\n%s", want, out)
		}
	}
	for _, absent := range []string{"amber-fox", "calm-crow", "dim-dove"} {
		if strings.Contains(out, absent) {
			t.Errorf("warning should not mention %s:\n%s", absent, out)
		}
	}

	if got := formatRepoIntentWarning(nil, ws, repo, "self", now); got != "" {
		t.Errorf("no intents should render no warning, got %q", got)
	}

	// More matches than the quote cap collapse into a count line.
	many := make([]collab.Row, 0, 5)
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		many = append(many, row("sess-"+id, "name-"+id, "claim "+id, nil, live))
	}
	out = formatRepoIntentWarning(many, ws, repo, "self", now)
	if !strings.Contains(out, "… and 2 more peer intent claim(s)") {
		t.Errorf("expected the overflow count line, got:\n%s", out)
	}
	if strings.Contains(out, "name-d") || strings.Contains(out, "name-e") {
		t.Errorf("claims past the cap must not be quoted, got:\n%s", out)
	}
}

func TestRepoStateVerb(t *testing.T) {
	cases := []struct {
		sub  string
		tier gitTier
		want bool
	}{
		{"reset", tierDestructive, true},
		{"rebase", tierDestructive, true},
		{"checkout", tierDestructive, true},
		{"restore", tierDestructive, true},
		{"branch", tierDestructive, true}, // branch delete
		{"commit", tierWrite, true},
		{"switch", tierWrite, true},
		{"checkout", tierWrite, true}, // checkout -b
		{"add", tierWrite, false},     // index only
		{"restore", tierWrite, false}, // restore --staged: index only
		{"stash", tierWrite, false},   // stash push: tree/index only
		{"branch", tierWrite, false},  // branch create: additive
		{"tag", tierWrite, false},     // tag create: additive
		{"mv", tierWrite, false},
		{"status", tierRead, false},
		{"push", tierNetwork, false},
	}
	for _, c := range cases {
		if got := repoStateVerb(c.sub, c.tier); got != c.want {
			t.Errorf("repoStateVerb(%q, %d) = %v, want %v", c.sub, c.tier, got, c.want)
		}
	}
}

func TestIntentCoversRepo(t *testing.T) {
	ws := t.TempDir()
	nested := ws + "/plumb"
	cases := []struct {
		name     string
		globs    []string
		repoRoot string
		want     bool
	}{
		{"unscoped always covers", nil, nested, true},
		{"unscoped covers outside-workspace repo", nil, "/elsewhere/repo", true},
		{"repo is the workspace root: any scope covers", []string{"internal/tools/**"}, ws, true},
		{"glob matching the repo's relative path", []string{"plumb/**"}, nested, true},
		{"slashless glob matching the repo basename", []string{"plumb"}, nested, true},
		{"glob away from the repo", []string{"other/**"}, nested, false},
		{"scoped intent cannot cover an outside-workspace repo", []string{"repo/**"}, "/elsewhere/repo", false},
	}
	for _, c := range cases {
		if got := intentCoversRepo(c.globs, ws, c.repoRoot); got != c.want {
			t.Errorf("%s: intentCoversRepo(%v, %s) = %v, want %v", c.name, c.globs, c.repoRoot, got, c.want)
		}
	}
}
