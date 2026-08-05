package cli

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestSkillCapableClients_ArePinned pins WHICH clients receive SKILL.md files.
//
// This is the one fact in the skill seam that cannot be derived: "client X reads
// SKILL.md from directory Y" is a claim about someone else's product, it rots
// when they reorganise, and it is exactly the sort of thing that gets asserted
// from memory rather than checked. Each entry here was verified against a live
// install (see the resolvers' comments in setup_skills.go), so adding one has to
// be a deliberate edit of this list, not a plausible-looking struct field that
// slid through review.
//
// The negative half matters as much: every other target must stay nil. A client
// with no verified skills directory must receive its steering as the condensed
// session_start guidance block, never as files written into a directory plumb
// guessed at.
func TestSkillCapableClients_ArePinned(t *testing.T) {
	want := []string{"claude-code", "codex", "kimi-code"}

	capable := skillCapableClients()
	got := make([]string, 0, len(capable))
	for _, c := range capable {
		got = append(got, c.use)
	}
	sort.Strings(got)

	if len(got) != len(want) {
		t.Fatalf("skill-capable clients: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("skill-capable clients: got %v, want %v — adding one is a claim about that "+
				"client's real skills directory and needs live verification, not an inference", got, want)
		}
	}
}

// TestSkillsDirs_HonourClientHomeOverrides checks each resolver against the same
// home-override precedence its config-path sibling uses. A skills directory that
// ignored $CODEX_HOME / $KIMI_CODE_HOME would write into the wrong tree for any
// user who relocated their client's data dir — silently, since the install
// succeeds either way.
func TestSkillsDirs_HonourClientHomeOverrides(t *testing.T) {
	t.Run("codex honours CODEX_HOME", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("CODEX_HOME", dir)
		assertDir(t, codexSkillsDir, filepath.Join(dir, "skills"))
	})
	t.Run("codex falls back to ~/.codex/skills", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("CODEX_HOME", "")
		t.Setenv("HOME", home)
		assertDir(t, codexSkillsDir, filepath.Join(home, ".codex", "skills"))
	})
	t.Run("kimi honours KIMI_CODE_HOME", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("KIMI_CODE_HOME", dir)
		assertDir(t, kimiCodeSkillsDir, filepath.Join(dir, "skills"))
	})
	t.Run("kimi falls back to ~/.kimi-code/skills", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("KIMI_CODE_HOME", "")
		t.Setenv("HOME", home)
		assertDir(t, kimiCodeSkillsDir, filepath.Join(home, ".kimi-code", "skills"))
	})
	t.Run("claude code is ~/.claude/skills", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		assertDir(t, claudeSkillsDir, filepath.Join(home, ".claude", "skills"))
	})
}

func assertDir(t *testing.T, fn func() (string, error), want string) {
	t.Helper()
	got, err := fn()
	if err != nil {
		t.Fatalf("resolving skills dir: %v", err)
	}
	if got != want {
		t.Errorf("skills dir: got %q, want %q", got, want)
	}
}

// TestInstallSkillsFor_EveryCapableClientGetsEverySkill is the phase-5 property:
// the skill set is client-independent, only the destination differs. Before this,
// installation was hard-wired to ~/.claude/skills at a single call site, so Kimi
// Code — which reads SKILL.md and was already given a session_start block
// pointing at the skills — received none of them.
func TestInstallSkillsFor_EveryCapableClientGetsEverySkill(t *testing.T) {
	embedded := embeddedSkills()
	if len(embedded) == 0 {
		t.Fatal("embeddedSkills() returned nothing — the embed is broken")
	}

	for _, c := range skillCapableClients() {
		t.Run(c.use, func(t *testing.T) {
			root := pointClientHomesAt(t)

			dir, results := installSkillsFor(c)
			if len(results) != len(embedded) {
				t.Fatalf("got %d results, want one per embedded skill (%d)", len(results), len(embedded))
			}
			if !isUnder(dir, root) {
				t.Fatalf("resolved skills dir %q is outside the test home %q — the resolver ignored "+
					"the environment and a real run would have written into the developer's own tree", dir, root)
			}

			for i, r := range results {
				if r.err != nil {
					t.Fatalf("installing %q: %v", r.name, r.err)
				}
				if r.action != "installed" {
					t.Errorf("%s: action %q, want %q on a fresh directory", r.name, r.action, "installed")
				}
				got, err := os.ReadFile(filepath.Join(dir, r.name, "SKILL.md"))
				if err != nil {
					t.Fatalf("reading installed skill %q: %v", r.name, err)
				}
				if string(got) != embedded[i].Content {
					t.Errorf("%s: installed content differs from the embedded source", r.name)
				}
			}

			// Idempotence: the same run again must report every skill unchanged,
			// which is what makes `plumb skills sync` safe to run repeatedly.
			_, second := installSkillsFor(c)
			for _, r := range second {
				if r.err != nil || r.action != "unchanged" {
					t.Errorf("second run: %s reported (%q, %v), want (\"unchanged\", nil)", r.name, r.action, r.err)
				}
			}
		})
	}
}

// TestInstallSkillsFor_SkipsClientsWithNoSkillChannel is the negative half: a
// target with no skills directory must write nothing at all. Returning no
// results (rather than results that all say "unchanged") is what lets `plumb
// skills` tell "this client has no skill channel" from "its skills were already
// current".
func TestInstallSkillsFor_SkipsClientsWithNoSkillChannel(t *testing.T) {
	root := pointClientHomesAt(t)

	for _, c := range allSetupClients() {
		if c.skillsDirFn != nil {
			continue
		}
		dir, results := installSkillsFor(c)
		if dir != "" || results != nil {
			t.Errorf("%s: got (%q, %d results), want no skill install for a client with no channel",
				c.use, dir, len(results))
		}
	}

	assertNoSkillsWritten(t, root)
}

// pointClientHomesAt redirects every home the skill resolvers consult at one
// fresh temp root, so a test can assert that nothing was written anywhere.
func pointClientHomesAt(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex-home"))
	t.Setenv("KIMI_CODE_HOME", filepath.Join(root, "kimi-home"))
	return root
}

// assertNoSkillsWritten fails if any SKILL.md landed under root.
func assertNoSkillsWritten(t *testing.T, root string) {
	t.Helper()
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && info.Name() == "SKILL.md" {
			t.Errorf("unexpected skill written at %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
}

// isUnder reports whether path is root or sits beneath it.
func isUnder(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || !strings.HasPrefix(rel, "..")
}

// TestRefreshClient_NeverTouchesSkills pins the split that made registration
// config-only: the bulk sweep (`plumb setup --repair` / `--all`) must not write
// skill files even for a skill-capable client it just registered, and must not
// refresh stale ones — skills moved to `plumb skills sync` so a config repair
// can never surprise a user by touching their skills directory.
func TestRefreshClient_NeverTouchesSkills(t *testing.T) {
	root := pointClientHomesAt(t)
	cfg := filepath.Join(root, "kimi-home", "mcp.json")
	target := kimiTargetAt(cfg)

	// Registering an installed-but-unregistered client: config written, skills
	// directory untouched.
	rows, changed := refreshClient(target, "/opt/plumb", true)
	if !changed {
		t.Fatal("expected the registration to count as a change")
	}
	assertRowStatus(t, rows, "registered")
	assertNoSkillsWritten(t, root)

	// A stale skill from an earlier install is left alone too — drift is
	// reported (printSkillsDriftHint, doctor), never repaired by setup.
	skillsDir := filepath.Join(root, "kimi-home", "skills")
	stale := filepath.Join(skillsDir, embeddedSkills()[0].Name, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, changed = refreshClient(target, "/opt/plumb", true)
	if changed {
		t.Error("a second sweep with nothing to repoint must report no change — stale skills do not count")
	}
	assertRowStatus(t, rows, "already current")
	got, err := os.ReadFile(stale)
	if err != nil || string(got) != "stale\n" {
		t.Errorf("refreshClient rewrote a stale skill (content %q, err %v) — that is `plumb skills sync`'s job", got, err)
	}
}

// TestRefreshClient_NoSkillsForAnUnregisteredClient is the guard on the other
// side. A bare `--repair` finds an installed client that does not use plumb and
// leaves it alone; writing skills into its tree would put plumb's files in a
// directory the user never pointed at plumb.
func TestRefreshClient_NoSkillsForAnUnregisteredClient(t *testing.T) {
	root := pointClientHomesAt(t)
	cfg := filepath.Join(root, "kimi-home", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte(`{"mcpServers":{"other":{"command":"other-bin"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	rows, changed := refreshClient(kimiTargetAt(cfg), "/opt/plumb", false)
	if changed {
		t.Error("bare --all must not change an unregistered client")
	}
	assertRowStatus(t, rows, "not registered")
	for _, r := range rows {
		if strings.HasPrefix(r.status, "skills") {
			t.Errorf("unregistered client got a skills row: %+v", r)
		}
	}
	assertNoSkillsWritten(t, root)
}

// TestRefreshClient_NoSkillRowWithoutASkillChannel pins that the sweep's table
// carries no skills rows at all: not for a client with no skill channel (this
// case), and — per TestRefreshClient_NeverTouchesSkills — not for one that has
// one either.
func TestRefreshClient_NoSkillRowWithoutASkillChannel(t *testing.T) {
	root := pointClientHomesAt(t)
	cfg := filepath.Join(root, "cursor", "mcp.json")
	target := setupTarget{
		use: "cursor", name: "Cursor",
		pathFn:    func() (string, error) { return cfg, nil },
		intoFn:    setupClaudeDesktopInto,
		extractFn: claudeDesktopCommandExtractor,
	}
	if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	rows, _ := refreshClient(target, "/opt/plumb", true)
	assertRowStatus(t, rows, "registered")
	for _, r := range rows {
		if strings.HasPrefix(r.status, "skills") {
			t.Errorf("client with no skill channel got a skills row: %+v", r)
		}
	}
	assertNoSkillsWritten(t, root)
}

// kimiTargetAt is the Kimi Code target pointed at a test config path, keeping
// its real intoFn and skills resolver.
func kimiTargetAt(cfg string) setupTarget {
	return setupTarget{
		use: "kimi-code", name: "Kimi Code",
		pathFn:      func() (string, error) { return cfg, nil },
		installedFn: func() bool { return true },
		intoFn: func(cfgPath, plumbBin string) (bool, []string, error) {
			return kimiCodeInto(cfgPath, plumbBin, false)
		},
		extractFn:   claudeDesktopCommandExtractor,
		skillsDirFn: kimiCodeSkillsDir,
	}
}

func assertRowStatus(t *testing.T, rows []clientRow, want string) {
	t.Helper()
	for _, r := range rows {
		if r.status == want {
			return
		}
	}
	t.Errorf("no row with status %q; got %+v", want, rows)
}
