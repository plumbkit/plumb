package cli

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/tui"
)

// TestClientConfigThatWillNotParseFails pins the one config fault every doctor
// check used to miss. A client config with broken syntax is unloadable — the
// client itself cannot read it, so plumb is not running there at all — yet the
// extractor's error was swallowed into a clean "registered" pass, and the Kimi
// tool-surface check stayed silent for the same reason. `plumb doctor` reported
// a fully healthy machine for a file no client could load.
func TestClientConfigThatWillNotParseFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	// Truncated mid-object: contains the word "plumb" (so checkOneClient gets
	// past its registered-at-all scan) and is not valid JSON.
	if err := os.WriteFile(path, []byte(`{"mcpServers": {"plumb": {"command": "/usr/local/bin/plumb"`), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	target := setupTarget{
		use: "kimi-code", name: "Kimi Code",
		pathFn:    func() (string, error) { return path, nil },
		extractFn: claudeDesktopCommandExtractor,
	}

	res := classifyClientBinary(target, path, "/usr/local/bin/plumb")
	if res.ok {
		t.Errorf("an unparseable config must fail the check, got %+v", res)
	}
	if res.fix == "" {
		t.Error("the failure must carry a fix line — the user has to know which file to repair")
	}
	if !strings.Contains(res.detail, "parse") {
		t.Errorf("detail should say the config cannot be parsed: %q", res.detail)
	}

	if got := checkOneClient(target, "/usr/local/bin/plumb"); got.ok {
		t.Errorf("checkOneClient must surface the parse failure, got %+v", got)
	}
}

// TestCheckDaemon_ReportsVersionMismatch pins doctor's visibility of the
// daemon/proxy version lag: the reconnect note now warns only once per daemon
// version per proxy, so `plumb doctor` is where the mismatch stays visible on
// demand. A daemon reporting a different build must fail the version check;
// the matching build must pass it.
func TestCheckDaemon_ReportsVersionMismatch(t *testing.T) {
	// Redirect the runtime dir (socket + version file) via HOME. The socket
	// path must stay short (the ~104-char unix-domain limit), so build the
	// temp home under /tmp rather than using the deeply nested t.TempDir().
	home, err := os.MkdirTemp("/tmp", "plumb-doctor-") //nolint:usetesting // t.TempDir() nests too deep: the unix socket path inside would exceed the ~104-char domain limit
	if err != nil {
		t.Fatalf("creating temp home: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	// XDG_RUNTIME_DIR outranks both of the below, so clear it FIRST. Without
	// this the test resolves the developer's real /run/user/$UID: it rewrites
	// the live daemon's plumb.version, and once a daemon is actually listening
	// there the net.Listen below fails with "address already in use".
	t.Setenv("XDG_RUNTIME_DIR", "")
	// On Linux os.UserCacheDir prefers XDG_CACHE_HOME over $HOME/.cache, so
	// redirect it too — otherwise a CI environment that sets it would point
	// the test at the developer's real runtime dir.
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))

	// checkDaemon dials the socket before reading the version file, so stand a
	// listener up at the (redirected) path.
	ln, err := net.Listen("unix", daemonSocketPath())
	if err != nil {
		t.Fatalf("listening on daemon socket: %v", err)
	}
	defer func() { _ = ln.Close() }()

	versionResult := func() checkResult {
		t.Helper()
		for _, r := range checkDaemon() {
			if r.name == "version" {
				return r
			}
		}
		t.Fatal("checkDaemon returned no version result")
		return checkResult{}
	}

	if err := os.WriteFile(daemonVersionPath(), []byte("0.0.0-stale"), 0o600); err != nil {
		t.Fatalf("writing version file: %v", err)
	}
	stale := versionResult()
	if stale.ok {
		t.Errorf("a daemon version differing from the binary (%s) must fail the check, got %+v", Version, stale)
	}
	if !strings.Contains(stale.detail, "0.0.0-stale") || !strings.Contains(stale.detail, Version) {
		t.Errorf("the mismatch detail must name both versions, got %q", stale.detail)
	}
	if stale.fix == "" {
		t.Error("the mismatch must carry a fix line")
	}

	if err := os.WriteFile(daemonVersionPath(), []byte(Version), 0o600); err != nil {
		t.Fatalf("writing version file: %v", err)
	}
	if current := versionResult(); !current.ok {
		t.Errorf("a matching daemon version must pass the check, got %+v", current)
	}
}

func TestJsonCheckResultMarshaling(t *testing.T) {
	checks := []checkResult{
		{name: "socket", ok: true, detail: "~/.cache/plumb/plumb.sock", fix: ""},
		{name: "version", ok: false, detail: "running 0.7.0, binary is 0.7.1", fix: "run `plumb stop`"},
	}
	out := make([]jsonCheckResult, len(checks))
	for i, c := range checks {
		out[i] = jsonCheckResult{Name: c.name, OK: c.ok, Detail: c.detail, Fix: c.fix}
	}
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded []jsonCheckResult
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(decoded) != 2 {
		t.Fatalf("want 2 results, got %d", len(decoded))
	}
	if decoded[0].Name != "socket" || !decoded[0].OK {
		t.Errorf("first result: got %+v", decoded[0])
	}
	if decoded[1].Name != "version" || decoded[1].OK || decoded[1].Fix == "" {
		t.Errorf("second result: got %+v", decoded[1])
	}
}

func TestParseJavaMajorVersion(t *testing.T) {
	cases := []struct {
		line string
		want int
	}{
		// Modern format: "openjdk 21.0.3 ..."
		{`openjdk 21.0.3 2024-04-16`, 21},
		{`openjdk 17.0.1 2021-10-19`, 17},
		{`openjdk 11.0.22 2024-01-16`, 11},
		// java prefix instead of openjdk
		{`java 21.0.3 2024-04-16`, 21},
		// Legacy 1.x format: "java version "1.8.0_292""
		{`java version "1.8.0_292"`, 8},
		{`java version "1.7.0_80"`, 7},
		// Version string with surrounding quotes (some JVM output styles)
		{`openjdk version "21.0.3" 2024-04-16`, 21},
		{`openjdk version "17.0.9" 2023-10-17`, 17},
		// Distro line-1 strings (vendor appears on line 2+, major must read from line 1):
		// Eclipse Temurin, Amazon Corretto, Microsoft Build of OpenJDK.
		{`openjdk 21.0.3 2024-04-16 LTS`, 21},
		{`openjdk 17.0.11 2024-04-16 LTS`, 17},
		{`openjdk 11.0.23 2024-04-16 LTS`, 11},
		// Unrecognised / empty
		{``, 0},
		{`some random text`, 0},
		{`GraalVM CE 21.0.0`, 21},
	}
	for _, tc := range cases {
		got := parseJavaMajorVersion(tc.line)
		if got != tc.want {
			t.Errorf("parseJavaMajorVersion(%q) = %d, want %d", tc.line, got, tc.want)
		}
	}
}

// TestPrintChecksBranchesSubChecksUnderParent pins the branch layout: a sub
// row folds beneath its parent, the name column is sized by parent rows alone,
// and every branch detail line lands in the parent's detail column. The fold is
// driven by the explicit subOf link — never by the "(…)" in a name — which is
// what keeps Language Servers' "go (live)" rows top-level.
func TestPrintChecksBranchesSubChecksUnderParent(t *testing.T) {
	capture := func(checks []checkResult) (raw string, plain []string) {
		t.Helper()
		raw = captureStdout(t, func() { printChecks(checks) })
		plain = strings.Split(strings.TrimSuffix(stripANSI(raw), "\n"), "\n")
		return raw, plain
	}
	// Column, not byte offset: the markers and branch glyphs are multi-byte
	// runes, and the layout arithmetic is in visible columns.
	col := func(line, sub string) int {
		i := strings.Index(line, sub)
		if i < 0 {
			return -1
		}
		return len([]rune(line[:i]))
	}

	t.Run("a clean branch aligns with the parent's columns", func(t *testing.T) {
		_, plain := capture([]checkResult{
			{name: "Claude Desktop", ok: true, detail: "~/Library/Application Support/Claude/claude_desktop_config.json"},
			{
				name: "Claude Desktop (extra profiles)", subOf: "Claude Desktop", ok: true,
				detail: "1 extra profile(s) current\n(heuristic — not an Anthropic-documented path)",
			},
			{name: "Antigravity Desktop", ok: true, detail: "~/gemini/antigravity/mcp/plumb.json"},
		})
		// nameW comes from the widest parent ("Antigravity Desktop", 19), so the
		// detail column is 7+19 = 26.
		if len(plain) != 4 {
			t.Fatalf("want parent, branch, branch tail, parent — got %d lines:\n%s", len(plain), strings.Join(plain, "\n"))
		}
		if col(plain[0], "~/Library") != 26 || col(plain[3], "~/gemini") != 26 {
			t.Errorf("parent detail must sit at column 26:\n%q\n%q", plain[0], plain[3])
		}
		if !strings.HasPrefix(plain[1], "     ╰─ Extra profiles") {
			t.Errorf("branch line = %q, want the glyph at the parent's name column with the derived label", plain[1])
		}
		if col(plain[1], "1 extra profile(s) current") != 26 {
			t.Errorf("branch first detail must sit in the parent's detail column (26), got %q", plain[1])
		}
		if want := strings.Repeat(" ", 26) + "╰─ (heuristic — not an Anthropic-documented path)"; plain[2] != want {
			t.Errorf("branch tail = %q, want %q", plain[2], want)
		}
	})

	t.Run("a stacked detail keeps middle lines plain and closes on the last", func(t *testing.T) {
		_, plain := capture([]checkResult{
			{name: "Antigravity Desktop", ok: true, detail: "p"},
			{name: "Kimi Code", ok: true, detail: "k"},
			{name: "Kimi Code (tool surface)", subOf: "Kimi Code", ok: true, detail: "head\nmiddle\ntail"},
		})
		if len(plain) != 5 {
			t.Fatalf("want parent, parent, branch + two stacked lines — got %d lines:\n%s", len(plain), strings.Join(plain, "\n"))
		}
		if plain[2] != "     ╰─ Tool surface      head" {
			t.Errorf("branch line = %q, want the first detail at column 26", plain[2])
		}
		if plain[3] != strings.Repeat(" ", 26)+"middle" {
			t.Errorf("middle line = %q, want it plain at the detail column", plain[3])
		}
		if want := strings.Repeat(" ", 26) + "╰─ tail"; plain[4] != want {
			t.Errorf("last line = %q, want %q", plain[4], want)
		}
	})

	t.Run("a too-long detail flows as a hanging paragraph with the command set off", func(t *testing.T) {
		lines := subDetailLines("no client-side allowlist, so Codex loads whatever plumb advertises — `plumb setup codex --lean` writes an enabled_tools allowlist trimming it to the 21-tool lean set\n(every tool under the default profile)", 26, 100)
		want := []subDetailLine{
			{text: "no client-side allowlist, so Codex loads whatever plumb advertises"},
			{text: "`plumb setup codex --lean`"},
			{text: "writes an enabled_tools allowlist trimming it to the 21-tool lean set", off: 3},
			{text: "(every tool under the default profile)", off: 3},
		}
		if len(lines) != len(want) {
			t.Fatalf("subDetailLines gave %d lines, want %d:\n%+v", len(lines), len(want), lines)
		}
		for i := range want {
			if lines[i] != want[i] {
				t.Errorf("line %d = %+v, want %+v", i, lines[i], want[i])
			}
		}
	})

	t.Run("a fitted stacked detail keeps its shape and closes on a glyph", func(t *testing.T) {
		lines := subDetailLines("head\nmiddle\ntail", 26, 100)
		want := []subDetailLine{
			{text: "head"},
			{text: "middle"},
			{text: "tail", close: true},
		}
		if len(lines) != len(want) {
			t.Fatalf("subDetailLines gave %d lines, want %d:\n%+v", len(lines), len(want), lines)
		}
		for i := range want {
			if lines[i] != want[i] {
				t.Errorf("line %d = %+v, want %+v", i, lines[i], want[i])
			}
		}
	})

	t.Run("an attention sub carries its fix at the detail column", func(t *testing.T) {
		raw, plain := capture([]checkResult{
			{name: "Claude Desktop", ok: true, detail: "~/Library/Application Support/Claude/claude_desktop_config.json"},
			{name: "Antigravity Desktop", ok: true, detail: "~/gemini/antigravity/mcp/plumb.json"},
			{
				name: "Claude Desktop (extra profiles)", subOf: "Claude Desktop", ok: true, warn: true,
				detail: "stale plumb binary in: ~/Library/Application Support/Claude.2",
				fix:    "run `plumb setup claude-desktop` to repoint every detected profile",
			},
		})
		if col(plain[1], "stale plumb binary") != 26 {
			t.Errorf("branch detail must sit at column 26, got %q", plain[1])
		}
		// The detail overflows the 80-column fallback here, so it flows: the
		// path's tail moves onto the hanging indent before the fix line.
		if want := strings.Repeat(" ", 29) + "Support/Claude.2"; plain[2] != want {
			t.Errorf("wrapped continuation = %q, want %q", plain[2], want)
		}
		if want := strings.Repeat(" ", 26) + "→ run `plumb setup claude-desktop` to repoint every detected profile"; plain[3] != want {
			t.Errorf("fix line = %q, want %q", plain[3], want)
		}
		// Structure carries the hint colour whatever the status: the same Render
		// call as the printer, so this holds whether or not the profile emits colour.
		if !strings.Contains(raw, tui.HintStyle.Render("╰─ Extra profiles")) {
			t.Errorf("glyph and label must render in HintStyle, got:\n%s", raw)
		}
	})

	t.Run("a sub whose parent row is absent renders top-level", func(t *testing.T) {
		_, plain := capture([]checkResult{
			{name: "Claude Desktop (extra profiles)", subOf: "Claude Desktop", ok: true, detail: "1 extra profile(s) current"},
		})
		if len(plain) != 1 || !strings.HasPrefix(plain[0], "  ✓  Claude Desktop (extra profiles)") {
			t.Errorf("an orphaned sub must render top-level rather than vanish, got %q", strings.Join(plain, "\n"))
		}
	})
}

// TestClaudeDesktopExtraProfilesResultBranchesUnderClaudeDesktop pins the
// parent link and the clean-pass detail shape: the heuristic caveat moved onto
// its own line so the rendered branch reads "N extra profile(s) current" with
// the caveat tucked beneath it.
func TestClaudeDesktopExtraProfilesResultBranchesUnderClaudeDesktop(t *testing.T) {
	res := claudeDesktopExtraProfilesResult(2, nil, nil)
	if res.subOf != claudeDesktopTarget.name {
		t.Errorf("subOf = %q, want the parent row %q — an unlinked sub renders as a stray top-level row", res.subOf, claudeDesktopTarget.name)
	}
	if want := "2 extra profile(s) current\n(heuristic — not an Anthropic-documented path)"; res.detail != want {
		t.Errorf("clean-pass detail = %q, want %q", res.detail, want)
	}
	if res.name != "Claude Desktop (extra profiles)" {
		t.Errorf("name = %q — the full name is the --json contract and must not change", res.name)
	}
}

// TestCheckMCPClients_TopLevelClientsAlphabetical pins the section's client
// order. allSetupClients is deliberately NOT alphabetical (the four originals
// first, for the setup tables) and doctor used to inherit that order; the
// section reads better sorted, so checkMCPClients sorts a copy and this test
// holds it there. Sub rows are excluded — they fold under their parent wherever
// it lands.
func TestCheckMCPClients_TopLevelClientsAlphabetical(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KIMI_CODE_HOME", filepath.Join(home, ".kimi-code"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))

	var names []string
	for _, r := range checkMCPClients() {
		if r.subOf == "" {
			names = append(names, r.name)
		}
	}
	if len(names) != len(allSetupClients()) {
		t.Fatalf("every client earns exactly one top-level row, got %d rows for %d clients", len(names), len(allSetupClients()))
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Errorf("top-level client rows must ascend: %q comes before %q", names[i-1], names[i])
		}
	}
}
