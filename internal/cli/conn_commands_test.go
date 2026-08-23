package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
)

// TestCommandsFromProject mirrors TestTaskProvenance: a [[command]] array in the
// project config marks its entries project-sourced (so run_command's trust gate
// applies), while no project array means the commands are global.
//
// It now asks the SESSION VIEW rather than a standalone helper. The helper it
// replaced looked the raw key up by exact name, which missed `[[COMMAND]]` and
// skipped the trust gate entirely; provenance is resolved from the policy spec
// at config apply instead. The fold spellings are exercised here as well as in
// conn_commands_trust_test.go, because this is the test that describes what
// "project-sourced" MEANS.
func TestCommandsFromProject(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	projectCommands := func(t *testing.T, body string) bool {
		t.Helper()
		ws := t.TempDir()
		if body != "" {
			writeExecProject(t, ws, body)
		}
		return execTrustSession(t, ws).view().projectCommands
	}

	if projectCommands(t, "") {
		t.Error("a workspace with no project config must report fromProject=false")
	}
	if projectCommands(t, "[edits]\nrate_limit_per_minute = 7\n") {
		t.Error("a project config with no [[command]] must report fromProject=false")
	}
	for _, body := range []string{
		"[[command]]\nname = \"lint\"\nexec = [\"golangci-lint\", \"run\"]\n",
		"[[COMMAND]]\nname = \"lint\"\nexec = [\"golangci-lint\", \"run\"]\n",
		"[[Command]]\nname = \"lint\"\nexec = [\"golangci-lint\", \"run\"]\n",
		"command = [{name = \"lint\", exec = [\"golangci-lint\", \"run\"]}]\n",
	} {
		if !projectCommands(t, body) {
			t.Errorf("a project config defining commands must report fromProject=true: %q", body)
		}
	}
}

func TestCommandWorkdir(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "internal", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := commandWorkdir(ws, "")
	if err != nil || got != ws {
		t.Errorf("empty workdir = %q, %v; want %q", got, err, ws)
	}
	got, err = commandWorkdir(ws, ".")
	if err != nil || got != ws {
		t.Errorf("dot workdir = %q, %v; want %q", got, err, ws)
	}
	want := filepath.Join(ws, "internal", "x")
	got, err = commandWorkdir(ws, "internal/x")
	if err != nil || got != want {
		t.Errorf("subdir workdir = %q, %v; want %q", got, err, want)
	}
}

// TestCommandWorkdir_RefusesASymlinkOutOfTheTree is the escape the load-time
// validator cannot see.
//
// validateCommandWorkingDir rejects an absolute path and a ".." segment — every
// escape you can SPELL. "escape" names a symlink, so it spells nothing: it is a
// plain relative single-segment path, lexically impeccable, and
// filepath.Join(ws, "escape") cleans to a string that looks contained. Only
// resolving it says otherwise, which is why the check has to run after
// resolution and has to REFUSE rather than rewrite the path into something that
// merely reads as contained.
//
// Without the resolution-time check this test's command runs in outside/, and
// nothing in the config, the validator, or the argv looks wrong.
func TestCommandWorkdir_RefusesASymlinkOutOfTheTree(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(ws, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// Precondition, through the real loader rather than a restatement of the rule:
	// a project config naming this working_dir loads WITHOUT error, so the lexical
	// validator genuinely accepts it and the gap is genuinely here.
	writeProjectConfig(t, ws, "[tasks.go]\nworking_dir = \"escape\"\n")
	if _, _, err := config.LoadProjectWithPolicy(config.Defaults(), ws); err != nil {
		t.Fatalf("precondition: a project config with working_dir = \"escape\" was expected to load cleanly, got %v", err)
	}

	got, err := commandWorkdir(ws, "escape")
	if err == nil {
		t.Fatalf("a working_dir symlinked out of the workspace was accepted, resolving to %q — "+
			"the command would have run in %s", got, outside)
	}
	if got != "" {
		t.Errorf("a refused working_dir must return no directory, got %q", got)
	}
}
