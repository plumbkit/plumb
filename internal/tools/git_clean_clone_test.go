package tools

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// git_clean_clone_test.go covers merge-tree's opt-in clean-clone preview
// (PLAN-454 gap 5): the answer a machine with no local git configuration and no
// system attributes would compute, which is the answer a hosted forge computes.

// cleanCloneFixture builds the scenario the feature exists for: two branches that
// both edit one file, and a MACHINE-LOCAL `merge=union` driver for it in
// .git/info/attributes. The ordinary preview is clean; a clean clone reports the
// conflict GitHub would (plumbkit/plumb#588's CHANGELOG trap).
func cleanCloneFixture(t *testing.T) (repo, base, topic string) {
	t.Helper()
	requireGit(t)
	repo = initTestRepo(t)
	base = strings.TrimSpace(gitReadOutput(t, repo, "symbolic-ref", "--short", "HEAD"))
	topic = "topic"

	file := filepath.Join(repo, "CHANGELOG.md")
	if err := os.WriteFile(file, []byte("## Unreleased\n- base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "CHANGELOG.md")
	gitRun(t, repo, "commit", "-qm", "base")

	gitRun(t, repo, "checkout", "-qb", topic)
	if err := os.WriteFile(file, []byte("## Unreleased\n- topic\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "CHANGELOG.md")
	gitRun(t, repo, "commit", "-qm", "topic")

	gitRun(t, repo, "checkout", "-q", base)
	if err := os.WriteFile(file, []byte("## Unreleased\n- main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "CHANGELOG.md")
	gitRun(t, repo, "commit", "-qm", "main")

	// The machine-local driver: union takes both sides, so this machine sees a
	// clean merge while a fresh clone sees a conflict.
	if err := os.MkdirAll(filepath.Join(repo, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "info", "attributes"),
		[]byte("CHANGELOG.md merge=union\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo, base, topic
}

func mergeTreeCall(t *testing.T, tool *Git, repo, base, topic string, cleanClone bool) (string, error) {
	t.Helper()
	args := map[string]any{
		"subcommand": "merge-tree",
		"args":       []string{"--write-tree", base, topic},
		"repo":       repo,
	}
	if cleanClone {
		args["clean_clone"] = true
	}
	return callGit(t, tool, args)
}

// countLooseObjects counts the files in the repository's object store — the
// witness for "the preview wrote nothing into the real repository".
func countLooseObjects(t *testing.T, repo string) int {
	t.Helper()
	root := filepath.Join(repo, ".git", "objects")
	n := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return n
}

func TestGit_MergeTreeCleanCloneShowsTheConflictTheLocalDriverHides(t *testing.T) {
	repo, base, topic := cleanCloneFixture(t)
	tool := NewGit(WriteDeps{}, func() GitPolicy { return GitPolicy{} })

	out, err := mergeTreeCall(t, tool, repo, base, topic, false)
	if err != nil {
		t.Fatalf("ordinary merge-tree: %v", err)
	}
	if strings.Contains(out, "CONFLICT") {
		t.Fatalf("the machine-local union driver should have hidden the conflict in the ordinary preview:\n%s", out)
	}

	out, err = mergeTreeCall(t, tool, repo, base, topic, true)
	if err == nil {
		t.Fatalf("the clean-clone preview must report the conflict a fresh clone sees:\n%s", out)
	}
	for _, want := range []string{"CONFLICT", "plumb-note: previewed in a clean clone"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("clean-clone answer is missing %q:\n%v", want, err)
		}
	}
	if !strings.Contains(err.Error(), "in-tree .gitattributes from") {
		t.Errorf("the note must say which tree's attributes applied:\n%v", err)
	}
}

func TestGit_MergeTreeCleanCloneWritesNothingToTheRealRepository(t *testing.T) {
	repo, base, topic := cleanCloneFixture(t)
	tool := NewGit(WriteDeps{}, func() GitPolicy { return GitPolicy{} })

	before := countLooseObjects(t, repo)
	if _, err := mergeTreeCall(t, tool, repo, base, topic, true); err == nil {
		// A conflict is the expected result here; a clean one still exercises the path.
		t.Log("clean-clone merge reported no conflict")
	}
	if after := countLooseObjects(t, repo); after != before {
		t.Errorf("the clean-clone preview changed the real object store: %d -> %d files", before, after)
	}
}

// TestGit_MergeTreeCleanCloneHonoursInTreeAttributes pins the other half of the
// feature's contract: an attribute committed in the tree IS part of the review, so
// it must still apply — only machine-local and system attributes are dropped.
func TestGit_MergeTreeCleanCloneHonoursInTreeAttributes(t *testing.T) {
	repo, base, topic := cleanCloneFixture(t)
	if err := os.Remove(filepath.Join(repo, ".git", "info", "attributes")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("CHANGELOG.md merge=union\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", ".gitattributes")
	gitRun(t, repo, "commit", "-qm", "in-tree union driver")

	tool := NewGit(WriteDeps{}, func() GitPolicy { return GitPolicy{} })
	out, err := mergeTreeCall(t, tool, repo, base, topic, true)
	if err != nil {
		t.Fatalf("an in-tree driver must apply in the clean clone: %v", err)
	}
	if strings.Contains(out, "CONFLICT") {
		t.Errorf("the in-tree union driver should have merged cleanly:\n%s", out)
	}
}

// TestGit_CleanCloneIgnoresGITConfigParameters is review round 2's S4-3: a denylist
// of GIT_* names cannot hold, because GIT_CONFIG_PARAMETERS injects configuration
// just as effectively as GIT_CONFIG_GLOBAL. The injected config points core's
// attributes file at a union driver — exactly what the machine-local driver does —
// so a leak would make the clean clone report a clean merge.
func TestGit_CleanCloneIgnoresGITConfigParameters(t *testing.T) {
	repo, base, topic := cleanCloneFixture(t)
	hostile := filepath.Join(t.TempDir(), "hostile-attributes")
	if err := os.WriteFile(hostile, []byte("CHANGELOG.md merge=union\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_PARAMETERS", "'core.attributesFile="+hostile+"'")

	tool := NewGit(WriteDeps{}, func() GitPolicy { return GitPolicy{} })
	if _, err := mergeTreeCall(t, tool, repo, base, topic, true); err == nil {
		t.Error("injected configuration reached the clean clone: the conflict disappeared")
	}
}

// TestGit_CleanCloneIgnoresAnInitTemplate is S4-2: an init template can seed the
// throwaway repository with an info/attributes of its own, and GIT_TEMPLATE_DIR is
// how one is pointed at without the caller writing it into their repository.
func TestGit_CleanCloneIgnoresAnInitTemplate(t *testing.T) {
	repo, base, topic := cleanCloneFixture(t)
	tpl := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tpl, "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tpl, "info", "attributes"),
		[]byte("CHANGELOG.md merge=union\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_TEMPLATE_DIR", tpl)

	tool := NewGit(WriteDeps{}, func() GitPolicy { return GitPolicy{} })
	if _, err := mergeTreeCall(t, tool, repo, base, topic, true); err == nil {
		t.Error("an init template reached the clean clone: the conflict disappeared")
	}
}

// TestGit_CleanCloneOptionGrammar is S4-4: a strategy option is not a revision, and
// a two-word `--merge-base <rev>` is resolved WITHOUT becoming the branch whose
// attributes apply. (git's merge-tree accepts the strategy option only joined —
// `-Xours` / `--strategy-option=ours`; a separate word is "unknown strategy option",
// which the first case pins, along with a ref that shares the strategy's name.)
func TestGit_CleanCloneOptionGrammar(t *testing.T) {
	repo, base, topic := cleanCloneFixture(t)
	gitRun(t, repo, "tag", "ours", topic) // a ref that shares the strategy's name

	tool := NewGit(WriteDeps{}, func() GitPolicy { return GitPolicy{} })
	baseSha := strings.TrimSpace(gitReadOutput(t, repo, "rev-parse", base))
	topicSha := strings.TrimSpace(gitReadOutput(t, repo, "rev-parse", topic))

	out, err := callGit(t, tool, map[string]any{
		"subcommand":  "merge-tree",
		"args":        []string{"--write-tree", "-Xours", base, topic},
		"repo":        repo,
		"clean_clone": true,
	})
	if err != nil {
		t.Fatalf("-Xours must resolve the conflict, not be read as a revision: %v", err)
	}
	if !strings.Contains(out, baseSha[:7]) {
		t.Errorf("the in-tree attributes must come from the first branch (%s):\n%s", baseSha[:7], out)
	}
	if strings.Contains(out, topicSha[:7]) {
		t.Errorf("the ref named after the strategy was taken for the branch (%s):\n%s", topicSha[:7], out)
	}

	out, err = callGit(t, tool, map[string]any{
		"subcommand":  "merge-tree",
		"args":        []string{"--write-tree", "--merge-base", base, topic, topic},
		"repo":        repo,
		"clean_clone": true,
	})
	if err != nil {
		t.Fatalf("a two-word --merge-base must be resolved: %v", err)
	}
	if !strings.Contains(out, topicSha[:7]) {
		t.Errorf("with a merge base given, the attributes must come from the first BRANCH (%s):\n%s", topicSha[:7], out)
	}
}

func TestGit_CleanCloneAppliesToMergeTreeOnly(t *testing.T) {
	repo, _, _ := cleanCloneFixture(t)
	tool := NewGit(WriteDeps{}, func() GitPolicy { return GitPolicy{} })
	_, err := callGit(t, tool, map[string]any{"subcommand": "status", "clean_clone": true, "repo": repo})
	if err == nil || !strings.Contains(err.Error(), "applies to merge-tree") {
		t.Fatalf("clean_clone off merge-tree = %v, want a refusal naming merge-tree", err)
	}
}

func TestGitSchema_AdvertisesCleanClone(t *testing.T) {
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(gitSchema, &schema); err != nil {
		t.Fatalf("gitSchema is not valid JSON: %v", err)
	}
	if _, ok := schema.Properties["clean_clone"]; !ok {
		t.Error("git schema does not advertise clean_clone")
	}
}
