package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/setup"
)

func TestInstructionPaths_ResolveUnderCwdAndHome(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", home)

	for _, tc := range []struct {
		name        string
		pathsFn     func() (string, string, error)
		wantProject string
		wantGlobal  string
	}{
		{"codex", codexInstructionPaths, filepath.Join(dir, "AGENTS.md"), filepath.Join(home, ".codex", "AGENTS.md")},
		{"gemini", geminiInstructionPaths, filepath.Join(dir, "GEMINI.md"), filepath.Join(home, ".gemini", "GEMINI.md")},
		{"claude-code", claudeCodeInstructionPaths, filepath.Join(dir, "CLAUDE.md"), filepath.Join(home, ".claude", "CLAUDE.md")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project, global, err := tc.pathsFn()
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if project != tc.wantProject {
				t.Errorf("project = %s, want %s", project, tc.wantProject)
			}
			if global != tc.wantGlobal {
				t.Errorf("global = %s, want %s", global, tc.wantGlobal)
			}
		})
	}
}

func TestApplyInstructionsBlock_ProjectOnlyByDefault(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", home)
	setupGlobalInstructionsFlag = false

	lines, err := applyInstructionsBlock(codexTarget)
	if err != nil {
		t.Fatalf("applyInstructionsBlock: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("expected exactly one report line (project only) without --global, got %v", lines)
	}
	if _, err := os.Stat(filepath.Join(dir, "AGENTS.md")); err != nil {
		t.Errorf("project AGENTS.md not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "AGENTS.md")); !os.IsNotExist(err) {
		t.Errorf("global AGENTS.md should not exist without --global (err=%v)", err)
	}
}

func TestApplyInstructionsBlock_GlobalFlagWritesBoth(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", home)
	setupGlobalInstructionsFlag = true
	t.Cleanup(func() { setupGlobalInstructionsFlag = false })

	lines, err := applyInstructionsBlock(codexTarget)
	if err != nil {
		t.Fatalf("applyInstructionsBlock: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("expected two report lines (project + global) with --global, got %v", lines)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "AGENTS.md")); err != nil {
		t.Errorf("global AGENTS.md not written with --global: %v", err)
	}
}

func TestApplyInstructionsBlock_NilInstructionsFnIsNoOp(t *testing.T) {
	lines, err := applyInstructionsBlock(claudeDesktopTarget)
	if err != nil {
		t.Fatalf("applyInstructionsBlock: %v", err)
	}
	if lines != nil {
		t.Errorf("expected nil lines for a target with no instructionsFn, got %v", lines)
	}
}

func TestRunSetupInstructionsCheckOrSync_CheckThenSync(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", home)

	if err := runSetupInstructionsCheckOrSync(false); err != nil {
		t.Fatalf("--check: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "AGENTS.md")); !os.IsNotExist(err) {
		t.Fatalf("--check must never write; AGENTS.md exists (err=%v)", err)
	}

	if err := runSetupInstructionsCheckOrSync(true); err != nil {
		t.Fatalf("--sync: %v", err)
	}
	// None of these three files are symlinked to each other here, so each
	// gets its OWN client's template, not the shared DefaultTemplate — see
	// TestRunSetupInstructionsCheckOrSync_SymlinkedLayoutConvergesOnSharedTemplate
	// for the case where they are.
	for file, client := range map[string]string{"AGENTS.md": "codex", "CLAUDE.md": "claude-code", "GEMINI.md": "gemini"} {
		data, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			t.Fatalf("reading %s after --sync: %v", file, err)
		}
		body, ok := setup.TemplateForClient(client)
		if !ok {
			t.Fatalf("no template registered for %s", client)
		}
		if !strings.Contains(string(data), body) {
			t.Errorf("%s missing its own (%s) template after --sync", file, client)
		}

		status, err := setup.Check(filepath.Join(dir, file), body, setup.DefaultVersion)
		if err != nil {
			t.Fatalf("Check %s: %v", file, err)
		}
		if status != setup.StatusCurrent {
			t.Errorf("status of %s after --sync = %v, want %v", file, status, setup.StatusCurrent)
		}
	}
}

// TestRunSetupInstructionsCheckOrSync_SymlinkedLayoutConvergesOnSharedTemplate
// is the key convergence test: on a layout where CLAUDE.md and GEMINI.md
// symlink to AGENTS.md (this repo's own), all three clients' differing
// per-client templates must not fight over the one real file. --sync must
// write the shared, client-agnostic DefaultTemplate, and running it (or a
// per-client `plumb setup <client>`, in any order) repeatedly must converge
// on that same content rather than oscillate between clients' templates.
func TestRunSetupInstructionsCheckOrSync_SymlinkedLayoutConvergesOnSharedTemplate(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", home)

	agentsPath := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(agentsPath, []byte("# shared brief\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(agentsPath, filepath.Join(dir, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(agentsPath, filepath.Join(dir, "GEMINI.md")); err != nil {
		t.Fatal(err)
	}

	// --sync first.
	if err := runSetupInstructionsCheckOrSync(true); err != nil {
		t.Fatalf("--sync: %v", err)
	}
	afterSync, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(afterSync), setup.DefaultTemplate) {
		t.Errorf("AGENTS.md missing the shared DefaultTemplate after --sync on a symlinked layout; got:\n%s", afterSync)
	}
	if strings.Count(string(afterSync), "<!-- plumb:managed:start") != 1 {
		t.Errorf("expected exactly one block on the shared file; got:\n%s", afterSync)
	}

	// Now drive each per-client setup command directly, in an arbitrary
	// order, and confirm the file's content never changes again — the
	// convergence property. applyInstructionsBlock is the function each
	// `plumb setup <client>` subcommand calls.
	setupGlobalInstructionsFlag = false
	for _, target := range []setupTarget{claudeCodeTarget, geminiTarget, codexTarget, geminiTarget, codexTarget} {
		if _, err := applyInstructionsBlock(target); err != nil {
			t.Fatalf("applyInstructionsBlock(%s): %v", target.name, err)
		}
		got, err := os.ReadFile(agentsPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(afterSync) {
			t.Fatalf("content oscillated after applyInstructionsBlock(%s):\nwant:\n%s\ngot:\n%s", target.name, afterSync, got)
		}
	}

	// --check must report every client's row as current, not modified/stale.
	groups, err := groupInstructionFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 {
		t.Fatalf("expected the three clients to collapse into one group, got %d", len(groups))
	}
	status, err := setup.Check(agentsPath, templateForGroup(groups[0]), setup.DefaultVersion)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if status != setup.StatusCurrent {
		t.Errorf("status = %v, want %v", status, setup.StatusCurrent)
	}
}
