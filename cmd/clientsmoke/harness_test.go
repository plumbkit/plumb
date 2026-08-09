//go:build clients || clients_e2e || clients_conformance

// Package clientsmoke is an on-demand integration harness that verifies plumb
// works as an MCP server through each supported client CLI, driven
// non-interactively (no TUI ever blocks).
//
// Three tiers, selected by build tag:
//
//   - -tags=clients      (TestClientsConnect): the auth-free CONNECTION tier.
//     For each client with an auth-free connecting probe (e.g. `gemini mcp
//     list`), it runs `plumb setup <client>` into an isolated HOME, runs the
//     probe, and asserts plumb recorded a client session — i.e. the CLI
//     completed the MCP `initialize` handshake. Deterministic, free, no keys.
//
//   - -tags=clients_e2e  (TestClientsAuth): the LLM AUTH tier. For each client
//     whose API key is present in the environment, it drives a headless prompt
//     that forces a plumb tool call and asserts a `tool_calls` row landed in
//     plumb's stats DB — proof the agent actually invoked a plumb tool through
//     the model. Skips any client whose key is unset. Costs money.
//
//   - -tags=clients_conformance (Codex and OpenCode): the local deterministic
//     tier. Scripted loopback providers direct an exact sequence through each
//     real client and separately assert discovery, invocation,
//     refusal recovery, daemon reconnect, replay, session identity, and stats.
//
// All tiers share this file: the binary build (TestMain), the per-test
// isolation (own HOME + XDG dirs, like cmd/smoke), the client table, and the
// plumb-side readers (session files and stats.db) that supply the success
// signal independent of each CLI's output format.
//
// Prerequisites: the client binaries on PATH (install with
// scripts/install-clients.sh); a per-client binary that's absent is skipped.
// Run:
//
//	make clients-test                 # connection tier
//	make clients-test-conformance     # deterministic real-client tier
//	make clients-test-auth            # auth tier (needs API keys)
package clientsmoke

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/sqlitex"
)

// plumbBin is the freshly-built plumb binary, shared across all subtests. Built
// once in TestMain so the client configs we write point at a real executable.
var plumbBin string

// realHome is the developer's real HOME, captured before any per-test override.
// Clients whose code is installed HOME-relative (hermes via pip --user) resolve
// their modules against HOME, so a probe under the isolated HOME needs to point
// back at the real install (see the hermes probeEnv / PYTHONUSERBASE).
var realHome string

func TestMain(m *testing.M) {
	realHome = os.Getenv("HOME")
	root, err := findRepoRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "clientsmoke: locate repo root:", err)
		os.Exit(1)
	}
	dir, err := os.MkdirTemp("", "clientsmoke-bin")
	if err != nil {
		fmt.Fprintln(os.Stderr, "clientsmoke: temp dir:", err)
		os.Exit(1)
	}
	bin := filepath.Join(dir, "plumb")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-o", bin, "./cmd/plumb/")
	build.Dir = root
	if out, berr := build.CombinedOutput(); berr != nil {
		fmt.Fprintf(os.Stderr, "clientsmoke: build plumb: %v\n%s", berr, out)
		os.Exit(1)
	}
	plumbBin = bin
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// findRepoRoot walks up from the working directory to the module root.
func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod found above %s", dir)
		}
		dir = parent
	}
}

// ─── client table ────────────────────────────────────────────────────────────

// clientSpec describes one client CLI and how to drive it in each tier. A
// single table feeds both TestClientsConnect and TestClientsAuth.
type clientSpec struct {
	name      string   // display name; also the messages key
	binary    string   // executable looked up on PATH (skip if absent)
	setupArgs []string // args to the plumb binary, e.g. {"setup", "gemini"}

	// Connection tier (-tags=clients).
	connect     bool                                                      // has an auth-free connecting probe
	connectArgs []string                                                  // probe argv, e.g. {"mcp", "list"}
	connectSkip string                                                    // reason logged when connect == false
	prep        func(t *testing.T, tmpHome, fixture string, env []string) // optional pre-probe setup (folder trust, MCP approval)
	probeEnv    func(realHome string) []string                            // extra env layered onto the probe/prompt, both tiers (e.g. PYTHONUSERBASE)
	wantOut     []string                                                  // advisory substrings expected in probe output

	// Auth tier (-tags=clients_e2e).
	authKeys   []string                     // acceptable API-key env vars; first set wins. empty ⇒ unsupported
	authEnv    func(key string) []string    // extra env (provider/model selection) layered on the inherited env
	promptArgs func(prompt string) []string // headless prompt argv, with auto-approve flags
}

// clientSpecs is the single source of truth for both tiers. Connection-tier
// support and auth-tier support are independent: codex/crush/goose/auggie have
// no auth-free probe (connect=false) but are still drivable in the auth tier.
func clientSpecs() []clientSpec {
	geminiPrompt := func(p string) []string { return []string{"-p", p, "--yolo"} }
	return []clientSpec{
		{
			name: "gemini", binary: "gemini", setupArgs: []string{"setup", "gemini"},
			connect: true, connectArgs: []string{"mcp", "list"}, prep: seedFolderTrust(".gemini"), wantOut: []string{"plumb"},
			authKeys: []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}, promptArgs: geminiPrompt,
		},
		{
			name: "qwen", binary: "qwen", setupArgs: []string{"setup", "qwen"},
			connect: true, connectArgs: []string{"mcp", "list"}, prep: seedFolderTrust(".qwen"), wantOut: []string{"plumb"},
			authKeys: []string{"OPENAI_API_KEY"},
			authEnv: func(k string) []string {
				return []string{"OPENAI_API_KEY=" + k, "OPENAI_BASE_URL=https://api.openai.com/v1", "OPENAI_MODEL=gpt-4o"}
			},
			promptArgs: geminiPrompt,
		},
		{
			name: "opencode", binary: "opencode", setupArgs: []string{"setup", "opencode"},
			connect: true, connectArgs: []string{"mcp", "list"}, wantOut: []string{"plumb"},
			authKeys:   []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY"},
			promptArgs: func(p string) []string { return []string{"run", p} },
		},
		{
			// cursor-agent caches MCP tool lists (account/cloud-backed): its headless
			// `mcp list-tools` returns plumb's tools WITHOUT spawning `plumb serve`, so
			// no probe provably reaches a fresh plumb, and `mcp list` is separately
			// buggy (reports "needs approval" even when approved). No auth-free
			// connection signal — still drivable in the auth tier (enable prep kept).
			name: "cursor-agent", binary: "cursor-agent", setupArgs: []string{"setup", "cursor"},
			connect: false, connectSkip: "cursor-agent caches MCP tool lists and does not reconnect in headless mode, so no probe provably reaches a fresh plumb (plus documented `mcp list` approval bugs)",
			prep:       enableCursorMCP,
			authKeys:   []string{"CURSOR_API_KEY"},
			promptArgs: func(p string) []string { return []string{"-p", p, "--force"} },
		},
		{
			name: "hermes", binary: "hermes", setupArgs: []string{"setup", "hermes"},
			connect: true, connectArgs: []string{"mcp", "test", "plumb"}, wantOut: []string{"plumb"},
			probeEnv:   func(home string) []string { return []string{"PYTHONUSERBASE=" + filepath.Join(home, ".local")} },
			authKeys:   []string{"OPENAI_API_KEY"},
			authEnv:    func(k string) []string { return []string{"OPENAI_API_KEY=" + k} },
			promptArgs: func(p string) []string { return []string{"-z", p} },
		},
		{
			name: "claude-code", binary: "claude", setupArgs: []string{"setup", "claude-code"},
			connect: true, connectArgs: []string{"mcp", "list"}, wantOut: []string{"plumb"},
			authKeys:   []string{"ANTHROPIC_API_KEY"},
			promptArgs: func(p string) []string { return []string{"-p", p, "--dangerously-skip-permissions"} },
		},
		{
			name: "codex", binary: "codex", setupArgs: []string{"setup", "codex"},
			connect: false, connectSkip: "`codex mcp list` is a static config/auth echo — no MCP handshake (use the auth tier)",
			prep:     codexLogin,
			authKeys: []string{"OPENAI_API_KEY"},
			// No sandbox: workspace-write cancels the MCP tool call (plumb serve can't
			// reach its daemon socket); bypass lets the handshake + tool call complete.
			promptArgs: func(p string) []string {
				return []string{"exec", "--dangerously-bypass-approvals-and-sandbox", p}
			},
		},
		{
			name: "auggie", binary: "auggie", setupArgs: []string{"setup", "augment"},
			connect: false, connectSkip: "`auggie mcp list` is static; `auggie tools list` needs an Augment token",
			authKeys:   []string{"AUGMENT_API_TOKEN", "AUGMENT_SESSION_AUTH"},
			promptArgs: func(p string) []string { return []string{"--print", p} },
		},
		{
			name: "crush", binary: "crush", setupArgs: []string{"setup", "crush"},
			connect: false, connectSkip: "Crush surfaces MCP status only in its TUI; no non-interactive connecting probe",
			authKeys: []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY"},
			// `crush run` is non-interactive and auto-executes; it has no --yolo flag.
			promptArgs: func(p string) []string { return []string{"run", p} },
		},
		{
			name: "goose", binary: "goose", setupArgs: []string{"setup", "goose"},
			connect: false, connectSkip: "Goose loads MCP extensions only inside a session; no auth-free connecting probe",
			authKeys: []string{"OPENAI_API_KEY"},
			authEnv: func(k string) []string {
				return []string{"GOOSE_PROVIDER=openai", "GOOSE_MODEL=gpt-4o", "OPENAI_API_KEY=" + k}
			},
			promptArgs: func(p string) []string { return []string{"run", "--no-session", "-t", p} },
		},
	}
}

// ─── isolation ───────────────────────────────────────────────────────────────

// mkTmpHome creates an isolated HOME under /tmp (kept short for macOS's 104-byte
// Unix-socket limit), removed at test end.
func mkTmpHome(t *testing.T) string {
	t.Helper()
	tmpHome, err := os.MkdirTemp("/tmp", "plcl")
	if err != nil {
		t.Fatal("create tmpHome:", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmpHome) })
	t.Cleanup(func() {
		if t.Failed() {
			preserveFailureEvidence(t, tmpHome)
		}
	})
	return tmpHome
}

// isolatedEnv overrides HOME, every XDG base dir, and client-specific config
// roots so the daemon a client spawns uses a fresh socket / data / config tree —
// leaving the developer's real daemon and client configs untouched. Session-
// manager recovery companions are scrubbed too, or plumb correctly interprets
// the deliberate temp XDG roots as a hijack and restores the real directories.
// All other environment (notably API keys) is inherited for the auth tier.
func isolatedEnv(tmpHome string, extra ...string) []string {
	base := os.Environ()
	out := make([]string, 0, len(base)+9)
	for _, e := range base {
		switch {
		case strings.HasPrefix(e, "HOME="),
			strings.HasPrefix(e, "XDG_CONFIG_HOME="),
			strings.HasPrefix(e, "XDG_CACHE_HOME="),
			strings.HasPrefix(e, "XDG_DATA_HOME="),
			strings.HasPrefix(e, "XDG_STATE_HOME="),
			strings.HasPrefix(e, "TSM_ORIG_XDG_"),
			strings.HasPrefix(e, "CODEX_HOME="),
			strings.HasPrefix(e, "PLUMB_STRICT_EDITS="),
			strings.HasPrefix(e, "PLUMB_TOOLS_PROFILE="):
			continue
		default:
			out = append(out, e)
		}
	}
	out = append(out,
		"HOME="+tmpHome,
		"XDG_CONFIG_HOME="+filepath.Join(tmpHome, ".config"),
		"XDG_CACHE_HOME="+filepath.Join(tmpHome, ".cache"),
		"XDG_DATA_HOME="+filepath.Join(tmpHome, ".local", "share"),
		"XDG_STATE_HOME="+filepath.Join(tmpHome, ".local", "state"),
		"CODEX_HOME="+filepath.Join(tmpHome, ".codex"),
	)
	return append(out, extra...)
}

// conformanceEnv removes provider credentials after applying the ordinary client
// isolation. The deterministic tier must never expose a developer's credentials
// to a real client process, even though the authenticated tier intentionally
// inherits them through isolatedEnv.
func conformanceEnv(tmpHome string, extra ...string) []string {
	env := isolatedEnv(tmpHome, extra...)
	out := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if isCredentialEnvKey(key) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func isCredentialEnvKey(key string) bool {
	upper := strings.ToUpper(key)
	return strings.HasSuffix(upper, "_API_KEY") ||
		strings.HasSuffix(upper, "_TOKEN") ||
		strings.HasSuffix(upper, "_SECRET") ||
		strings.HasSuffix(upper, "_PASSWORD") ||
		strings.HasSuffix(upper, "_CREDENTIALS") ||
		strings.HasSuffix(upper, "_ACCESS_KEY_ID")
}

func TestIsolatedEnv_ScrubsCodexHome(t *testing.T) {
	t.Setenv("CODEX_HOME", "/real/codex")
	tmpHome := t.TempDir()
	env := isolatedEnv(tmpHome)

	want := "CODEX_HOME=" + filepath.Join(tmpHome, ".codex")
	seen := 0
	for _, e := range env {
		if strings.HasPrefix(e, "CODEX_HOME=") {
			seen++
			if e != want {
				t.Fatalf("CODEX_HOME = %q, want %q", e, want)
			}
		}
	}
	if seen != 1 {
		t.Fatalf("CODEX_HOME entries = %d, want 1", seen)
	}
}

func TestIsolatedEnv_ScrubsSessionManagerOrigins(t *testing.T) {
	t.Setenv("TSM_ORIG_XDG_DATA_HOME", "/real/data")
	for _, e := range isolatedEnv(t.TempDir()) {
		if strings.HasPrefix(e, "TSM_ORIG_XDG_") {
			t.Fatalf("session-manager recovery companion leaked into isolated env: %q", e)
		}
	}
}

func TestIsolatedEnv_ReplacesPlumbPolicyOverrides(t *testing.T) {
	t.Setenv("PLUMB_STRICT_EDITS", "0")
	t.Setenv("PLUMB_TOOLS_PROFILE", "lean")
	env := isolatedEnv(t.TempDir(), "PLUMB_STRICT_EDITS=1", "PLUMB_TOOLS_PROFILE=full")

	for _, want := range []string{"PLUMB_STRICT_EDITS=1", "PLUMB_TOOLS_PROFILE=full"} {
		seen := 0
		for _, entry := range env {
			if strings.HasPrefix(entry, strings.SplitN(want, "=", 2)[0]+"=") {
				seen++
				if entry != want {
					t.Fatalf("policy override = %q, want %q", entry, want)
				}
			}
		}
		if seen != 1 {
			t.Fatalf("%s entries = %d, want 1", want, seen)
		}
	}
}

func TestConformanceEnv_ScrubsProviderCredentials(t *testing.T) {
	for key, value := range map[string]string{
		"OPENAI_API_KEY":                 "real-openai-key",
		"ANTHROPIC_AUTH_TOKEN":           "real-anthropic-token",
		"AWS_SECRET_ACCESS_KEY":          "real-aws-secret",
		"AWS_ACCESS_KEY_ID":              "real-aws-id",
		"GOOGLE_APPLICATION_CREDENTIALS": "/real/google-credentials.json",
	} {
		t.Setenv(key, value)
	}
	t.Setenv("CLIENTSMOKE_INNOCUOUS", "preserved")

	env := conformanceEnv(t.TempDir(), "OPENAI_API_KEY=placeholder")
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if isCredentialEnvKey(key) {
			t.Fatalf("credential leaked into conformance environment: %s", key)
		}
	}
	if !slices.Contains(env, "CLIENTSMOKE_INNOCUOUS=preserved") {
		t.Fatal("conformance environment removed an unrelated variable")
	}
}

func TestDaemonPIDPathsCoverClientEnvironmentFiltering(t *testing.T) {
	tmpHome := t.TempDir()
	got := daemonPIDPaths(tmpHome)
	want := []string{filepath.Join(tmpHome, ".cache", "plumb", "plumb.pid")}
	if runtime.GOOS == "darwin" {
		want = append(want, filepath.Join(tmpHome, "Library", "Caches", "plumb", "plumb.pid"))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("daemonPIDPaths = %q, want %q", got, want)
	}
}

// makeBareFixture creates a temp workspace with just a .plumb/ marker — enough
// for plumb to attach, no language server needed.
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

// ─── plumb-side signals ──────────────────────────────────────────────────────

func dataDirs(tmpHome string) []string {
	dirs := []string{filepath.Join(tmpHome, ".local", "share", "plumb")}
	if runtime.GOOS == "darwin" {
		dirs = append(dirs, filepath.Join(tmpHome, "Library", "Application Support", "plumb"))
	}
	return dirs
}

func daemonPIDPaths(tmpHome string) []string {
	paths := []string{filepath.Join(tmpHome, ".cache", "plumb", "plumb.pid")}
	if runtime.GOOS == "darwin" {
		paths = append(paths, filepath.Join(tmpHome, "Library", "Caches", "plumb", "plumb.pid"))
	}
	return paths
}

func daemonLogPath(tmpHome string) string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(tmpHome, "Library", "Logs", "plumb", "daemon.log")
	}
	return filepath.Join(tmpHome, ".local", "state", "plumb", "daemon.log")
}

// preserveFailureEvidence copies only plumb runtime evidence — never client
// config or auth state — outside the test's temporary HOME before it is removed.
func preserveFailureEvidence(t *testing.T, tmpHome string) {
	t.Helper()
	dst, err := os.MkdirTemp("", "clientsmoke-failure-")
	if err != nil {
		t.Logf("preserve failure evidence: %v", err)
		return
	}

	copied := 0
	if copyEvidenceFile(daemonLogPath(tmpHome), filepath.Join(dst, "daemon.log")) {
		copied++
	}
	for i, src := range daemonPIDPaths(tmpHome) {
		if copyEvidenceFile(src, filepath.Join(dst, fmt.Sprintf("runtime-%d", i), "plumb.pid")) {
			copied++
		}
	}
	for i, dir := range dataDirs(tmpHome) {
		evidenceDir := filepath.Join(dst, fmt.Sprintf("data-%d", i))
		for _, name := range []string{"stats.db", "stats.db-wal", "stats.db-shm"} {
			if copyEvidenceFile(filepath.Join(dir, name), filepath.Join(evidenceDir, name)) {
				copied++
			}
		}
		sessions := filepath.Join(dir, "sessions")
		if entries, readErr := os.ReadDir(sessions); readErr == nil {
			for _, entry := range entries {
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
					continue
				}
				if copyEvidenceFile(filepath.Join(sessions, entry.Name()), filepath.Join(evidenceDir, "sessions", entry.Name())) {
					copied++
				}
			}
		}
	}

	if copied == 0 {
		_ = os.RemoveAll(dst)
		t.Log("no plumb runtime evidence was available to preserve")
		return
	}
	t.Logf("plumb failure evidence preserved at %s (%d files; client credentials excluded)", dst, copied)
}

func copyEvidenceFile(src, dst string) bool {
	data, err := os.ReadFile(src)
	if err != nil {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return false
	}
	return os.WriteFile(dst, data, 0o600) == nil
}

// sessionEvidence is the subset of a plumb session file we assert on.
type sessionEvidence struct {
	ID            string `json:"id"`
	ClientName    string `json:"client_name"`
	ClientVersion string `json:"client_version"`
	Folder        string `json:"folder"`
}

// findClientSession returns the first session file plumb wrote that records a
// client identity — proof a CLI completed the MCP initialize handshake. The
// per-test data dir is fresh, so any such file belongs to this run.
func findClientSession(t *testing.T, tmpHome string) (sessionEvidence, bool) {
	t.Helper()
	for _, dataDir := range dataDirs(tmpHome) {
		dir := filepath.Join(dataDir, "sessions")
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			var s sessionEvidence
			if json.Unmarshal(b, &s) == nil && s.ClientName != "" {
				return s, true
			}
		}
	}
	return sessionEvidence{}, false
}

// countToolCalls reports how many plumb tool calls the stats DB recorded and the
// distinct tool names — the auth-tier proof that the agent drove a plumb tool.
func countToolCalls(t *testing.T, tmpHome string) (int, string) {
	t.Helper()
	total := 0
	var names []string
	for _, dataDir := range dataDirs(tmpHome) {
		n, tools, ok := countToolCallsAt(t, filepath.Join(dataDir, "stats.db"))
		if !ok {
			continue
		}
		total += n
		if tools != "" {
			names = append(names, tools)
		}
	}
	return total, strings.Join(names, ",")
}

func countToolCallsAt(t *testing.T, path string) (int, string, bool) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		return 0, "", false
	}
	// Was `path+"?mode=ro&_busy_timeout=2000"`, which got both halves wrong:
	// modernc ignores the mattn-style _busy_timeout= spelling (leaving it at 0)
	// and ignores mode= entirely on a bare path (leaving the handle writable).
	db, err := sqlitex.OpenReadOnly(path, sqlitex.ReadOnlyOptions{BusyTimeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("open stats.db: %v", err)
	}
	defer db.Close() //nolint:errcheck // best-effort close in a deferred test cleanup
	var n int
	var tools sql.NullString
	if err := db.QueryRow(`SELECT COUNT(*), GROUP_CONCAT(DISTINCT tool) FROM tool_calls`).Scan(&n, &tools); err != nil {
		t.Fatalf("query tool_calls: %v", err)
	}
	return n, tools.String, true
}

// pollToolCalls reads the stats DB until a tool_calls row appears or timeout
// elapses, absorbing the async writer's small commit lag while the daemon stays
// alive.
func pollToolCalls(t *testing.T, tmpHome string, timeout time.Duration) (int, string) {
	t.Helper()
	return pollToolCallsAtLeast(t, tmpHome, 1, timeout)
}

func pollToolCallsAtLeast(t *testing.T, tmpHome string, minimum int, timeout time.Duration) (int, string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		n, tools := countToolCalls(t, tmpHome)
		if n >= minimum || time.Now().After(deadline) {
			return n, tools
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// ─── process helpers ─────────────────────────────────────────────────────────

// runPlumbSetup runs `plumb <setupArgs...>` with the isolated env, writing the
// client's MCP config into the isolated HOME.
func runPlumbSetup(t *testing.T, env []string, args ...string) {
	t.Helper()
	cmd := exec.Command(plumbBin, args...)
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("plumb %v: %v\n%s", args, err, out)
	}
}

// stopDaemon tears down only the daemon recorded in this test's isolated
// runtime directory. Calling `plumb stop` here is unsafe because its pgrep
// fallback deliberately finds every plumb daemon owned by the user.
func stopDaemon(tmpHome string) {
	seen := make(map[int]bool)
	for _, pidPath := range daemonPIDPaths(tmpHome) {
		data, err := os.ReadFile(pidPath)
		if err != nil {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil || pid <= 0 || seen[pid] {
			continue
		}
		seen[pid] = true
		stopDaemonPID(pidPath, pid)
	}
}

func stopDaemonPID(pidPath string, pid int) {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	if err := proc.Signal(os.Interrupt); err != nil {
		return
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(pidPath); os.IsNotExist(err) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	_ = proc.Kill()
}

// seedFolderTrust returns a prep hook that marks the fixture trusted for
// Gemini-family CLIs (gemini, qwen), which otherwise refuse to load stdio MCP
// servers in an untrusted folder. It both disables the folder-trust feature in
// settings.json (preserving the mcpServers plumb setup wrote) and writes an
// explicit trustedFolders.json entry, covering schema variants across versions.
func seedFolderTrust(homeSub string) func(t *testing.T, tmpHome, fixture string, env []string) {
	return func(t *testing.T, tmpHome, fixture string, _ []string) {
		t.Helper()
		dir := filepath.Join(tmpHome, homeSub)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		settings := filepath.Join(dir, "settings.json")
		m := map[string]any{}
		if b, err := os.ReadFile(settings); err == nil {
			_ = json.Unmarshal(b, &m)
		}
		sec, _ := m["security"].(map[string]any)
		if sec == nil {
			sec = map[string]any{}
		}
		sec["folderTrust"] = map[string]any{"enabled": false}
		m["security"] = sec
		writeJSONFile(t, settings, m)
		writeJSONFile(t, filepath.Join(dir, "trustedFolders.json"), map[string]string{fixture: "TRUST_FOLDER"})
	}
}

// enableCursorMCP adds plumb to cursor-agent's local approved-list so the probe
// actually loads it — cursor-agent refuses to load an unapproved MCP server
// (the analogue of gemini's folder trust). Best-effort: a failure is logged, not
// fatal, so the probe still runs and the session assertion reports the truth.
func enableCursorMCP(t *testing.T, _, fixture string, env []string) {
	t.Helper()
	cmd := exec.Command("cursor-agent", "mcp", "enable", "plumb")
	cmd.Env = env
	cmd.Dir = fixture
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Logf("cursor-agent mcp enable plumb (non-fatal): %v\n%s", err, out)
	}
}

// codexLogin authenticates codex from OPENAI_API_KEY. `codex exec` does not read
// the env var — it needs a prior login that writes auth.json into the (isolated)
// CODEX_HOME/~/.codex. Best-effort: a failure is logged, not fatal.
func codexLogin(t *testing.T, _, fixture string, env []string) {
	t.Helper()
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		return
	}
	cmd := exec.Command("codex", "login", "--with-api-key")
	cmd.Env = env
	cmd.Dir = fixture
	cmd.Stdin = strings.NewReader(key)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Logf("codex login --with-api-key (non-fatal): %v\n%s", err, out)
	}
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// firstSetEnv returns the value of the first environment variable in names that
// is set and non-empty, plus the variable name. ok is false if none are set.
func firstSetEnv(names []string) (name, value string, ok bool) {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return n, v, true
		}
	}
	return "", "", false
}

// truncate clips long command output for log readability.
func truncate(b []byte, max int) string {
	s := string(b)
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n…(truncated)"
}
