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
	for _, f := range []string{"AGENTS.md", "CLAUDE.md", "GEMINI.md"} {
		data, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatalf("reading %s after --sync: %v", f, err)
		}
		if !strings.Contains(string(data), setup.DefaultTemplate) {
			t.Errorf("%s missing the default template after --sync", f)
		}
	}

	status, err := setup.Check(filepath.Join(dir, "AGENTS.md"), setup.DefaultTemplate, setup.DefaultVersion)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if status != setup.StatusCurrent {
		t.Errorf("status after --sync = %v, want %v", status, setup.StatusCurrent)
	}
}
