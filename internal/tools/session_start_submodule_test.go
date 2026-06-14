package tools

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initSuperWithSubmodule builds a throwaway superproject with one initialised
// git submodule and returns the superproject root. Skips the test when git is
// unavailable. file-protocol submodule add is enabled explicitly because modern
// git refuses local-path submodules by default.
func initSuperWithSubmodule(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	super := filepath.Join(root, "super")
	sub := filepath.Join(root, "sub")
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git %v unavailable: %v (%s)", args, err, out)
		}
	}
	mkrepo := func(dir string) {
		t.Helper()
		if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
			t.Skipf("git init unavailable: %v (%s)", err, out)
		}
		run(dir, "-C", dir, "commit", "-q", "--allow-empty", "-m", "init")
	}
	mkrepo(super)
	mkrepo(sub)
	run(super, "-C", super, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "plumb")
	return super
}

func TestGitSubmodules(t *testing.T) {
	super := initSuperWithSubmodule(t)
	got := gitSubmodules(super)
	if len(got) != 1 || got[0] != "plumb" {
		t.Fatalf("gitSubmodules = %v, want [plumb]", got)
	}
}

func TestGitSubmodules_NoneWhenAbsent(t *testing.T) {
	ws := t.TempDir()
	if got := gitSubmodules(ws); got != nil {
		t.Errorf("gitSubmodules on a repo with no .gitmodules = %v, want nil", got)
	}
}

func TestWriteSessionSubmodules(t *testing.T) {
	t.Run("rendered with repo-targeting guidance", func(t *testing.T) {
		super := initSuperWithSubmodule(t)
		var sb strings.Builder
		writeSessionSubmodules(&sb, super)
		out := sb.String()
		for _, want := range []string{
			"## Submodules (nested git repositories)",
			"- plumb/",
			filepath.Join(super, "plumb"), // the example repo= path
			"only the submodule's pointer",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("want submodule section to contain %q\n%s", want, out)
			}
		}
	})

	t.Run("omitted when no submodules", func(t *testing.T) {
		var sb strings.Builder
		writeSessionSubmodules(&sb, t.TempDir())
		if sb.Len() != 0 {
			t.Errorf("submodule section should be empty without submodules, got %q", sb.String())
		}
	})
}
