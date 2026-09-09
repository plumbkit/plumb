package cli

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/clientcaps"
	"github.com/plumbkit/plumb/internal/tools"
)

// leanWriter pairs a --lean-capable client with the writer that manages its
// allowlist, so one table drives both new clients through the whole contract.
// Kimi is not in it: its writer takes a bool, not a leanChoice, because its
// shipped contract has no clearing path (see kimiCodeInto) — setup_kimi_lean_test.go
// walks that one.
type leanWriter struct {
	label  string
	client leanClient
	into   func(cfgPath, plumbBin string, choice leanChoice) (bool, []string, error)
}

func leanWriters() []leanWriter {
	return []leanWriter{
		{"codex", codexLeanClient, codexLeanInto},
		{"gemini", geminiLeanClient, geminiLeanInto},
	}
}

// readLeanPlumbEntry returns the plumb server entry from a client's config,
// decoded through that client's own parser (TOML for Codex, JSON for the rest).
func readLeanPlumbEntry(t *testing.T, c leanClient, path string) map[string]any {
	t.Helper()
	assertNoSentinelOnDisk(t, c, path)
	cfg, err := c.parse(path)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	servers, ok := cfg[c.serversKey].(map[string]any)
	if !ok {
		t.Fatalf("%s missing or not a table in %s: %v", c.serversKey, path, cfg)
	}
	entry, ok := servers["plumb"].(map[string]any)
	if !ok {
		t.Fatalf("plumb entry missing or not a table in %s: %v", path, servers)
	}
	return entry
}

// assertNoSentinelOnDisk is the canary for removeKey, mergeServerEntry's
// "delete this key" value. The merge deletes rather than assigns it, so it never
// reaches a serialiser — but it is now general vocabulary on a helper ten clients
// and three serialisers share, and a sentinel that DID reach one would not fail
// loudly: it marshals as an empty group, producing a silently malformed config
// rather than an error.
//
// It checks the DECODED config, not the bytes. A byte scan for `{}` was the
// obvious first cut and it was a no-op for the one client whose config carries
// unrelated user state: go-toml does not write an empty inline table, it writes a
// table HEADER — `[mcp_servers.plumb.enabled_tools]` — with no braces anywhere.
// An empty map after decoding is what a leak looks like in every format plumb
// writes, so that is what this walks. Every read in these tests runs through
// readLeanPlumbEntry, so it covers every config the writers produce.
func assertNoSentinelOnDisk(t *testing.T, c leanClient, path string) {
	t.Helper()
	cfg, err := c.parse(path)
	if err != nil {
		return // the caller's own read reports an unreadable file better than this can
	}
	assertNoEmptyGroup(t, path, "", cfg)
}

func assertNoEmptyGroup(t *testing.T, path, at string, v any) {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		if list, isList := v.([]any); isList {
			for i, e := range list {
				assertNoEmptyGroup(t, path, fmt.Sprintf("%s[%d]", at, i), e)
			}
		}
		return
	}
	if len(m) == 0 && at != "" {
		t.Errorf("%s: %s decoded to an empty object/table — the removeKey sentinel reached a serialiser",
			path, at)
		return
	}
	for k, e := range m {
		assertNoEmptyGroup(t, path, strings.TrimPrefix(at+"."+k, "."), e)
	}
}

// assertPinnedAllowlist is the load-bearing assertion of this whole feature: the
// written value must equal tools.LeanToolNames() exactly. A client-side
// allowlist is enforced by the CLIENT, so plumb's server-side "bootstrap tools
// are always advertised" guarantee cannot rescue a name the client itself
// filtered out — the pinned set is the only permitted source.
func assertPinnedAllowlist(t *testing.T, c leanClient, path string) {
	t.Helper()
	got := readLeanPlumbEntry(t, c, path)[c.key]
	if !reflect.DeepEqual(got, wantLeanTools()) {
		t.Errorf("%s = %v, want tools.LeanToolNames() = %v", c.key, got, wantLeanTools())
	}
}

func assertNoAllowlist(t *testing.T, c leanClient, path string) {
	t.Helper()
	if v, has := readLeanPlumbEntry(t, c, path)[c.key]; has {
		t.Errorf("%s should be absent, got %v", c.key, v)
	}
}

// TestLeanClientWriters walks the --lean contract for every client whose writer
// takes a leanChoice. The cases that matter most are (b), (d), and (e): (b) is
// the short-circuit defeat — without the lean-aware idempotence predicate,
// --lean would silently do nothing on an already-registered machine, which is
// every existing user; (d) is the clearing path, the symmetric half Kimi lacks;
// (e) is the bulk sweep, which must never strip a key the user set on purpose.
func TestLeanClientWriters(t *testing.T) {
	const bin = "/usr/local/bin/plumb"

	for _, w := range leanWriters() {
		t.Run(w.label, func(t *testing.T) {
			cfgName := filepath.Base(mustPath(t, w.client.pathFn))

			fresh := func(t *testing.T) string {
				t.Helper()
				return filepath.Join(t.TempDir(), cfgName)
			}

			t.Run("fresh config with lean writes the pinned allowlist", func(t *testing.T) {
				path := fresh(t)
				added, preserved, err := w.into(path, bin, leanPin)
				if err != nil {
					t.Fatalf("lean register: %v", err)
				}
				if !added {
					t.Error("expected added=true for a fresh config")
				}
				if len(preserved) != 0 {
					t.Errorf("expected no preserved servers, got %v", preserved)
				}
				if got := readLeanPlumbEntry(t, w.client, path)["command"]; got != bin {
					t.Errorf("command = %v, want %q", got, bin)
				}
				assertPinnedAllowlist(t, w.client, path)
			})

			t.Run("already registered without the key: lean adds it", func(t *testing.T) {
				path := fresh(t)
				if _, _, err := w.into(path, bin, leanKeep); err != nil {
					t.Fatalf("bare register: %v", err)
				}
				assertNoAllowlist(t, w.client, path)

				added, _, err := w.into(path, bin, leanPin)
				if err != nil {
					t.Fatalf("lean register: %v", err)
				}
				if !added {
					t.Error("expected added=true — the same-binary short-circuit must not swallow --lean")
				}
				assertPinnedAllowlist(t, w.client, path)
			})

			t.Run("second lean run is a no-op", func(t *testing.T) {
				path := fresh(t)
				if _, _, err := w.into(path, bin, leanPin); err != nil {
					t.Fatalf("first lean run: %v", err)
				}
				added, _, err := w.into(path, bin, leanPin)
				if err != nil {
					t.Fatalf("second lean run: %v", err)
				}
				if added {
					t.Error("expected added=false on an identical second --lean run")
				}
				assertPinnedAllowlist(t, w.client, path)
			})

			t.Run("a bare re-register clears the key", func(t *testing.T) {
				path := fresh(t)
				if _, _, err := w.into(path, bin, leanPin); err != nil {
					t.Fatalf("lean register: %v", err)
				}
				added, _, err := w.into(path, bin, leanClear)
				if err != nil {
					t.Fatalf("bare re-register: %v", err)
				}
				if !added {
					t.Error("expected added=true — removing the allowlist is a change to the file")
				}
				assertNoAllowlist(t, w.client, path)
				if got := readLeanPlumbEntry(t, w.client, path)["command"]; got != bin {
					t.Errorf("clearing the allowlist must leave the registration intact, command = %v", got)
				}

				added, _, err = w.into(path, bin, leanClear)
				if err != nil {
					t.Fatalf("second bare re-register: %v", err)
				}
				if added {
					t.Error("expected added=false — there is no key left to clear")
				}
			})

			t.Run("a bulk sweep preserves the allowlist", func(t *testing.T) {
				path := fresh(t)
				if _, _, err := w.into(path, bin, leanPin); err != nil {
					t.Fatalf("lean register: %v", err)
				}
				added, _, err := w.into(path, "/new/plumb", leanKeep)
				if err != nil {
					t.Fatalf("bulk repoint: %v", err)
				}
				if !added {
					t.Error("expected added=true — the binary changed")
				}
				if got := readLeanPlumbEntry(t, w.client, path)["command"]; got != "/new/plumb" {
					t.Errorf("command = %v, want the new binary", got)
				}
				assertPinnedAllowlist(t, w.client, path)
			})

			t.Run("lean replaces a hand-edited allowlist", func(t *testing.T) {
				path := fresh(t)
				custom := map[string]any{w.client.serversKey: map[string]any{"plumb": map[string]any{
					"command":     bin,
					"args":        []string{"serve"},
					w.client.key:  []string{"read_file"},
					"extra_field": "kept",
				}}}
				if err := w.client.write(path, custom); err != nil {
					t.Fatalf("writing custom config: %v", err)
				}
				added, _, err := w.into(path, bin, leanPin)
				if err != nil {
					t.Fatalf("lean over custom: %v", err)
				}
				if !added {
					t.Error("expected added=true — a custom list must be replaced by --lean")
				}
				assertPinnedAllowlist(t, w.client, path)
				if got := readLeanPlumbEntry(t, w.client, path)["extra_field"]; got != "kept" {
					t.Errorf("the merge must preserve unrelated keys on the plumb entry, extra_field = %v", got)
				}
			})

			t.Run("other MCP servers are preserved", func(t *testing.T) {
				path := fresh(t)
				seed := map[string]any{w.client.serversKey: map[string]any{
					"other": map[string]any{"command": "/bin/other"},
				}}
				if err := w.client.write(path, seed); err != nil {
					t.Fatalf("seeding config: %v", err)
				}
				_, preserved, err := w.into(path, bin, leanPin)
				if err != nil {
					t.Fatalf("lean register: %v", err)
				}
				if !reflect.DeepEqual(preserved, []string{"other"}) {
					t.Errorf("preserved = %v, want [other]", preserved)
				}
				assertPinnedAllowlist(t, w.client, path)
			})
		})
	}
}

func mustPath(t *testing.T, fn func() (string, error)) string {
	t.Helper()
	p, err := fn()
	if err != nil {
		t.Fatalf("resolving config path: %v", err)
	}
	return p
}

// TestLeanChoiceOf pins the three-way resolution. The bulk case is the one that
// matters: `plumb setup --all` carries no --lean state, so reading its silence
// as leanClear would strip every allowlist on the machine during a routine
// binary repoint.
func TestLeanChoiceOf(t *testing.T) {
	t.Cleanup(func() { setupAllFlag, setupRepairFlag, setupInstallMissingFlag = false, false, false })

	if got := leanChoiceOf(true); got != leanPin {
		t.Errorf("--lean on a named subcommand = %v, want leanPin", got)
	}
	if got := leanChoiceOf(false); got != leanClear {
		t.Errorf("a bare named subcommand = %v, want leanClear", got)
	}
	for _, bulk := range []*bool{&setupAllFlag, &setupRepairFlag, &setupInstallMissingFlag} {
		*bulk = true
		if got := leanChoiceOf(false); got != leanKeep {
			t.Errorf("a bulk sweep = %v, want leanKeep — it must not strip an allowlist", got)
		}
		if got := leanChoiceOf(true); got != leanKeep {
			t.Errorf("a bulk sweep = %v, want leanKeep whatever the stale flag state says", got)
		}
		*bulk = false
	}
}

// TestLeanFlagsOnTheCommands pins the CLI seam for the two new clients. Every
// other test drives the writers directly, so they all stay green if a generated
// command loses its flag — and `plumb setup codex --lean` then dies with
// "unknown flag" before any of the tested code runs.
func TestLeanFlagsOnTheCommands(t *testing.T) {
	for _, tc := range []struct {
		use  string
		flag *bool
	}{
		{"codex", &setupCodexLeanFlag},
		{"gemini", &setupGeminiLeanFlag},
	} {
		t.Run(tc.use, func(t *testing.T) {
			cmd := setupSubcommand(t, tc.use)
			if cmd.Flags().Lookup("lean") == nil {
				t.Fatalf("`plumb setup %s` must expose --lean — the setupTarget.flags hook is what registers it", tc.use)
			}
			t.Cleanup(func() {
				*tc.flag = false
				_ = cmd.Flags().Set("lean", "false")
			})
			if *tc.flag {
				t.Fatal("the flag var should default to false")
			}
			if err := cmd.Flags().Set("lean", "true"); err != nil {
				t.Fatalf("setting --lean: %v", err)
			}
			if !*tc.flag {
				t.Error("--lean must be bound to the var this target's intoFn reads")
			}
		})
	}

	if f := setupSubcommand(t, "claude-desktop").Flags().Lookup("lean"); f != nil {
		t.Error("--lean must stay per-target; a client with no allowlist key must not get it")
	}
}

// TestShippedLeanTargetsReadTheirFlags drives the SHIPPED targets end to end
// (allSetupClients → intoFn → a real config on disk) in both flag states,
// because only the pair proves the wiring: the true case alone would pass a
// hardcoded leanPin, the false case alone a hardcoded leanClear.
func TestShippedLeanTargetsReadTheirFlags(t *testing.T) {
	const bin = "/usr/local/bin/plumb"
	t.Cleanup(func() { setupCodexLeanFlag, setupGeminiLeanFlag = false, false })

	for _, tc := range []struct {
		use    string
		flag   *bool
		client leanClient
	}{
		{"codex", &setupCodexLeanFlag, codexLeanClient},
		{"gemini", &setupGeminiLeanFlag, geminiLeanClient},
	} {
		t.Run(tc.use, func(t *testing.T) {
			target := shippedTarget(t, tc.use)
			cfgName := filepath.Base(mustPath(t, tc.client.pathFn))

			// One config, driven through both flag states in turn. Asserting the
			// bare run on a FRESH file would be vacuous — an absent key proves
			// nothing about clearing — so the bare run has to land on a config that
			// already carries the allowlist this test just wrote.
			path := filepath.Join(t.TempDir(), cfgName)

			*tc.flag = true
			added, _, err := target.intoFn(path, bin)
			if err != nil {
				t.Fatalf("lean register through the shipped target: %v", err)
			}
			if !added {
				t.Error("expected added=true for a fresh --lean registration")
			}
			assertPinnedAllowlist(t, tc.client, path)

			*tc.flag = false
			if _, _, err := target.intoFn(path, bin); err != nil {
				t.Fatalf("bare register through the shipped target: %v", err)
			}
			assertNoAllowlist(t, tc.client, path)

			// Re-pin, so the bulk assertion below has something to preserve.
			*tc.flag = true
			if _, _, err := target.intoFn(path, bin); err != nil {
				t.Fatalf("re-pinning through the shipped target: %v", err)
			}
			leanPath := path

			// And the bulk sweep, through the same intoFn: a repoint must keep it.
			setupRepairFlag = true
			t.Cleanup(func() { setupRepairFlag = false })
			*tc.flag = false
			if _, _, err := target.intoFn(leanPath, "/moved/plumb"); err != nil {
				t.Fatalf("bulk repoint through the shipped target: %v", err)
			}
			assertPinnedAllowlist(t, tc.client, leanPath)
			setupRepairFlag = false
		})
	}
}

// TestLeanSetupNote pins the hint to its trigger and its content: silent without
// --lean, and when it fires it must name the key, the refresh command, and the
// tool count, because a stale snapshot is the allowlist's only failure mode.
// It drives the SHIPPED targets' note hooks, so deleting `note:` from a target
// turns this red.
func TestLeanSetupNote(t *testing.T) {
	t.Cleanup(func() { setupCodexLeanFlag, setupGeminiLeanFlag = false, false })

	for _, tc := range []struct {
		use    string
		flag   *bool
		client leanClient
	}{
		{"codex", &setupCodexLeanFlag, codexLeanClient},
		{"gemini", &setupGeminiLeanFlag, geminiLeanClient},
	} {
		t.Run(tc.use, func(t *testing.T) {
			note := shippedTarget(t, tc.use).note
			if note == nil {
				t.Fatalf("the %s target must wire a note hook — on the already-registered path it is the user's only confirmation", tc.use)
			}

			// Without --lean the note must NOT be silent: that run CLEARS the key,
			// and the silence was the bug (TestBareSetupAnnouncesTheClearedAllowlist
			// drives the whole path). Silence belongs to leanKeep — the bulk sweeps,
			// which touch nothing.
			*tc.flag = false
			if got := note(); !strings.Contains(got, "cleared") {
				t.Errorf("the bare run removes the allowlist, so its note must say so, got %q", got)
			}
			if got := leanSetupNote(tc.client, leanKeep); got != "" {
				t.Errorf("a bulk sweep changes nothing, so it must announce nothing, got %q", got)
			}

			*tc.flag = true
			got := note()
			for _, want := range []string{
				tc.client.key,
				"plumb setup " + tc.use + " --lean",
				"snapshot",
				strconv.Itoa(len(tools.LeanToolNames())),
			} {
				if !strings.Contains(got, want) {
					t.Errorf("note missing %q: %q", want, got)
				}
			}
			// Codex and Gemini clear on a bare re-register, unlike Kimi — the note
			// is where the user learns which contract they are on.
			if !strings.Contains(got, "clears the key") {
				t.Errorf("the note must state the clearing contract: %q", got)
			}
		})
	}
}

// TestClaudeDesktopNeverGetsAnAllowlistKey pins why Gemini stopped borrowing
// setupClaudeDesktopInto. The two configs are the same shape, so routing both
// through one writer was free until --lean arrived; sharing it now would put an
// includeTools key into a Claude Desktop config that does not read one.
func TestClaudeDesktopNeverGetsAnAllowlistKey(t *testing.T) {
	t.Cleanup(func() { setupGeminiLeanFlag = false })
	setupGeminiLeanFlag = true

	path := filepath.Join(t.TempDir(), "claude_desktop_config.json")
	if _, _, err := setupClaudeDesktopInto(path, "/usr/local/bin/plumb"); err != nil {
		t.Fatalf("setupClaudeDesktopInto: %v", err)
	}
	entry := readLeanPlumbEntry(t, geminiLeanClient, path) // same mcpServers shape
	for _, c := range leanAllowlistClients() {
		if v, has := entry[c.key]; has {
			t.Errorf("Claude Desktop's entry grew a %s key (%v) — it does not read one", c.key, v)
		}
	}
}

// shippedTarget returns a client's entry as `plumb setup` actually ships it, so
// a test drives the real wiring rather than a locally-built lookalike.
func shippedTarget(t *testing.T, use string) setupTarget {
	t.Helper()
	for _, tgt := range allSetupClients() {
		if tgt.use == use {
			return tgt
		}
	}
	t.Fatalf("no %s entry in allSetupClients()", use)
	return setupTarget{}
}

// TestBareRepointDoesNotSilentlyDropTheAllowlist is the regression test for the
// review's reproduction: `--lean` writes 21 names, the binary moves, doctor
// warns and prints a fix, the user runs exactly that fix — and their tool
// surface silently widens back to 57 because a bare re-register clears the key.
//
// The fix line is doctor's only actionable output on that path, so it is the
// place the allowlist has to survive: it must carry --lean whenever the client's
// config holds an allowlist today, and must NOT suggest it otherwise (a flag
// that appears unbidden would pin a surface the user never asked to narrow).
func TestBareRepointDoesNotSilentlyDropTheAllowlist(t *testing.T) {
	const (
		movedBin   = "/nonexistent/moved-away/plumb"
		currentBin = "/usr/local/bin/plumb"
	)

	for _, w := range leanWriters() {
		t.Run(w.label, func(t *testing.T) {
			target := shippedTarget(t, w.label)
			cfgName := filepath.Base(mustPath(t, w.client.pathFn))

			leanPath := filepath.Join(t.TempDir(), cfgName)
			if _, _, err := w.into(leanPath, movedBin, leanPin); err != nil {
				t.Fatalf("lean register: %v", err)
			}
			res := classifyClientBinary(target, leanPath, currentBin)
			if res.fix == "" {
				t.Fatalf("a missing registered binary must carry a fix line: %+v", res)
			}
			wantCmd := "plumb setup " + w.label + " --lean"
			if !strings.Contains(res.fix, wantCmd) {
				t.Errorf("doctor's repoint fix is %q — following it runs a bare re-register, which clears %s "+
					"and widens the surface from %d tools back to the full registry. Want it to name %q.",
					res.fix, w.client.key, len(tools.LeanToolNames()), wantCmd)
			}

			barePath := filepath.Join(t.TempDir(), cfgName)
			if _, _, err := w.into(barePath, movedBin, leanClear); err != nil {
				t.Fatalf("bare register: %v", err)
			}
			if bare := classifyClientBinary(target, barePath, currentBin); strings.Contains(bare.fix, "--lean") {
				t.Errorf("a client with no allowlist must be repointed with the plain command, got %q", bare.fix)
			}
		})
	}

	// Kimi preserves the key on a bare re-register, so its repoint was never
	// destructive — and there `--lean` can only make things WORSE, because it
	// REPLACES the list. The suggestion therefore turns on the grade, not on mere
	// presence: only a list that reads as plumb's own aged snapshot is worth
	// refreshing, and a hand-picked one must be left exactly alone.
	t.Run("kimi-code", func(t *testing.T) {
		lean := tools.LeanToolNames()
		for _, tc := range []struct {
			name      string
			allowlist []string
			wantLean  bool
		}{
			{"hand-picked list is not plumb's to replace", []string{"read_file", "edit_file", "git"}, false},
			{"a current snapshot has nothing to refresh", lean, false},
			{"an aged snapshot is worth refreshing", lean[:len(lean)-1], true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "mcp.json")
				writeKimiAllowlist(t, path, movedBin, tc.allowlist)
				res := classifyClientBinary(shippedTarget(t, "kimi-code"), path, currentBin)
				if got := strings.Contains(res.fix, "--lean"); got != tc.wantLean {
					t.Errorf("repoint fix %q suggests --lean = %v, want %v — a bare re-register "+
						"preserves the key here, so --lean is the destructive option", res.fix, got, tc.wantLean)
				}
			})
		}
	})
}

// TestBareSetupAnnouncesTheClearedAllowlist is the other half of the same
// finding: the clearing path must not be the silent one. It drives the SHIPPED
// target end to end, so a note hook that short-circuits on "no --lean" fails
// here rather than shipping a command that deletes user configuration quietly.
func TestBareSetupAnnouncesTheClearedAllowlist(t *testing.T) {
	t.Cleanup(func() { setupCodexLeanFlag, setupGeminiLeanFlag = false, false })

	for _, tc := range []struct {
		use    string
		flag   *bool
		client leanClient
	}{
		{"codex", &setupCodexLeanFlag, codexLeanClient},
		{"gemini", &setupGeminiLeanFlag, geminiLeanClient},
	} {
		t.Run(tc.use, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), filepath.Base(mustPath(t, tc.client.pathFn)))
			target := shippedTarget(t, tc.use)
			target.pathFn = func() (string, error) { return path, nil }
			target.skillsDirFn = nil // the skills hint is a separate concern

			*tc.flag = true
			if out := captureStdout(t, func() {
				if err := runSetupTarget(target); err != nil {
					t.Errorf("lean register: %v", err)
				}
			}); !strings.Contains(out, "now pins") {
				t.Fatalf("expected the --lean run to confirm the allowlist:\n%s", out)
			}
			assertPinnedAllowlist(t, tc.client, path)

			*tc.flag = false
			out := captureStdout(t, func() {
				if err := runSetupTarget(target); err != nil {
					t.Errorf("bare re-register: %v", err)
				}
			})
			assertNoAllowlist(t, tc.client, path)
			for _, want := range []string{tc.client.key, "cleared", "plumb setup " + tc.use + " --lean"} {
				if !strings.Contains(out, want) {
					t.Errorf("the run removed the allowlist without saying %q:\n%s", want, out)
				}
			}
		})
	}

	// The note must not claim anything it cannot substantiate. It used to close
	// with "the previous config was backed up alongside it", which is false on a
	// first-ever registration — mergeServerEntry skips the backup when the file is
	// new — and on an idempotent second run, which writes nothing at all.
	t.Run("no unsubstantiated backup claim", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, filepath.Base(mustPath(t, codexLeanClient.pathFn)))
		target := shippedTarget(t, "codex")
		target.pathFn = func() (string, error) { return path, nil }
		target.skillsDirFn = nil
		setupCodexLeanFlag = false

		first := captureStdout(t, func() {
			if err := runSetupTarget(target); err != nil {
				t.Errorf("first-ever bare register: %v", err)
			}
		})
		second := captureStdout(t, func() {
			if err := runSetupTarget(target); err != nil {
				t.Errorf("idempotent second run: %v", err)
			}
		})
		if !strings.Contains(second, "already registered") {
			t.Fatalf("expected the second run to write nothing:\n%s", second)
		}
		backups, err := filepath.Glob(filepath.Join(dir, "*.bak"))
		if err != nil {
			t.Fatalf("globbing backups: %v", err)
		}
		for _, out := range []string{first, second} {
			for _, claim := range []string{"backed up", "backup"} {
				if strings.Contains(out, claim) && len(backups) == 0 {
					t.Errorf("the note claims %q but no backup exists (%v):\n%s", claim, backups, out)
				}
			}
		}
	})

	// Kimi's bare re-register preserves the key, so it has nothing to announce —
	// and must stay silent rather than claim a change it did not make.
	t.Cleanup(func() { setupKimiLeanFlag = false })
	setupKimiLeanFlag = false
	if got := kimiLeanNote(); got != "" {
		t.Errorf("Kimi preserves on a bare re-register, so its note must stay silent, got %q", got)
	}
}

// TestLeanClientsDeclareTheirCapability ties the two halves of the client-side
// allowlist across the package boundary. internal/cli decides which clients
// `--lean` can write an allowlist for; internal/tools decides what session_start
// may then SAY to them, keyed off clientcaps.ClientSideAllowlist. Nothing in the
// type system connects the two, so adding a fourth --lean client without the
// clientcaps flag would ship guidance naming tools that client's own config had
// just removed — the exact failure this test exists to prevent.
func TestLeanClientsDeclareTheirCapability(t *testing.T) {
	for _, c := range leanAllowlistClients() {
		// The setup subcommand doubles as the clientInfo.name prefix for all
		// three; that coincidence is what makes the lookup work, so assert it
		// rather than assume it.
		caps := clientcaps.Lookup(c.setupCmd)
		if !caps.ClientSideAllowlist {
			t.Errorf("`plumb setup %s --lean` writes a %s allowlist, but clientcaps.Lookup(%q) reports "+
				"ClientSideAllowlist=false (entry %q) — session_start will steer that client at tools "+
				"its own config filtered out", c.setupCmd, c.key, c.setupCmd, caps.Name)
		}
	}
}

// TestCodexSetupDefersItsDirectToolPresentation keeps the workable Codex
// optimisation distinct from server-side lean: Codex receives Plumb's complete
// MCP catalogue, but keeps those schemas out of the initial model request.
func TestCodexSetupDefersItsDirectToolPresentation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	const bin = "/usr/local/bin/plumb"

	if err := writeTOML(path, map[string]any{
		"mcp_servers": map[string]any{
			"plumb": map[string]any{"command": bin, "args": []string{"serve"}},
		},
	}); err != nil {
		t.Fatal("write legacy Codex registration:", err)
	}

	added, _, err := codexLeanInto(path, bin, leanClear)
	if err != nil {
		t.Fatal("upgrade Codex:", err)
	}
	if !added {
		t.Fatal("legacy Codex registration reported no change")
	}
	if got := readLeanPlumbEntry(t, codexLeanClient, path)["omit_tools_from"]; !stringSliceEqual(got, codexDeferredToolSurfaces) {
		t.Errorf("omit_tools_from = %v, want %v", got, codexDeferredToolSurfaces)
	}

	added, _, err = codexLeanInto(path, bin, leanClear)
	if err != nil {
		t.Fatal("re-register Codex:", err)
	}
	if added {
		t.Error("unchanged deferred Codex registration should be idempotent")
	}
}
