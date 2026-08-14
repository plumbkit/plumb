package tools

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestGitChildEnv_EmptyInheritsVerbatim pins the no-op case: with nothing
// configured the builder returns nil so cmd.Env stays nil and os/exec inherits
// the daemon's environment directly. Returning os.Environ() instead would be a
// reconstruction, not an inheritance, and would silently change behaviour for
// every user who never set the knob.
func TestGitChildEnv_EmptyInheritsVerbatim(t *testing.T) {
	if env := gitChildEnv(nil); env != nil {
		t.Errorf("nil overrides must yield a nil env (inherit), got %d entries", len(env))
	}
	if env := gitChildEnv(map[string]string{}); env != nil {
		t.Errorf("empty overrides must yield a nil env (inherit), got %d entries", len(env))
	}
}

// TestGitChildEnv_ExtendsAndOverrides pins the replace-vs-extend decision: the
// inherited environment survives (git needs PATH, HOME and SSH_AUTH_SOCK to
// function at all) and a configured name beats the inherited value of the same
// name.
func TestGitChildEnv_ExtendsAndOverrides(t *testing.T) {
	t.Setenv("PLUMB_GIT_ENV_INHERITED", "kept")
	t.Setenv("PLUMB_GIT_ENV_CLASHING", "inherited")

	env := gitChildEnv(map[string]string{
		"PLUMB_GIT_ENV_CLASHING": "overridden",
		"PLUMB_GIT_ENV_NEW":      "added",
		"PLUMB_GIT_ENV_BLANK":    "",
	})

	want := map[string]string{
		"PLUMB_GIT_ENV_INHERITED": "kept",
		"PLUMB_GIT_ENV_CLASHING":  "overridden",
		"PLUMB_GIT_ENV_NEW":       "added",
		"PLUMB_GIT_ENV_BLANK":     "",
	}
	for k, v := range want {
		if got, ok := lookupEnvEntry(env, k); !ok || got != v {
			t.Errorf("%s = %q (present=%v), want %q", k, got, ok, v)
		}
	}
	// An override must REPLACE the inherited entry, not shadow it with a
	// duplicate: which of two entries wins is left undefined by POSIX, and Go
	// documents only that "duplicate environment variables are appended".
	if n := countEnvEntries(env, "PLUMB_GIT_ENV_CLASHING"); n != 1 {
		t.Errorf("PLUMB_GIT_ENV_CLASHING appears %d times, want exactly 1", n)
	}
	// PATH is the load-bearing part of "extend": without it git cannot find its
	// own subcommands, and a hook cannot find a toolchain.
	if _, ok := lookupEnvEntry(env, "PATH"); !ok {
		t.Error("the inherited PATH must survive — the knob extends, it does not replace")
	}
}

func lookupEnvEntry(env []string, key string) (string, bool) {
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, key+"="); ok {
			return v, true
		}
	}
	return "", false
}

func countEnvEntries(env []string, key string) int {
	n := 0
	for _, e := range env {
		if strings.HasPrefix(e, key+"=") {
			n++
		}
	}
	return n
}

// TestGit_ConfiguredEnvReachesTheChild is the end-to-end proof, through the real
// tool and a real git binary: [git] env is observed by the git process itself.
//
// GIT_AUTHOR_NAME/EMAIL are the probe because git records them in the commit
// object, so the assertion reads back what the CHILD saw rather than what plumb
// intended to pass. initTestRepo sets a different user.name in the repo config,
// so a commit that ignored the environment is authored by "Test User" and the
// test fails — the value cannot be produced by any path other than the
// environment actually arriving.
func TestGit_ConfiguredEnvReachesTheChild(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("writing file: %v", err)
	}
	tool := NewGit(WriteDeps{}, func() GitPolicy {
		return GitPolicy{
			AllowWrites: true,
			Env: map[string]string{
				"GIT_AUTHOR_NAME":  "Env Probe",
				"GIT_AUTHOR_EMAIL": "probe@plumb.test",
			},
		}
	})
	if _, err := callGit(t, tool, map[string]any{"subcommand": "add", "files": []string{"f.txt"}, "repo": dir}); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if _, err := callGit(t, tool, map[string]any{"subcommand": "commit", "message": "env probe", "repo": dir}); err != nil {
		t.Fatalf("git commit: %v", err)
	}

	out, err := callGit(t, tool, map[string]any{
		"subcommand": "log", "args": []string{"-1", "--format=%an <%ae>"}, "repo": dir,
	})
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if !strings.Contains(out, "Env Probe <probe@plumb.test>") {
		t.Errorf("the configured [git] env did not reach the git child: author = %q, want \"Env Probe <probe@plumb.test>\"", strings.TrimSpace(out))
	}
	// The inherited environment must still be there: git found itself on PATH
	// and read the repo config, which is only possible because the knob extends.
	if strings.Contains(out, "Test User") {
		t.Errorf("the repo-config author leaked through, got %q", out)
	}
}

// TestGitChildEnv_DeterministicOnCaseFold pins that two names differing only by
// case are emitted in a defined order (sorted) rather than by map-iteration
// luck. Linux and Darwin have case-SENSITIVE environments, so there both names
// simply survive; Windows is where it matters — os/exec folds case when it
// deduplicates cmd.Env and keeps the last matching entry, so the winner is
// "last after sorting", which is only well-defined because of the sort.
func TestGitChildEnv_DeterministicOnCaseFold(t *testing.T) {
	overrides := map[string]string{"Plumb_Fold": "upper-first", "plumb_fold": "lower-first"}
	first := gitChildEnv(overrides)
	for range 20 {
		if got := gitChildEnv(overrides); !slices.Equal(got, first) {
			t.Fatal("gitChildEnv is not deterministic across runs with the same overrides")
		}
	}
}
