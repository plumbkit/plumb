package tools

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// git_noninteractive_test.go pins #544: the git child plumb runs has no terminal
// and nobody to type into one, so a verb that opens core.editor — the --continue
// of a conflicted rebase, cherry-pick or revert — has to accept git's prepared
// message rather than launch an editor. Every test here goes through the real
// tool and a real git binary.

// missingEditor is a core.editor that cannot run: before the fix, git tried to
// exec it and failed with "cannot exec '…': No such file or directory", which is
// the exact symptom #544 reports for a user whose core.editor is nvim.
const missingEditor = "/nonexistent/plumb-544-editor"

// neutraliseEditorEnv removes every variable git consults before core.editor, so
// the repository's core.editor is what an unfixed child would run. Left alone, a
// developer's own GIT_EDITOR (or a harness's GIT_EDITOR=true) would make these
// tests pass on the code they are meant to catch.
func neutraliseEditorEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"GIT_EDITOR", "GIT_SEQUENCE_EDITOR", "VISUAL", "EDITOR", "GIT_TERMINAL_PROMPT"} {
		unsetEnvForTest(t, k)
	}
}

// conflictRepo builds a repository whose default branch and a "topic" branch
// both rewrite c.txt, so replaying one onto the other conflicts. It returns the
// repository, the default branch's name and the topic commit's SHA, with the
// default branch checked out and core.editor pointing at missingEditor.
func conflictRepo(t *testing.T) (repo, base, topicSHA string) {
	t.Helper()
	requireGit(t)
	repo = initTestRepo(t)
	base = gwGit(t, repo, "branch", "--show-current")
	gwWrite(t, repo, "c.txt", "base\n")
	gwGit(t, repo, "add", "c.txt")
	gwGit(t, repo, "commit", "-q", "-m", "base c")

	gwGit(t, repo, "switch", "-q", "-c", "topic")
	gwWrite(t, repo, "c.txt", "topic\n")
	gwGit(t, repo, "commit", "-q", "-am", "topic change")
	topicSHA = gwGit(t, repo, "rev-parse", "HEAD")

	gwGit(t, repo, "switch", "-q", base)
	gwWrite(t, repo, "c.txt", "main\n")
	gwGit(t, repo, "commit", "-q", "-am", "main change")

	gwGit(t, repo, "config", "core.editor", missingEditor)
	return repo, base, topicSHA
}

// resolveConflict writes the resolution and stages it, as a user would before
// asking git to continue.
func resolveConflict(t *testing.T, repo string) {
	t.Helper()
	gwWrite(t, repo, "c.txt", "resolved\n")
	gwGit(t, repo, "add", "c.txt")
}

// TestGit_ContinueAfterAConflictNeedsNoEditor is the regression test for #544:
// each sequencer verb is started through the tool, stops on a conflict, is
// resolved, and is continued through the tool. The subject check proves git's
// prepared message was accepted as written.
//
// rebase is the case that failed before the fix: `rebase --continue` opens
// core.editor for a resolved pick whatever its stdin is. git 2.55's cherry-pick
// and revert --continue skip the editor when stdin is not a terminal, so those
// two rows pass either way; they stay as coverage of the verbs #544 names, and
// TestGit_EditingVerbsNeedNoEditor holds the cherry-pick and revert forms that
// do open it.
func TestGit_ContinueAfterAConflictNeedsNoEditor(t *testing.T) {
	cases := []struct {
		verb string
		// start returns the args that begin the operation and the subject the
		// resulting commit must carry.
		start func(base, topicSHA string) ([]string, string)
		// onTopic runs the operation on the topic branch rather than the default.
		onTopic bool
		// stateDir is the in-progress marker under .git that --continue clears.
		stateDir string
	}{
		{
			verb:     "rebase",
			start:    func(base, _ string) ([]string, string) { return []string{base}, "topic change" },
			onTopic:  true,
			stateDir: "rebase-merge",
		},
		{
			verb:     "cherry-pick",
			start:    func(_, sha string) ([]string, string) { return []string{sha}, "topic change" },
			stateDir: "CHERRY_PICK_HEAD",
		},
		{
			verb: "revert",
			// HEAD~1 is "main change"; the setup below commits a later edit of
			// c.txt on top, so undoing it conflicts.
			start:    func(_, _ string) ([]string, string) { return []string{"HEAD~1"}, `Revert "main change"` },
			stateDir: "REVERT_HEAD",
		},
	}
	for _, tc := range cases {
		t.Run(tc.verb, func(t *testing.T) {
			neutraliseEditorEnv(t)
			repo, base, topicSHA := conflictRepo(t)
			if tc.onTopic {
				gwGit(t, repo, "switch", "-q", "topic")
			}
			if tc.verb == "revert" {
				// Move c.txt on after "main change", so undoing that commit
				// conflicts with the newer content.
				gwWrite(t, repo, "c.txt", "later\n")
				gwGit(t, repo, "commit", "-q", "-am", "later change")
			}
			args, wantSubject := tc.start(base, topicSHA)
			tool := gwTool(nil)

			out, err := callGit(t, tool, map[string]any{"subcommand": tc.verb, "args": args, "confirm": true, "repo": repo})
			if err == nil {
				t.Fatalf("git %s %v was expected to stop on a conflict, but succeeded:\n%s", tc.verb, args, out)
			}
			if _, statErr := os.Stat(filepath.Join(repo, ".git", tc.stateDir)); statErr != nil {
				t.Fatalf("git %s did not leave an operation in progress (%s missing): %v\n%v", tc.verb, tc.stateDir, statErr, err)
			}

			resolveConflict(t, repo)
			out, err = callGit(t, tool, map[string]any{"subcommand": tc.verb, "args": []string{"--continue"}, "confirm": true, "repo": repo})
			if err != nil {
				t.Fatalf("git %s --continue through the tool failed — the child tried to open an editor: %v\n%s", tc.verb, err, out)
			}
			if got := gwGit(t, repo, "log", "-1", "--format=%s"); got != wantSubject {
				t.Errorf("after %s --continue HEAD's subject = %q, want git's prepared message %q", tc.verb, got, wantSubject)
			}
			if _, statErr := os.Stat(filepath.Join(repo, ".git", tc.stateDir)); !os.IsNotExist(statErr) {
				t.Errorf("git %s is still in progress after --continue (%s present)", tc.verb, tc.stateDir)
			}
			if got := gwGit(t, repo, "show", "HEAD:c.txt"); got != "resolved" {
				t.Errorf("the resolution was not committed: HEAD:c.txt = %q", got)
			}
		})
	}
}

// TestGit_EditingVerbsNeedNoEditor: `cherry-pick -e` and `revert --edit` open
// core.editor on a clean pick, with or without a terminal. Before the fix both
// failed with "cannot exec '…'" and left the operation half-done.
func TestGit_EditingVerbsNeedNoEditor(t *testing.T) {
	cases := []struct {
		verb        string
		args        func(topicSHA string) []string
		wantSubject string
	}{
		{"cherry-pick", func(sha string) []string { return []string{"-e", sha} }, "topic add"},
		{"revert", func(string) []string { return []string{"--edit", "HEAD"} }, `Revert "main change"`},
	}
	for _, tc := range cases {
		t.Run(tc.verb, func(t *testing.T) {
			neutraliseEditorEnv(t)
			requireGit(t)
			repo := initTestRepo(t)
			base := gwGit(t, repo, "branch", "--show-current")
			gwGit(t, repo, "switch", "-q", "-c", "topic")
			gwWrite(t, repo, "t.txt", "topic\n")
			gwGit(t, repo, "add", "t.txt")
			gwGit(t, repo, "commit", "-q", "-m", "topic add")
			topicSHA := gwGit(t, repo, "rev-parse", "HEAD")
			gwGit(t, repo, "switch", "-q", base)
			gwWrite(t, repo, "c.txt", "main\n")
			gwGit(t, repo, "add", "c.txt")
			gwGit(t, repo, "commit", "-q", "-m", "main change")
			gwGit(t, repo, "config", "core.editor", missingEditor)

			if out, err := callGit(t, gwTool(nil), map[string]any{"subcommand": tc.verb, "args": tc.args(topicSHA), "confirm": true, "repo": repo}); err != nil {
				t.Fatalf("git %s through the tool tried to open an editor: %v\n%s", tc.verb, err, out)
			}
			if got := gwGit(t, repo, "log", "-1", "--format=%s"); got != tc.wantSubject {
				t.Errorf("HEAD's subject = %q, want git's prepared message %q", got, tc.wantSubject)
			}
		})
	}
}

// TestGit_InteractiveRebaseRunsTheTodoAsWritten: `rebase -i` opens the
// sequence editor before anything else. sequence.editor is set to one that
// cannot run, and that config beats GIT_EDITOR — so only GIT_SEQUENCE_EDITOR=true
// gets past it, running the todo list git wrote, i.e. a plain replay.
func TestGit_InteractiveRebaseRunsTheTodoAsWritten(t *testing.T) {
	neutraliseEditorEnv(t)
	repo, _, _ := conflictRepo(t)
	gwGit(t, repo, "config", "sequence.editor", missingEditor)
	before := gwGit(t, repo, "log", "--format=%s", "-2")
	tool := gwTool(nil)
	if out, err := callGit(t, tool, map[string]any{"subcommand": "rebase", "args": []string{"-i", "HEAD~2"}, "confirm": true, "repo": repo}); err != nil {
		t.Fatalf("rebase -i through the tool tried to open an editor: %v\n%s", err, out)
	}
	if after := gwGit(t, repo, "log", "--format=%s", "-2"); after != before {
		t.Errorf("rebase -i changed the history: %q, want %q", after, before)
	}
}

// TestGit_InheritedEditorIsNotUsed: a GIT_EDITOR the daemon inherited is the
// user's interactive editor from whatever shell started it, not a choice about
// plumb — unlike GOWORK, the default beats it. Without that, a user who exports
// GIT_EDITOR=nvim still hits #544 however core.editor is set.
func TestGit_InheritedEditorIsNotUsed(t *testing.T) {
	neutraliseEditorEnv(t)
	repo, base, _ := conflictRepo(t)
	gwGit(t, repo, "config", "--unset", "core.editor")
	t.Setenv("GIT_EDITOR", missingEditor)
	gwGit(t, repo, "switch", "-q", "topic")
	tool := gwTool(nil)

	if _, err := callGit(t, tool, map[string]any{"subcommand": "rebase", "args": []string{base}, "confirm": true, "repo": repo}); err == nil {
		t.Fatal("the rebase was expected to stop on a conflict")
	}
	resolveConflict(t, repo)
	if out, err := callGit(t, tool, map[string]any{"subcommand": "rebase", "args": []string{"--continue"}, "confirm": true, "repo": repo}); err != nil {
		t.Fatalf("rebase --continue ran the inherited GIT_EDITOR: %v\n%s", err, out)
	}
}

// TestGit_ConfiguredEditorWins: a GIT_EDITOR set under [git] env is a deliberate
// choice for plumb's children, so it beats the default — the same precedence
// [git] env GOWORK has over the automatic GOWORK=off. The configured editor
// rewrites the subject, so the commit shows which editor ran.
func TestGit_ConfiguredEditorWins(t *testing.T) {
	neutraliseEditorEnv(t)
	repo, base, _ := conflictRepo(t)
	gwGit(t, repo, "switch", "-q", "topic")
	editor := filepath.Join(t.TempDir(), "editor.sh")
	gwWrite(t, filepath.Dir(editor), filepath.Base(editor),
		"#!/bin/sh\nprintf 'edited by the configured editor\\n' > \"$1\"\n")
	if err := os.Chmod(editor, 0o755); err != nil { //nolint:gosec // G302: the test editor must be executable
		t.Fatal(err)
	}
	tool := gwTool(map[string]string{"GIT_EDITOR": editor})

	if _, err := callGit(t, tool, map[string]any{"subcommand": "rebase", "args": []string{base}, "confirm": true, "repo": repo}); err == nil {
		t.Fatal("the rebase was expected to stop on a conflict")
	}
	resolveConflict(t, repo)
	if out, err := callGit(t, tool, map[string]any{"subcommand": "rebase", "args": []string{"--continue"}, "confirm": true, "repo": repo}); err != nil {
		t.Fatalf("rebase --continue: %v\n%s", err, out)
	}
	if got := gwGit(t, repo, "log", "-1", "--format=%s"); got != "edited by the configured editor" {
		t.Errorf("HEAD's subject = %q: the [git] env GIT_EDITOR did not run in place of plumb's default", got)
	}
}

// TestGit_ChildSeesTheNonInteractiveEnv reads, from inside a hook the git child
// runs, the three variables plumb sets — with conflicting values inherited, so a
// default that lost to the daemon's environment shows up here. The probe is a
// pre-rebase hook rather than pre-commit because git itself runs commit hooks
// with GIT_EDITOR=: when it will not open an editor, which would hide ours.
func TestGit_ChildSeesTheNonInteractiveEnv(t *testing.T) {
	requireGit(t)
	neutraliseEditorEnv(t)
	t.Setenv("GIT_EDITOR", missingEditor)
	t.Setenv("GIT_SEQUENCE_EDITOR", missingEditor)
	t.Setenv("GIT_TERMINAL_PROMPT", "1")
	repo := initTestRepo(t)
	base := gwGit(t, repo, "branch", "--show-current")
	gwGit(t, repo, "switch", "-q", "-c", "side")
	gwWrite(t, repo, "side.txt", "side\n")
	gwGit(t, repo, "add", "side.txt")
	gwGit(t, repo, "commit", "-q", "-m", "side")
	gwGit(t, repo, "switch", "-q", base)

	record := filepath.Join(t.TempDir(), "seen")
	hook := filepath.Join(repo, ".git", "hooks", "pre-rebase")
	gwWrite(t, filepath.Dir(hook), filepath.Base(hook),
		"#!/bin/sh\nprintf 'GIT_EDITOR=%s\\nGIT_SEQUENCE_EDITOR=%s\\nGIT_TERMINAL_PROMPT=%s\\n' "+
			"\"$GIT_EDITOR\" \"$GIT_SEQUENCE_EDITOR\" \"$GIT_TERMINAL_PROMPT\" > '"+record+"'\n")
	if err := os.Chmod(hook, 0o755); err != nil { //nolint:gosec // G302: a hook must be executable
		t.Fatal(err)
	}
	if out, err := callGit(t, gwTool(nil), map[string]any{"subcommand": "rebase", "args": []string{"side"}, "confirm": true, "repo": repo}); err != nil {
		t.Fatalf("git rebase: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the pre-rebase hook did not run: %v", err)
	}
	want := "GIT_EDITOR=true\nGIT_SEQUENCE_EDITOR=true\nGIT_TERMINAL_PROMPT=0\n"
	if string(raw) != want {
		t.Errorf("the git child's environment:\n%s\nwant:\n%s", raw, want)
	}
}

// TestGitChildEnv_NonInteractiveDefaults pins the builder: the defaults are
// present with nothing configured, replace an inherited value rather than
// duplicate it, and lose to a [git] env entry of the same name.
func TestGitChildEnv_NonInteractiveDefaults(t *testing.T) {
	t.Setenv("GIT_EDITOR", "nvim")
	for _, overrides := range []map[string]string{nil, {}} {
		env := gitChildEnv(overrides)
		for _, kv := range gitNonInteractiveEnv() {
			if got, ok := lookupEnvEntry(env, kv[0]); !ok || got != kv[1] {
				t.Errorf("overrides %v: %s = %q (present=%v), want %q", overrides, kv[0], got, ok, kv[1])
			}
			if n := countEnvEntries(env, kv[0]); n != 1 {
				t.Errorf("overrides %v: %s appears %d times, want exactly 1", overrides, kv[0], n)
			}
		}
		if _, ok := lookupEnvEntry(env, "PATH"); !ok {
			t.Error("the inherited PATH must survive")
		}
	}
	env := gitChildEnv(map[string]string{"GIT_EDITOR": "my-editor", "GIT_TERMINAL_PROMPT": "1"})
	if got, _ := lookupEnvEntry(env, "GIT_EDITOR"); got != "my-editor" || countEnvEntries(env, "GIT_EDITOR") != 1 {
		t.Errorf("a [git] env GIT_EDITOR must win once; got %q ×%d", got, countEnvEntries(env, "GIT_EDITOR"))
	}
	if got, _ := lookupEnvEntry(env, "GIT_TERMINAL_PROMPT"); got != "1" {
		t.Errorf("a [git] env GIT_TERMINAL_PROMPT must win; got %q", got)
	}
	if got, _ := lookupEnvEntry(env, "GIT_SEQUENCE_EDITOR"); got != "true" {
		t.Errorf("a default the overrides do not name must survive; GIT_SEQUENCE_EDITOR = %q", got)
	}
}

// TestExecGitCmd_NilEnvGetsTheDefaults: a git child whose spec left Env nil (a
// hand-built gitChildSpec) must not fall back to the daemon's environment and
// its editor. `git var GIT_EDITOR` prints the editor git would run.
func TestExecGitCmd_NilEnvGetsTheDefaults(t *testing.T) {
	requireGit(t)
	neutraliseEditorEnv(t)
	t.Setenv("GIT_EDITOR", missingEditor)
	dir := initTestRepo(t)
	var stdout bytes.Buffer
	cmd := exec.CommandContext(t.Context(), "git", "var", "GIT_EDITOR")
	cmd.Dir = dir
	cmd.Stdout = &stdout
	if _, err := execGitCmd(cmd, false, dir); err != nil {
		t.Fatalf("git var: %v", err)
	}
	if got := strings.TrimSpace(stdout.String()); got != "true" {
		t.Errorf("a nil-Env git child resolved its editor to %q, want \"true\"", got)
	}
}

// TestGit_MergeNeedsNoEditor: merge arrived on main (#530) after #544 was
// written, and `git merge` opens the editor on a non-fast-forward merge when
// GIT_MERGE_AUTOEDIT=yes is exported — a variable an automation harness or a
// shell profile can leave in the daemon's environment. core.editor cannot run,
// so before the fix this merge failed with "cannot exec" instead of recording
// the two-parent commit with git's own message. `--no-edit` is the control:
// it overrides GIT_MERGE_AUTOEDIT, so it passes with or without the fix.
func TestGit_MergeNeedsNoEditor(t *testing.T) {
	for _, args := range [][]string{{"side"}, {"--no-edit", "side"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			neutraliseEditorEnv(t)
			repo := mergeFixture(t)
			runGitDirect(t, repo, "config", "core.editor", missingEditor)
			t.Setenv("GIT_MERGE_AUTOEDIT", "yes")

			out, err := callGit(t, writesOnlyGit(repo), map[string]any{"subcommand": "merge", "args": args})
			if err != nil {
				t.Fatalf("git merge %v through the tool tried to open an editor: %v\n%s", args, err, out)
			}
			if n := headParents(t, repo); n != 2 {
				t.Errorf("HEAD has %d parents after the merge, want 2", n)
			}
			if got := gwGit(t, repo, "log", "-1", "--format=%s"); got != "Merge branch 'side'" {
				t.Errorf("HEAD's subject = %q, want git's prepared merge message", got)
			}
		})
	}
}

// TestGit_AnnotatedTagWithoutAMessageFailsLoudly pins what the docs promise for
// a verb with no prepared message: `tag -a` without -m reaches the no-op editor
// with an empty template, and git refuses to tag rather than record a blank
// annotation. The caller sees git's own words and no tag exists afterwards.
func TestGit_AnnotatedTagWithoutAMessageFailsLoudly(t *testing.T) {
	neutraliseEditorEnv(t)
	repo := mergeFixture(t)
	runGitDirect(t, repo, "config", "core.editor", missingEditor)

	out, err := callGit(t, writesOnlyGit(repo), map[string]any{"subcommand": "tag", "args": []string{"-a", "v1"}})
	if err == nil {
		t.Fatalf("tag -a without a message succeeded:\n%s", out)
	}
	if !strings.Contains(err.Error(), "no tag message") {
		t.Errorf("the failure should be git's own refusal, not an editor error; got: %v", err)
	}
	if tags := gwGit(t, repo, "tag", "--list"); tags != "" {
		t.Errorf("a tag was created despite the refusal: %q", tags)
	}
}
