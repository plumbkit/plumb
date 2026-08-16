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
	want := []string{"claude-code", "codex", "kimi-code", "zcode"}

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
	t.Run("zcode is ~/.zcode/skills", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		assertDir(t, zcodeSkillsDir, filepath.Join(home, ".zcode", "skills"))
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
				if want := stampSkillContent(embedded[i].Content); string(got) != want {
					t.Errorf("%s: installed content differs from the stamped embedded source", r.name)
				}
				if v, ok := skillMarkerVersion(string(got)); !ok || v != Version {
					t.Errorf("%s: marker = (%q, %v), want the running version %q", r.name, v, ok, Version)
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

// TestStampSkillContent pins the marker's placement: after the closing
// frontmatter delimiter when there is one (inside the block it would corrupt
// the YAML the clients parse), leading the file otherwise, and always
// strippable back to the exact source — the property the content comparison
// relies on to keep a version bump from reading as drift. It also pins CRLF
// handling: a CRLF skill must be recognised as carrying frontmatter (not
// fall through to the no-frontmatter branch, which would prepend the marker
// before "---" and corrupt the block every verified client parses), and the
// inserted marker must match the file's own line-ending style rather than
// always LF.
func TestStampSkillContent(t *testing.T) {
	pinVersion(t, "9.9.9")
	const markerLF = "<!-- plumb: 9.9.9 -->\n"
	const markerCRLF = "<!-- plumb: 9.9.9 -->\r\n"

	cases := []struct {
		name    string
		content string
		want    string
	}{
		{
			"LF with frontmatter", "---\nname: x\ndescription: y\n---\nbody\n",
			"---\nname: x\ndescription: y\n---\n" + markerLF + "body\n",
		},
		{
			"CRLF with frontmatter", "---\r\nname: x\r\ndescription: y\r\n---\r\nbody\r\n",
			"---\r\nname: x\r\ndescription: y\r\n---\r\n" + markerCRLF + "body\r\n",
		},
		{"no frontmatter leads the file", "body\n", markerLF + "body\n"},
		{"unterminated frontmatter leads the file", "---\nname: x\nbody\n", markerLF + "---\nname: x\nbody\n"},
		{"empty file", "", markerLF},
		{
			"body contains a delimiter-like line but no frontmatter",
			"body\n---\nmore\n",
			markerLF + "body\n---\nmore\n",
		},
		{
			"marker on the last line (frontmatter with no body)",
			"---\nname: x\n---\n",
			"---\nname: x\n---\n" + markerLF,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stamped := stampSkillContent(tc.content)
			if stamped != tc.want {
				t.Errorf("stampSkillContent = %q, want %q", stamped, tc.want)
			}
			if got := stripSkillMarker(stamped); got != tc.content {
				t.Errorf("stripSkillMarker(stampSkillContent(c)) = %q, want the original %q", got, tc.content)
			}
			if v, ok := skillMarkerVersion(stamped); !ok || v != "9.9.9" {
				t.Errorf("skillMarkerVersion(stamped) = (%q, %v), want (\"9.9.9\", true)", v, ok)
			}
		})
	}
}

// TestStampSkillContent_CRLFFrontmatterNotCorrupted is a focused regression
// for the CRLF finding: stamping a CRLF skill must never change what precedes
// the closing "---" delimiter. Before the fix, a CRLF file failed the bare
// "---\n" prefix check, fell into the no-frontmatter branch, and got its
// marker PREPENDED — so byte 0 was no longer "-", and claude-code/codex/
// kimi-code (which all parse frontmatter as the block between the leading
// "---" lines) would stop seeing the skill's name/description and it would
// silently vanish from that client's catalogue.
func TestStampSkillContent_CRLFFrontmatterNotCorrupted(t *testing.T) {
	pinVersion(t, "9.9.9")
	content := "---\r\nname: x\r\ndescription: y\r\n---\r\nbody\r\n"

	stamped := stampSkillContent(content)

	if !strings.HasPrefix(stamped, "---\r\n") {
		t.Fatalf("stamped content does not open with the frontmatter delimiter: %q", stamped)
	}
	wantFrontmatter := "---\r\nname: x\r\ndescription: y\r\n---\r\n"
	if !strings.HasPrefix(stamped, wantFrontmatter) {
		t.Errorf("frontmatter block was altered: got %q, want it to start with %q", stamped, wantFrontmatter)
	}
}

// TestInstallSkill_RestampsStaleMarkerInPlace exercises installSkill's
// restamp-in-place branch (readErr == nil && stripSkillMarker(existing) ==
// content): the same skill content behind a stale — or, per the second
// subtest, missing — marker must still report "unchanged" while the file on
// disk picks up the current version. Nothing else reaches this branch:
// TestInstallSkillsFor_EveryCapableClientGetsEverySkill's idempotence check
// hits the earlier "existing == stamped" case (both content and marker
// already current), so a regression that dropped the restamp — writing
// content instead of stamped, or skipping the write and returning
// "unchanged" over the stale file unchanged — would pass every other test in
// this file yet fail here.
func TestInstallSkill_RestampsStaleMarkerInPlace(t *testing.T) {
	embedded := embeddedSkills()
	if len(embedded) == 0 {
		t.Fatal("embeddedSkills() returned nothing — the embed is broken")
	}
	skill := embedded[0]

	writeExisting := func(t *testing.T, dir string, existing string) string {
		t.Helper()
		skillDir := filepath.Join(dir, skill.Name)
		if err := os.MkdirAll(skillDir, 0o755); err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(skillDir, "SKILL.md")
		if err := os.WriteFile(dst, []byte(existing), 0o600); err != nil {
			t.Fatal(err)
		}
		return dst
	}

	assertRestamped := func(t *testing.T, dst string) {
		t.Helper()
		got, err := os.ReadFile(dst)
		if err != nil {
			t.Fatalf("reading restamped skill: %v", err)
		}
		if v, ok := skillMarkerVersion(string(got)); !ok || v != "9.9.9" {
			t.Errorf("marker after restamp = (%q, %v), want (\"9.9.9\", true) — the stale/missing "+
				"marker was not refreshed", v, ok)
		}
		if got := stripSkillMarker(string(got)); got != skill.Content {
			t.Errorf("content around the marker differs from the embedded source after restamp:\ngot  %q\nwant %q",
				got, skill.Content)
		}
	}

	t.Run("stale marker version", func(t *testing.T) {
		dir := t.TempDir()
		pinVersion(t, "0.1.0")
		dst := writeExisting(t, dir, stampSkillContent(skill.Content))

		pinVersion(t, "9.9.9")
		action, err := installSkill(dir, skill.Name, skill.Content)
		if err != nil {
			t.Fatalf("installSkill: %v", err)
		}
		if action != "unchanged" {
			t.Fatalf("action = %q, want %q", action, "unchanged")
		}
		assertRestamped(t, dst)
	})

	t.Run("missing marker", func(t *testing.T) {
		dir := t.TempDir()
		dst := writeExisting(t, dir, skill.Content)

		pinVersion(t, "9.9.9")
		action, err := installSkill(dir, skill.Name, skill.Content)
		if err != nil {
			t.Fatalf("installSkill: %v", err)
		}
		if action != "unchanged" {
			t.Fatalf("action = %q, want %q", action, "unchanged")
		}
		assertRestamped(t, dst)
	})
}

// TestVersionOlder pins the semver-ish comparison behind the "installed by"
// wording: only a strictly older marker is named; equal, newer, and
// unparseable (either side, including the "dev" build stamp) all fall back
// to the plain form rather than inventing an ordering.
func TestVersionOlder(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.15.1", "0.16.3", true},
		{"v0.15.1", "0.16.3", true},
		{"0.15", "0.15.1", true},
		{"0.16.3", "0.16.3", false},
		{"0.17.0", "0.16.3", false},
		{"0.16.3-rc.1", "0.16.3", false},
		{"dev", "0.16.3", false},
		{"0.15.1", "dev", false},
		{"", "0.16.3", false},
	}
	for _, tc := range cases {
		if got := versionOlder(tc.a, tc.b); got != tc.want {
			t.Errorf("versionOlder(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
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

// TestEmbeddedSkills_ReferencesAreInTheBinary is the defect this pair of tests
// exists for. plumb-chat/SKILL.md instructs the reader to use
// references/idle-agent-wake-hook.md; the embed pattern was skills/*/SKILL.md,
// so the file was not in the binary at all and a release-binary user got a skill
// pointing at something they could not obtain.
//
// It asserts a NAMED reference rather than "some skill has some reference":
// the latter passes on any stray file added later while the one SKILL.md
// actually cites goes missing, which is exactly the failure being fixed.
func TestEmbeddedSkills_ReferencesAreInTheBinary(t *testing.T) {
	var chat *embeddedSkill
	for i, s := range embeddedSkills() {
		if s.Name == "plumb-chat" {
			chat = &embeddedSkills()[i]
		}
	}
	if chat == nil {
		t.Fatal("plumb-chat is not among the embedded skills")
	}

	var found *embeddedFile
	for i, ref := range chat.References {
		if ref.Name == "idle-agent-wake-hook.md" {
			found = &chat.References[i]
		}
	}
	if found == nil {
		t.Fatalf("plumb-chat/SKILL.md cites references/idle-agent-wake-hook.md but it is not embedded; got %d reference(s)", len(chat.References))
	}
	if len(found.Content) == 0 {
		t.Error("the embedded reference is empty")
	}
	if !strings.Contains(chat.Content, "references/idle-agent-wake-hook.md") {
		t.Error("plumb-chat/SKILL.md no longer cites the reference this test pins — re-point or remove it")
	}
}

// TestInstallSkillsFor_WritesReferencesBesideSKILLmd covers the install side and
// the two states the status table has to tell apart afterwards.
func TestInstallSkillsFor_WritesReferencesBesideSKILLmd(t *testing.T) {
	pointClientHomesAt(t)
	c := skillCapableClients()[0]

	dir, results := installSkillsFor(c)
	for _, r := range results {
		if r.err != nil {
			t.Fatalf("installing %q: %v", r.name, r.err)
		}
	}

	ref := filepath.Join(dir, "plumb-chat", "references", "idle-agent-wake-hook.md")
	got, err := os.ReadFile(ref)
	if err != nil {
		t.Fatalf("reading installed reference: %v", err)
	}

	// Exact, unstamped: a reference is plain material a user may open in any
	// viewer, so unlike SKILL.md it carries no provenance marker. Looked up by
	// NAME, not References[0] — a second reference sorting ahead of this one
	// would otherwise compare the wrong file's content and fail spuriously.
	want := embeddedReferenceNamed(t, "plumb-chat", "idle-agent-wake-hook.md").Content
	if string(got) != want {
		t.Error("installed reference differs from the embedded source (or was stamped, which it must not be)")
	}

	// A second sync is a no-op.
	if _, results = installSkillsFor(c); results[0].err != nil {
		t.Fatalf("re-sync: %v", results[0].err)
	}
	for _, r := range results {
		if r.name == "plumb-chat" && r.action != "unchanged" {
			t.Errorf("re-sync reported %q for plumb-chat, want %q", r.action, "unchanged")
		}
	}

	// Deleting the reference must make the skill read stale, not installed:
	// grading on SKILL.md alone would report everything current while the file
	// SKILL.md sends the reader to is gone.
	if err := os.Remove(ref); err != nil {
		t.Fatal(err)
	}
	for _, s := range embeddedSkills() {
		if s.Name != "plumb-chat" {
			continue
		}
		if state := skillStateAt(dir, s.Name, s.Content, s.References); state == skillStateInstalled {
			t.Error("a skill whose reference note is missing still reports installed")
		}
	}
}

// TestInstallSkillsFor_RefreshesADriftedReference covers the other half of the
// reference path: a note that EXISTS but no longer matches the embedded copy.
// The test above only ever deletes the file, and deletion and drift take
// different branches in both directions — installSkillReferences backs a
// drifted note up before rewriting it (a missing one is written outright), and
// referencesCurrent has to compare content rather than merely stat the path.
//
// Both branches were unpinned without this: making installSkillReferences skip
// any reference that already exists, and making referencesCurrent ignore
// content and check only readability, each left the whole internal/cli suite
// green. So this asserts all three consequences of a drifted note — the
// stale reading, the backup, and the restored content — plus the "updated"
// promotion, which is the only thing that stops a run that rewrote a reference
// reporting "unchanged" because the SKILL.md beside it happened to be current.
func TestInstallSkillsFor_RefreshesADriftedReference(t *testing.T) {
	pointClientHomesAt(t)
	c := skillCapableClients()[0]

	dir, results := installSkillsFor(c)
	for _, r := range results {
		if r.err != nil {
			t.Fatalf("installing %q: %v", r.name, r.err)
		}
	}

	chat := embeddedSkillNamed(t, "plumb-chat")
	ref := filepath.Join(dir, chat.Name, "references", "idle-agent-wake-hook.md")
	const drifted = "hand-edited, and no longer what the skill ships\n"
	if err := os.WriteFile(ref, []byte(drifted), 0o600); err != nil {
		t.Fatal(err)
	}

	// SKILL.md is untouched and current, so grading it alone would report the
	// skill installed while the note it sends the reader to says something else.
	if state := skillStateAt(dir, chat.Name, chat.Content, chat.References); state == skillStateInstalled {
		t.Error("a skill whose reference note has drifted still reports installed")
	}

	_, results = installSkillsFor(c)
	var action string
	for _, r := range results {
		if r.name != chat.Name {
			continue
		}
		if r.err != nil {
			t.Fatalf("re-sync: %v", r.err)
		}
		action = r.action
	}
	if action != "updated" {
		t.Errorf("re-sync over a drifted reference reported %q, want %q", action, "updated")
	}

	got, err := os.ReadFile(ref)
	if err != nil {
		t.Fatalf("reading refreshed reference: %v", err)
	}
	if want := embeddedReferenceNamed(t, chat.Name, "idle-agent-wake-hook.md").Content; string(got) != want {
		t.Error("the drifted reference was not restored to the embedded content")
	}

	// The overwritten copy is recoverable, on the same terms as a drifted
	// SKILL.md: sync never silently discards something the user may have edited.
	backups, err := filepath.Glob(ref + ".*.bak")
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 1 {
		t.Fatalf("want exactly 1 backup of the drifted reference, got %d: %v", len(backups), backups)
	}
	if data, err := os.ReadFile(backups[0]); err != nil || string(data) != drifted {
		t.Errorf("backup does not hold the drifted content (err=%v)", err)
	}
}

// embeddedSkillNamed returns the embedded skill called name, failing the test
// when there is none.
func embeddedSkillNamed(t *testing.T, name string) embeddedSkill {
	t.Helper()
	for _, s := range embeddedSkills() {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("%q is not among the embedded skills", name)
	return embeddedSkill{}
}

// embeddedReferenceNamed returns skill's reference note called ref, failing the
// test when there is none. Lookup is by name so that adding a second reference
// cannot silently repoint an assertion at the wrong file.
func embeddedReferenceNamed(t *testing.T, skill, ref string) embeddedFile {
	t.Helper()
	s := embeddedSkillNamed(t, skill)
	for _, r := range s.References {
		if r.Name == ref {
			return r
		}
	}
	t.Fatalf("%s does not ship references/%s; got %d reference(s)", skill, ref, len(s.References))
	return embeddedFile{}
}
