//go:build integration

// Tier-D coverage for the self-test design: behaviours that are unsafe or
// non-deterministic to drive against a live workspace, so the agent self-test
// (the `selftest` MCP prompt) defers them here. None of these need gopls — they
// use a bare, language-less fixture so the daemon attaches instantly.
//
// Also hosts the anti-rot parity guard: the live tools/list must equal the
// self-test's canonical coverage list (mcp.SelftestToolNames), so a tool added
// without a checklist entry fails CI.
//
// Run: go test -tags=integration -timeout=3m ./cmd/smoke/
package smoke_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/tools"
)

const tierDTimeout = 60 * time.Second

// ─── fixtures + helpers ──────────────────────────────────────────────────────

// makeBareFixture creates a workspace with only a .plumb/ marker — no language
// marker, so plumb attaches as LanguageNone and never starts a language server.
func makeBareFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	if err := os.Mkdir(filepath.Join(dir, ".plumb"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// makeBareGitFixture is makeBareFixture plus a git repo with the given files
// committed, so dirty-guard and strict-mode behaviours have a clean baseline.
func makeBareGitFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := makeBareFixture(t)
	gitInRepo(t, dir, "init")
	setGitIdentity(t, dir)
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitInRepo(t, dir, "add", "-A")
	gitInRepo(t, dir, "commit", "-m", "initial")
	return dir
}

// gitInRepo runs a git command in dir (test-side setup, not via plumb).
func gitInRepo(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// setGitIdentity writes a repo-local committer identity so commits succeed even
// though the daemon runs with an isolated HOME (no global git config).
func setGitIdentity(t *testing.T, dir string) {
	t.Helper()
	gitInRepo(t, dir, "config", "user.email", "smoke@test.local")
	gitInRepo(t, dir, "config", "user.name", "smoke")
}

func mkTmpHome(t *testing.T) string {
	t.Helper()
	tmpHome, err := os.MkdirTemp("/tmp", "plsmk")
	if err != nil {
		t.Fatal("create tmpHome:", err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpHome) })
	return tmpHome
}

// callResult sends a tools/call and returns the text result plus whether it was
// an error — unlike mcpClient.call, it does not fail the test on an error, so a
// test can assert that a call was *rejected*.
func callResult(t *testing.T, c *mcpClient, tool string, args map[string]any, timeout time.Duration) (string, bool) {
	t.Helper()
	id, err := c.send("tools/call", map[string]any{"name": tool, "arguments": args})
	if err != nil {
		t.Fatalf("tools/call %s: send: %v", tool, err)
	}
	msg, err := c.recv(id, timeout)
	if err != nil {
		t.Fatalf("tools/call %s: %v", tool, err)
	}
	if msg.Error != nil {
		return msg.Error.Message, true
	}
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(msg.Result, &result); err != nil {
		t.Fatalf("tools/call %s: unmarshal: %v", tool, err)
	}
	var sb strings.Builder
	for _, ct := range result.Content {
		if ct.Type == "text" {
			sb.WriteString(ct.Text)
		}
	}
	return sb.String(), result.IsError
}

// ─── parity guard ────────────────────────────────────────────────────────────

// TestSmoke_ToolListParity is the anti-rot guard: the set of registered tools
// (live tools/list) must equal the self-test's canonical coverage list. Adding
// a tool without a checklist entry, or leaving a stale entry, fails here.
func TestSmoke_ToolListParity(t *testing.T) {
	plumbBin := buildPlumb(t)
	fixture := makeBareFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), tierDTimeout)
	defer cancel()

	c := newMCPClient(t, ctx, plumbBin, mkTmpHome(t), fixture)
	c.initialize(t, fixture)

	id, err := c.send("tools/list", map[string]any{})
	if err != nil {
		t.Fatal("tools/list send:", err)
	}
	msg, err := c.recv(id, toolTimeout)
	if err != nil {
		t.Fatal("tools/list:", err)
	}
	if msg.Error != nil {
		t.Fatalf("tools/list error: %s", msg.Error.Message)
	}
	var result struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(msg.Result, &result); err != nil {
		t.Fatal("tools/list unmarshal:", err)
	}

	live := make(map[string]bool, len(result.Tools))
	for _, tl := range result.Tools {
		live[tl.Name] = true
	}
	checklist := make(map[string]bool)
	for _, n := range mcp.SelftestToolNames() {
		checklist[n] = true
	}

	for n := range live {
		if !checklist[n] {
			t.Errorf("tool %q is registered but missing from the self-test checklist — add it to a tier in internal/mcp/selftest_prompt.go", n)
		}
	}
	for n := range checklist {
		if !live[n] {
			t.Errorf("tool %q is in the self-test checklist but not registered — stale entry in internal/mcp/selftest_prompt.go", n)
		}
	}
}

// ─── lean manifest ─────────────────────────────────────────────────────────────

// listToolNames fetches tools/list and returns the advertised tool names.
func listToolNames(t *testing.T, c *mcpClient) map[string]bool {
	t.Helper()
	id, err := c.send("tools/list", map[string]any{})
	if err != nil {
		t.Fatal("tools/list send:", err)
	}
	msg, err := c.recv(id, toolTimeout)
	if err != nil {
		t.Fatal("tools/list:", err)
	}
	if msg.Error != nil {
		t.Fatalf("tools/list error: %s", msg.Error.Message)
	}
	var result struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(msg.Result, &result); err != nil {
		t.Fatal("tools/list unmarshal:", err)
	}
	names := make(map[string]bool, len(result.Tools))
	for _, tl := range result.Tools {
		names[tl.Name] = true
	}
	return names
}

// TestSmoke_LeanManifestKeepsBootstrap drives the live daemon with a client
// that resolves to the LEAN profile — via a [tools.client_profiles] override
// keyed to the harness's clientInfo name ("smoke-test", see
// mcpClient.initialize) — and asserts over the wire that the advertised manifest
// is exactly LeanTools ∪ BootstrapTools (== LeanTools today, bootstrap ⊆ lean)
// with all four bootstrap tools present. TestSmoke_ToolListParity covers the
// complementary full/unknown path against the same live tools/list.
//
// The override is written to the GLOBAL config, not the workspace's. It used to
// go in .plumb/config.toml, which no longer works and should not: a project
// config narrowing the advertised tool set is an evidence-suppression vector —
// for a client that discovers tools only from tools/list, a repository could hide
// search_in_files and find_files from the very session auditing it. Only
// WIDENING survives from a project file now (see docs/project-config-trust.md).
// What this test is actually about is the shape of the lean manifest, not the
// layer the profile came from, so it asserts the same thing through the layer a
// user controls.
//
// The profile still resolves asynchronously relative to initialize, so the test
// polls tools/list until it takes effect before the exact-set assertions.
func TestSmoke_LeanManifestKeepsBootstrap(t *testing.T) {
	plumbBin := buildPlumb(t)
	fixture := makeBareFixture(t)
	tmpHome := mkTmpHome(t)
	// isolatedEnv points XDG_CONFIG_HOME at <tmpHome>/.config, so this is the
	// global config the daemon under test will read.
	globalDir := filepath.Join(tmpHome, ".config", "plumb")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal("create global config dir:", err)
	}
	cfg := "[tools.client_profiles]\n\"smoke-test\" = \"lean\"\n"
	if err := os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal("write global config:", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), tierDTimeout)
	defer cancel()

	c := newMCPClient(t, ctx, plumbBin, tmpHome, fixture)
	c.initialize(t, fixture)
	c.call(t, "session_start", map[string]any{"workspace": fixture}, toolTimeout)

	// Poll until the client-override resolves this connection to lean — the
	// sentinel is any commodity tool (copy_file) disappearing from the manifest.
	deadline := time.Now().Add(15 * time.Second)
	var live map[string]bool
	for {
		live = listToolNames(t, c)
		if !live["copy_file"] {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("lean profile never took effect — copy_file still advertised in the %d-tool manifest: %v", len(live), live)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Exactly LeanTools ∪ BootstrapTools: every expected name advertised, no
	// stragglers beyond the expected set.
	for name := range tools.LeanTools {
		if !live[name] {
			t.Errorf("lean manifest is missing lean tool %q", name)
		}
	}
	for name := range tools.BootstrapTools {
		if !live[name] {
			t.Errorf("lean manifest is missing bootstrap tool %q", name)
		}
	}
	for name := range live {
		if !tools.IsLean(name) && !tools.IsBootstrap(name) {
			t.Errorf("lean manifest advertises unexpected tool %q (not in LeanTools ∪ BootstrapTools)", name)
		}
	}

	// The orientation packet must name the resolved profile and reason — the
	// session_start surface Task 3 wired, observed here over the wire.
	out := c.call(t, "session_start", map[string]any{"workspace": fixture}, toolTimeout)
	assertContains(t, "session_start profile note", out, "Tool profile: lean")
	assertContains(t, "session_start profile reason", out, "(reason: client-override)")
}

// ─── strict mode ─────────────────────────────────────────────────────────────

// TestSmoke_StrictModeRejectsUnreadEdit proves strict mode blocks an edit_file
// that was not preceded by a read_file, and that reading first opens the gate.
func TestSmoke_StrictModeRejectsUnreadEdit(t *testing.T) {
	plumbBin := buildPlumb(t)
	fixture := makeBareGitFixture(t, map[string]string{"note.txt": "alpha\n"})
	ctx, cancel := context.WithTimeout(context.Background(), tierDTimeout)
	defer cancel()

	c := newMCPClient(t, ctx, plumbBin, mkTmpHome(t), fixture, "PLUMB_STRICT_EDITS=true")
	c.initialize(t, fixture)
	c.call(t, "session_start", map[string]any{"workspace": fixture}, toolTimeout)

	note := filepath.Join(fixture, "note.txt")

	out, isErr := callResult(t, c, "edit_file", map[string]any{
		"file_path": note,
		"edits":     []map[string]any{{"old_string": "alpha", "new_string": "beta"}},
	}, toolTimeout)
	if !isErr {
		t.Fatalf("strict mode did not reject an unread edit; got: %s", out)
	}
	assertContains(t, "strict edit", out, "has not been read")

	readOut := c.call(t, "read_file", map[string]any{"file_path": note}, toolTimeout)
	mtime := extractMtime(t, readOut)
	editOut := c.call(t, "edit_file", map[string]any{
		"file_path":      note,
		"edits":          []map[string]any{{"old_string": "alpha", "new_string": "beta"}},
		"expected_mtime": mtime,
	}, toolTimeout)
	assertContains(t, "edit after read", editOut, "applied 1 edit")
}

// anyMtimeRe pulls an mtime field out of ANY read-recording tool's output.
// read_file/read_symbol emit "# plumb-read mtime=<value> ..."; read_multiple_files
// emits a per-file compact "# mtime=<value> ..." (see rmfCompactHeader) — this
// covers both, unlike extractMtime's fixed "# plumb-read" prefix.
var anyMtimeRe = regexp.MustCompile(`mtime=(\S+)`)

// extractAnyMtime pulls the first mtime= field out of a read-recording tool's
// output, whichever header shape it uses.
func extractAnyMtime(t *testing.T, readOut string) string {
	t.Helper()
	m := anyMtimeRe.FindStringSubmatch(readOut)
	if m == nil {
		t.Fatal("extractAnyMtime: no mtime= field found in output:\n" + readOut)
	}
	return m[1]
}

// TestStrictReadRoundTrip is the PLAN-361 wiring guard, parameterized over
// EVERY read-recording tool (read_file, read_symbol, read_multiple_files):
// under [edits] strict mode, an edit_file on a never-read path is rejected,
// then reading the SAME path via the tool under test opens the strict-mode
// gate and the follow-up edit_file succeeds. A future read-shaped tool added
// to the registry without its ReadTracker/readsFor wiring threaded through
// (the read_multiple_files defect, PLAN-357) goes red here immediately, not
// just in the unit-level TestToolWiringParity (internal/cli) — this exercises
// the full wire protocol end to end, over a live daemon.
func TestStrictReadRoundTrip(t *testing.T) {
	plumbBin := buildPlumb(t)

	const sampleGo = "package sample\n\n// Greet returns a greeting.\nfunc Greet() string {\n\treturn \"hi\"\n}\n"

	cases := []struct {
		tool      string
		file      string
		readArgs  func(path string) map[string]any
		oldString string
		newString string
		// lspBacked marks the case whose read touches a .go file and is
		// therefore the first — and only — call in this test to reach the
		// language server, absorbing the whole gopls cold start. See
		// lspToolTimeout.
		lspBacked bool
	}{
		{
			tool:      "read_file",
			file:      "note.txt",
			readArgs:  func(path string) map[string]any { return map[string]any{"file_path": path} },
			oldString: "alpha",
			newString: "beta",
		},
		{
			tool: "read_symbol",
			file: "sample.go",
			readArgs: func(path string) map[string]any {
				return map[string]any{"path": path, "name": "Greet"}
			},
			oldString: "hi",
			newString: "hello",
			lspBacked: true,
		},
		{
			tool: "read_multiple_files",
			file: "note.txt",
			readArgs: func(path string) map[string]any {
				return map[string]any{"paths": []string{path}}
			},
			oldString: "alpha",
			newString: "beta",
		},
	}

	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			fixture := makeBareGitFixture(t, map[string]string{
				"note.txt":  "alpha\n",
				"sample.go": sampleGo,
			})
			// The LSP-backed case gets the cold-start budget on its read AND on
			// the subprocess context that has to outlive it — a call timeout
			// longer than the context bounding the client is not a budget, it is
			// a comment. Both are derived from lspToolTimeout so raising one
			// carries to the other.
			readTimeout, budget := toolTimeout, tierDTimeout
			if tc.lspBacked {
				readTimeout = lspToolTimeout
				budget = tierDTimeout + lspToolTimeout
			}
			ctx, cancel := context.WithTimeout(context.Background(), budget)
			defer cancel()

			c := newMCPClient(t, ctx, plumbBin, mkTmpHome(t), fixture, "PLUMB_STRICT_EDITS=true")
			c.initialize(t, fixture)
			c.call(t, "session_start", map[string]any{"workspace": fixture}, toolTimeout)

			path := filepath.Join(fixture, tc.file)

			_, isErr := callResult(t, c, "edit_file", map[string]any{
				"file_path": path,
				"edits":     []map[string]any{{"old_string": tc.oldString, "new_string": tc.newString}},
			}, toolTimeout)
			if !isErr {
				t.Fatalf("%s: strict mode did not reject an unread edit on %s", tc.tool, path)
			}

			readOut := c.call(t, tc.tool, tc.readArgs(path), readTimeout)
			mtime := extractAnyMtime(t, readOut)

			editOut := c.call(t, "edit_file", map[string]any{
				"file_path":      path,
				"edits":          []map[string]any{{"old_string": tc.oldString, "new_string": tc.newString}},
				"expected_mtime": mtime,
			}, toolTimeout)
			assertContains(t, tc.tool+": edit after read via "+tc.tool, editOut, "applied 1 edit")
		})
	}
}

// ─── transaction rollback ────────────────────────────────────────────────────

// TestSmoke_TransactionRollback proves transaction_apply is all-or-nothing: a
// transaction whose second op cannot match leaves the first op's file untouched.
func TestSmoke_TransactionRollback(t *testing.T) {
	plumbBin := buildPlumb(t)
	fixture := makeBareFixture(t)
	a := filepath.Join(fixture, "a.txt")
	b := filepath.Join(fixture, "b.txt")
	if err := os.WriteFile(a, []byte("A1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("B1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), tierDTimeout)
	defer cancel()

	c := newMCPClient(t, ctx, plumbBin, mkTmpHome(t), fixture)
	c.initialize(t, fixture)
	c.call(t, "session_start", map[string]any{"workspace": fixture}, toolTimeout)

	out, isErr := callResult(t, c, "transaction_apply", map[string]any{
		"dirty_ok": true,
		"operations": []map[string]any{
			{"file_path": a, "edits": []map[string]any{{"old_string": "A1", "new_string": "A2"}}},
			{"file_path": b, "edits": []map[string]any{{"old_string": "NOPE", "new_string": "B2"}}},
		},
	}, toolTimeout)
	if !isErr {
		t.Fatalf("transaction with an unmatchable op should fail; got: %s", out)
	}

	got, err := os.ReadFile(a)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "A1") || strings.Contains(string(got), "A2") {
		t.Errorf("a.txt was mutated despite rollback: %q", got)
	}
}

// ─── git tiers ───────────────────────────────────────────────────────────────

// TestSmoke_GitTiers walks the git tool up its permission tiers in a throwaway
// repo: git_init creates it, add+commit are write-tier, and reset --hard is a
// destructive-tier op gated by PLUMB_GIT_ALLOW_DESTRUCTIVE + confirm.
func TestSmoke_GitTiers(t *testing.T) {
	plumbBin := buildPlumb(t)
	fixture := makeBareFixture(t) // no git yet — git_init creates it
	ctx, cancel := context.WithTimeout(context.Background(), tierDTimeout)
	defer cancel()

	c := newMCPClient(t, ctx, plumbBin, mkTmpHome(t), fixture, "PLUMB_GIT_ALLOW_DESTRUCTIVE=true")
	c.initialize(t, fixture)
	c.call(t, "session_start", map[string]any{"workspace": fixture}, toolTimeout)

	initOut := c.call(t, "git_init", map[string]any{"path": fixture}, toolTimeout)
	assertContains(t, "git_init", initOut, "initialised git repository")
	setGitIdentity(t, fixture) // local identity; daemon's HOME is isolated

	hello := filepath.Join(fixture, "hello.txt")
	c.call(t, "write_file", map[string]any{"file_path": hello, "content": "v1\n"}, toolTimeout)
	c.call(t, "git", map[string]any{"subcommand": "add", "files": []string{"hello.txt"}, "repo": fixture}, toolTimeout)
	c.call(t, "git", map[string]any{"subcommand": "commit", "message": "add hello", "repo": fixture}, toolTimeout)

	// Dirty the committed file, then discard the change with a destructive reset.
	c.call(t, "write_file", map[string]any{"file_path": hello, "content": "v2\n"}, toolTimeout)
	out, isErr := callResult(t, c, "git", map[string]any{
		"subcommand": "reset", "args": []string{"--hard", "HEAD"}, "confirm": true, "repo": fixture,
	}, toolTimeout)
	if isErr {
		t.Fatalf("destructive reset (allowed + confirmed) should succeed; got: %s", out)
	}

	got, err := os.ReadFile(hello)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != "v1" {
		t.Errorf("reset --hard did not restore committed content: got %q, want \"v1\"", got)
	}
}
