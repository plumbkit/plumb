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
// connection. The hint budget is unbounded (0) — see
// intentGitToolWithBudget for the Finding-2 budget test.
func intentGitTool(ws, id, name string, store *collab.Store, intentsOn bool) *Git {
	return intentGitToolWithBudget(ws, id, name, store, intentsOn, 0)
}

// intentGitToolWithBudget is intentGitTool with an explicit [collab]
// hint_budget_bytes snapshot, for pinning that the git repo-intent warning is
// clamped to it like every other injected peer-signal block.
func intentGitToolWithBudget(ws, id, name string, store *collab.Store, intentsOn bool, hintBudgetBytes int) *Git {
	return NewGit(
		WriteDeps{WorkspaceFn: func() string { return ws }},
		func() GitPolicy { return GitPolicy{AllowWrites: true, AllowDestructive: true} },
	).WithSession(id, func() string { return name }).
		WithPeerIntents(func() bool { return intentsOn }, func() *collab.Store { return store },
			func() int { return hintBudgetBytes })
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

// TestGitPeerIntentWarning_TierAwareCoverage is the Finding-1 acceptance
// scenario end-to-end, with a real git binary, in the common single-repo
// layout (repo == workspace): a narrow-scoped peer intent must not warn on a
// write-tier repo-state op (commit) but must still warn on a destructive-tier
// op (reset), while an unscoped broadcast warns on both.
func TestGitPeerIntentWarning_TierAwareCoverage(t *testing.T) {
	requireGit(t)
	repo := initTestRepo(t)
	store := openIntentStore(t, repo)
	sessA := intentGitTool(repo, "sess-a", "amber-fox", store, true)

	putPeerIntent(t, store, "sess-b", "blue-heron", "working on the site", []string{"site/**"})

	// Narrow glob + commit (write-tier): no warning — this is the exact
	// scenario that regresses to `true` on the pre-fix code (rel == "."
	// returned true unconditionally, regardless of tier or glob scope).
	stageFile(t, sessA, repo, "one.txt", "one\n", false)
	out, err := callGit(t, sessA, map[string]any{"subcommand": "commit", "message": "one"})
	if err != nil {
		t.Fatalf("commit one: %v", err)
	}
	if strings.Contains(out, "plumb-warning") {
		t.Errorf("narrow glob must not warn on a write-tier commit, got:\n%s", out)
	}

	// The same narrow glob + reset (destructive-tier): warns.
	out, err = callGit(t, sessA, map[string]any{"subcommand": "reset", "args": []string{"--soft", "HEAD~1"}, "confirm": true})
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if !strings.Contains(out, `peer blue-heron claimed: "working on the site"`) {
		t.Errorf("narrow glob must still warn on a destructive-tier reset, got:\n%s", out)
	}

	// An unscoped broadcast intent + commit (write-tier): warns.
	stageFile(t, sessA, repo, "one.txt", "one\n", false)
	putPeerIntent(t, store, "sess-c", "calm-crow", "rebasing ops main", nil)
	out, err = callGit(t, sessA, map[string]any{"subcommand": "commit", "message": "one again"})
	if err != nil {
		t.Fatalf("commit again: %v", err)
	}
	if !strings.Contains(out, `peer calm-crow claimed: "rebasing ops main"`) {
		t.Errorf("unscoped intent must warn on a write-tier commit, got:\n%s", out)
	}
}

// TestGitPeerIntentWarning_AncestorRepoLayout is the Finding-4 live-git
// scenario: the workspace is a SUBDIRECTORY of the git top-level (a monorepo
// pinned to one service). A scoped intent can now match, tier-aware the same
// as the repo == workspace case.
func TestGitPeerIntentWarning_AncestorRepoLayout(t *testing.T) {
	requireGit(t)
	repoRoot := t.TempDir()
	runGitDirect(t, repoRoot, "init")
	runGitDirect(t, repoRoot, "config", "user.email", "test@example.com")
	runGitDirect(t, repoRoot, "config", "user.name", "Test User")
	ws := repoRoot + "/services/api"
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ws+"/init.txt", []byte("init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDirect(t, repoRoot, "add", "services/api/init.txt")
	runGitDirect(t, repoRoot, "commit", "-m", "initial")
	store := openIntentStore(t, ws)
	sessA := intentGitTool(ws, "sess-a", "amber-fox", store, true)

	commit := func(msg string) string {
		t.Helper()
		if err := os.WriteFile(ws+"/"+msg+".txt", []byte(msg+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		// files are relative to the git TOPLEVEL, not the subdirectory repo —
		// see TestGit_AddRelativePathFromSubdirRepo.
		if _, err := callGit(t, sessA, map[string]any{"subcommand": "add", "files": []string{"services/api/" + msg + ".txt"}}); err != nil {
			t.Fatalf("add %s: %v", msg, err)
		}
		out, err := callGit(t, sessA, map[string]any{"subcommand": "commit", "message": msg})
		if err != nil {
			t.Fatalf("commit %s: %v", msg, err)
		}
		return out
	}

	// A narrow, workspace-scoped glob must not warn on a write-tier commit —
	// pre Finding-4 the ancestor direction fell through to "outside the
	// workspace" and never matched at all, unscoped broadcast excepted.
	putPeerIntent(t, store, "sess-b", "blue-heron", "touching a handler", []string{"handlers/**"})
	if out := commit("one"); strings.Contains(out, "plumb-warning") {
		t.Errorf("narrow glob must not warn on a write-tier commit, got:\n%s", out)
	}

	// A workspace-wide glob DOES warn — it is the best expressible proxy for
	// "repo-wide" from a workspace pinned below the git top-level.
	putPeerIntent(t, store, "sess-c", "calm-crow", "broad claim", []string{"**"})
	if out := commit("two"); !strings.Contains(out, `peer calm-crow claimed: "broad claim"`) {
		t.Errorf("workspace-wide glob should warn even though it names a subset of the full repo, got:\n%s", out)
	}
}

// TestGitPeerIntentWarning_SurfacesOnFailure is the Finding-3 test: the
// warning is computed BEFORE the git child runs, so it must still surface
// when that child then fails — exactly when a peer's claim is most likely to
// explain the failure, and the query cost has already been paid.
func TestGitPeerIntentWarning_SurfacesOnFailure(t *testing.T) {
	requireGit(t)
	repo := initTestRepo(t)
	store := openIntentStore(t, repo)
	sessA := intentGitTool(repo, "sess-a", "amber-fox", store, true)

	putPeerIntent(t, store, "sess-b", "blue-heron", "rebasing ops main", nil)

	// Nothing staged: commit fails ("nothing to commit"), but the warning
	// that was computed before the git child ran must still appear.
	_, err := callGit(t, sessA, map[string]any{"subcommand": "commit", "message": "nothing to commit"})
	if err == nil {
		t.Fatal("expected the commit to fail with nothing staged")
	}
	if !strings.Contains(err.Error(), `peer blue-heron claimed: "rebasing ops main"`) {
		t.Errorf("a failed commit must still surface the peer intent warning, got:\n%s", err.Error())
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
	// tierDestructive here so the "covering glob" case (a glob matching the
	// nested repo's own path) and the unscoped broadcasts all still match —
	// tier only changes behaviour for a SCOPED intent at rel == "." or the
	// ancestor layout, neither of which this table exercises; see
	// TestIntentCoversRepo for the tier-specific cases.
	out := formatRepoIntentWarning(intents, ws, repo, "self", now, tierDestructive, 0)
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

	if got := formatRepoIntentWarning(nil, ws, repo, "self", now, tierDestructive, 0); got != "" {
		t.Errorf("no intents should render no warning, got %q", got)
	}

	// More matches than the quote cap collapse into a count line.
	many := make([]collab.Row, 0, 5)
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		many = append(many, row("sess-"+id, "name-"+id, "claim "+id, nil, live))
	}
	out = formatRepoIntentWarning(many, ws, repo, "self", now, tierDestructive, 0)
	if !strings.Contains(out, "… and 2 more peer intent claim(s)") {
		t.Errorf("expected the overflow count line, got:\n%s", out)
	}
	if strings.Contains(out, "name-d") || strings.Contains(out, "name-e") {
		t.Errorf("claims past the cap must not be quoted, got:\n%s", out)
	}
}

// TestFormatRepoIntentWarning_HonoursHintBudget is the Finding-2 test: a
// configured small budget actually bounds the emitted block, clamped on a
// UTF-8 boundary like every other injected peer-signal block.
func TestFormatRepoIntentWarning_HonoursHintBudget(t *testing.T) {
	now := time.Now()
	ws := t.TempDir()
	intents := []collab.Row{
		{
			Kind: collab.KindIntent, AuthorID: "sess-b", AuthorSession: "blue-heron",
			Body:      "rebasing ops main across a long paragraph of free text that would normally blow well past a tight byte budget on its own",
			CreatedAt: now, ExpiresAt: now.Add(40 * time.Minute),
		},
	}
	const budget = 80
	out := formatRepoIntentWarning(intents, ws, ws, "self", now, tierWrite, budget)
	if out == "" {
		t.Fatal("expected a non-empty (but clamped) warning")
	}
	if len(out) > budget {
		t.Errorf("warning exceeds configured hint_budget_bytes: %d bytes > %d:\n%s", len(out), budget, out)
	}
	// Unbounded (0) stays unbounded — the default in every other test in this file.
	unbounded := formatRepoIntentWarning(intents, ws, ws, "self", now, tierWrite, 0)
	if len(unbounded) <= budget {
		t.Fatalf("test setup: expected the unclamped warning to exceed %d bytes, got %d", budget, len(unbounded))
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
	// ancestorWS is a subdirectory workspace pinned under a larger git
	// top-level (Finding 4): the repo is an ANCESTOR of the workspace, the
	// opposite direction from nested above (repo nested under the workspace).
	ancestorRepo := t.TempDir()
	ancestorWS := ancestorRepo + "/services/api"
	cases := []struct {
		name     string
		globs    []string
		repoRoot string
		tier     gitTier
		ws       string // defaults to ws when empty
		want     bool
	}{
		{"unscoped always covers (write)", nil, nested, tierWrite, "", true},
		{"unscoped always covers (destructive)", nil, nested, tierDestructive, "", true},
		{"unscoped covers outside-workspace repo", nil, "/elsewhere/repo", tierWrite, "", true},

		// Finding 1 — the acceptance scenario: repo IS the workspace root
		// (the common single-repo layout). A narrow glob no longer covers a
		// write-tier repo-state op (this is the case that FAILS on the
		// pre-fix code, which returned true unconditionally at rel == ".").
		{"repo is the workspace root: narrow glob does NOT cover a write-tier op", []string{"site/**"}, ws, tierWrite, "", false},
		{"repo is the workspace root: narrow glob covers a destructive-tier op", []string{"site/**"}, ws, tierDestructive, "", true},
		{"repo is the workspace root: workspace-wide glob covers a write-tier op", []string{"**"}, ws, tierWrite, "", true},
		{"repo is the workspace root: bare '.' glob covers a write-tier op", []string{"."}, ws, tierWrite, "", true},

		{"glob matching the repo's relative path", []string{"plumb/**"}, nested, tierWrite, "", true},
		{"slashless glob matching the repo basename", []string{"plumb"}, nested, tierWrite, "", true},
		{"glob away from the repo", []string{"other/**"}, nested, tierDestructive, "", false},
		{"scoped intent cannot cover an outside-workspace repo", []string{"repo/**"}, "/elsewhere/repo", tierWrite, "", false},

		// Finding 4 — the workspace is a SUBDIRECTORY of the git top-level
		// (repoRoot is an ancestor of ws): a scoped intent can still match,
		// using the same reasoning as the repo == workspace case, since
		// every workspace-relative path is inside the larger repository.
		{"ancestor repo: narrow glob does NOT cover a write-tier op", []string{"handlers/**"}, ancestorRepo, tierWrite, ancestorWS, false},
		{"ancestor repo: narrow glob covers a destructive-tier op", []string{"handlers/**"}, ancestorRepo, tierDestructive, ancestorWS, true},
		{"ancestor repo: workspace-wide glob covers a write-tier op", []string{"**"}, ancestorRepo, tierWrite, ancestorWS, true},
	}
	for _, c := range cases {
		effWS := c.ws
		if effWS == "" {
			effWS = ws
		}
		if got := intentCoversRepo(c.globs, effWS, c.repoRoot, c.tier); got != c.want {
			t.Errorf("%s: intentCoversRepo(%v, %s, %s, tier=%d) = %v, want %v", c.name, c.globs, effWS, c.repoRoot, c.tier, got, c.want)
		}
	}
}

// TestIntentCoversRepo_NarrowVsDestructive_FailsPreFix documents, standalone,
// the specific Finding-1 regression: at rel == "." a narrow glob must NOT
// cover a write-tier repo-state op, even though the same glob DOES cover a
// destructive-tier op. On the pre-fix code (intentCoversRepo with no tier
// parameter, unconditionally returning true at rel == ".") the write-tier
// case here fails: both assertions would observe `true`. See the PR
// verification notes for confirmation by temporary revert.
func TestIntentCoversRepo_NarrowVsDestructive_FailsPreFix(t *testing.T) {
	ws := t.TempDir()
	narrow := []string{"site/**"}
	if got := intentCoversRepo(narrow, ws, ws, tierWrite); got {
		t.Error("narrow glob must NOT cover a write-tier repo-state op at rel == \".\" — this is the Finding 1 regression")
	}
	if got := intentCoversRepo(narrow, ws, ws, tierDestructive); !got {
		t.Error("the same narrow glob must still cover a destructive-tier op — the broad behaviour is intentionally kept there")
	}
}
