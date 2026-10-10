package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// git_clean_clone.go implements `merge-tree`'s opt-in clean-clone preview
// (PLAN-454 gap 5): the answer a machine with no local git configuration and no
// system attributes would compute — what a fresh clone of the repository computes.
//
// Why it is needed: `git merge-tree` reads the LOCAL repository's merge drivers,
// including `.git/info/attributes` — a machine-local file no reviewer can see. A
// `merge=union` driver there turns a real conflict into a clean preview, and no
// git flag disables it (`-c core.attributesFile=` does not override
// info/attributes). That is exactly what hid plumbkit/plumb#588's CHANGELOG
// conflict: the local preview was clean while GitHub reported CONFLICTING. The
// working method was a throwaway bare clone by hand.
//
// The preview is that clone, made and discarded inside one call: a bare
// repository whose object store is an ALTERNATE of the real one, run with
// GIT_CONFIG_GLOBAL=/dev/null, GIT_CONFIG_NOSYSTEM=1 and GIT_ATTR_NOSYSTEM=1, so
// no local or system configuration and no system attributes apply. The real
// repository gains no object — `--write-tree` writes its result into the
// throwaway store, which the alternates file lets it read from the real one
// without writing back. An in-tree `.gitattributes` is still honoured, because
// that file is part of the tree under review and not machine state:
// GIT_ATTR_SOURCE names the first branch's commit, the tree being merged INTO.
//
// It is opt-in because it answers a different question. The default is "what does
// this machine's git say", which is the right answer for a local merge; the forge
// equivalent has to be asked for.
//
// It runs outside runGit's per-repository lock, ref guard and own-writes
// tracking deliberately: the command never writes to the real repository, so
// there is nothing to serialise against, and holding the lock for a throwaway
// read would queue real work behind it.

// runCleanClone runs a merge-tree preview in a throwaway clean clone of the
// repository holding a.Repo, applies the caller's output window, and returns the
// result with a note saying how it was produced.
func (t *Git) runCleanClone(ctx context.Context, a gitToolArgs, window gitWindow) (string, error) {
	root, err := findGitRoot(a.Repo)
	if err != nil {
		return "", fmt.Errorf("git merge-tree: %w", err)
	}
	args, attrSource := cleanCloneArgs(ctx, root, a.Args)
	dir, err := os.MkdirTemp("", "plumb-merge-preview-")
	if err != nil {
		return "", fmt.Errorf("git merge-tree: clean-clone preview: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := writeAlternateRepo(ctx, root, dir); err != nil {
		return "", err
	}
	body := cleanCloneNote(attrSource)
	out, runErr := runCleanCloneMergeTree(ctx, dir, args, attrSource)
	body += out
	if runErr != nil {
		// The note is part of the answer even when git reports a conflict, because
		// the conflict IS the answer this preview exists to surface. %w keeps git's own
		// report in the chain for a caller that classifies errors.
		return "", fmt.Errorf("%s%w", body, runErr)
	}
	if window.wanted() {
		windowed, werr := applyGitWindow(body, window)
		if werr != nil {
			return "", werr
		}
		return formatGitOutput("merge-tree", windowed, true), nil
	}
	return formatGitOutput("merge-tree", body, false), nil
}

// cleanCloneArgs resolves every revision the call names to a SHA, because the
// throwaway repository carries the objects but none of the refs, and reports the
// commit whose in-tree `.gitattributes` the preview should honour — the first
// branch named, the tree being merged into.
//
// Arguments that are not revisions (paths, plain words) and flag values are
// passed through unchanged: only positionals and `--merge-base=<rev>` name a
// revision in this subcommand's grammar.
func cleanCloneArgs(ctx context.Context, root string, args []string) (out []string, attrSource string) {
	out = make([]string, len(args))
	positional := 0
	for i := 0; i < len(args); i++ {
		arg := args[i]
		out[i] = arg
		if arg == "--" {
			break // after -- everything is a path, never a revision
		}
		if base, ok := strings.CutPrefix(arg, "--merge-base="); ok {
			if sha, resolved := revParseCommit(ctx, root, base); resolved {
				out[i] = "--merge-base=" + sha
			}
			continue
		}
		if optionTakesValue(arg) {
			// The value belongs to the option, not to the merge: a strategy option's
			// value must not be rev-parsed just because a ref of that name exists.
			i = consumeOptionValue(ctx, root, args, out, i)
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		sha, resolved := revParseCommit(ctx, root, arg)
		if !resolved {
			continue
		}
		out[i] = sha
		positional++
		if positional == 1 {
			attrSource = sha
		}
	}
	return out, attrSource
}

// consumeOptionValue handles an option that takes its value as a separate argument:
// the value is stepped over, and resolved only when the option is --merge-base,
// which is the one value in merge-tree's grammar that IS a revision. It returns the
// index of the value so the caller's loop advances past it.
func consumeOptionValue(ctx context.Context, root string, args, out []string, i int) int {
	if i+1 >= len(args) {
		return i
	}
	if args[i] == "--merge-base" {
		if sha, resolved := revParseCommit(ctx, root, args[i+1]); resolved {
			out[i+1] = sha
		}
	}
	return i + 1
}

// optionTakesValue reports whether a merge-tree option consumes the next argument.
// A joined form (`-Xours`, `--strategy-option=ours`) needs no entry here: it is one
// argument and takes no separate value.
func optionTakesValue(arg string) bool {
	switch arg {
	case "-X", "--strategy-option", "--merge-base":
		return true
	}
	return false
}

// revParseCommit resolves rev to a commit SHA in root, reporting false when it is
// not a revision at all (a path, a flag value) rather than failing the call.
func revParseCommit(ctx context.Context, root, rev string) (string, bool) {
	cmd := exec.CommandContext(ctx, "git", gitNoOptionalLocks, "-C", root, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	sha := strings.TrimSpace(string(out))
	return sha, sha != ""
}

// writeAlternateRepo creates a bare repository at dir whose object store is an
// alternate of root's, so every object the merge needs is readable and nothing is
// written back.
func writeAlternateRepo(ctx context.Context, root, dir string) error {
	// --template= (empty) suppresses every init template, including the one
	// GIT_TEMPLATE_DIR names, so a template cannot seed the "clean" clone with an
	// info/attributes of its own. The init runs in cleanCloneEnv for the same reason
	// the merge does (review round 2, S4-2).
	init := exec.CommandContext(ctx, "git", "init", "--bare", "-q", "--template=", dir)
	init.Env = cleanCloneEnv("")
	if out, err := init.CombinedOutput(); err != nil {
		return fmt.Errorf("git merge-tree: clean-clone preview: creating the preview repository: %w: %s",
			err, strings.TrimSpace(string(out)))
	}
	cmd := exec.CommandContext(ctx, "git", gitNoOptionalLocks, "-C", root, "rev-parse", "--git-path", "objects")
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("git merge-tree: clean-clone preview: locating this repository's object store: %w", err)
	}
	objects := strings.TrimSpace(string(out))
	if !filepath.IsAbs(objects) {
		objects = filepath.Join(root, objects)
	}
	info := filepath.Join(dir, "objects", "info")
	if err := os.MkdirAll(info, 0o755); err != nil {
		return fmt.Errorf("git merge-tree: clean-clone preview: %w", err)
	}
	if err := os.WriteFile(filepath.Join(info, "alternates"), []byte(objects+"\n"), 0o600); err != nil {
		return fmt.Errorf("git merge-tree: clean-clone preview: %w", err)
	}
	return nil
}

// runCleanCloneMergeTree runs `git merge-tree` in the throwaway repository. A
// conflict (exit 1) is returned as an error carrying git's own report, exactly as
// the ordinary path reports one — the report is the answer, not a tool failure.
func runCleanCloneMergeTree(ctx context.Context, dir string, args []string, attrSource string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"merge-tree"}, args...)...) //nolint:gosec // G204: argv is the caller's already-classified merge-tree arguments
	cmd.Dir = dir
	cmd.Env = cleanCloneEnv(attrSource)
	out, err := cmd.CombinedOutput()
	if err != nil {
		report := strings.TrimSpace(string(out))
		if report == "" {
			return "", fmt.Errorf("clean-clone merge-tree: %w", err)
		}
		return "", errors.New(report)
	}
	return string(out), nil
}

// cleanCloneEnv is the child's environment: the daemon's, with EVERY GIT_*
// variable removed except GIT_EXEC_PATH (which locates git's own helpers and
// carries no configuration), then the settings that make the clone clean.
//
// The first version removed a denylist of names, and review round 2 was right to
// refuse it: GIT_CONFIG_PARAMETERS and GIT_CONFIG_COUNT/KEY_n/VALUE_n inject
// configuration exactly as effectively as GIT_CONFIG_GLOBAL, and a denylist only
// ever knows the names someone thought of. GIT_ATTR_SOURCE is set when a tree was
// resolved, so an in-tree `.gitattributes` still applies.
func cleanCloneEnv(attrSource string) []string {
	env := make([]string, 0, len(os.Environ())+4)
	for _, kv := range os.Environ() {
		if name, _, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(name, "GIT_") && name != "GIT_EXEC_PATH" {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_ATTR_NOSYSTEM=1")
	if attrSource != "" {
		env = append(env, "GIT_ATTR_SOURCE="+attrSource)
	}
	return env
}

// cleanCloneNote says how the preview was produced, because a clean-clone answer
// that differs from the ordinary one must not look like a plumb bug.
func cleanCloneNote(attrSource string) string {
	note := "# plumb-note: previewed in a clean clone — no local or system git config or attributes"
	if attrSource != "" {
		if len(attrSource) > 7 {
			attrSource = attrSource[:7]
		}
		note += ", in-tree .gitattributes from " + attrSource
	}
	return note + ".\n"
}
