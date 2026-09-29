package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/plumbkit/plumb/internal/paths"
)

// mutationtest_root.go decides WHERE mutation_test's commands run.
//
// The stored compile and test commands are resolved for the connection's
// workspace, so they run from that workspace's directory. A mutant in a git
// WORKTREE lives somewhere else entirely — typically inside the workspace
// directory, but in a different work-tree with its own copy of every file — and
// running the workspace's tests against it tests the other copy. Every mutant then
// reads SURVIVED with `compile ok` and `tests ok`, which is indistinguishable from
// "your assertions are vacuous": a verdict nothing stands behind, the failure mode
// the tool exists to prevent, reached through the directory instead of the mutant.
//
// The hazard is specific: a LINKED worktree (git worktree add) always has another
// copy of its files — the main work-tree, and any other worktree — that commands
// run from elsewhere reach instead. A file in the main work-tree of some other
// repository (a submodule, a sibling module a go.work or `make -C` reaches) has no
// such twin: commands that reach it at all reach it, as they always did.
//
// The rule, applied after preflight (files known and snapshotted, nothing mutated,
// nothing charged against the write budget) and before the baseline (nothing run),
// per command, against the work-tree its directory is in (the run tree):
//
//   - every mutated file is in the run tree, or in no git work-tree, or in the main
//     work-tree of a different repository: unchanged — the behaviour before
//     re-rooting existed, including the "no git safety net" warning;
//   - every mutated file is in ONE other work-tree of the run tree's repository:
//     the command runs from the same relative directory under that work-tree, and
//     the report says so;
//   - otherwise it is REFUSED before anything runs: files spread over several
//     work-trees, a file in a linked worktree the command cannot be moved into (of
//     another repository, or when the run directory is in none), a relative
//     directory the destination lacks or reaches through a symlink to somewhere
//     else, or a command argument naming an absolute path into the tree it would
//     be moved away from.
//
// A file in a SUBMODULE is placed by its outermost superproject: the submodule
// checked out inside a linked worktree is that worktree's copy, and its twin is the
// same submodule in the main checkout, however unrelated the submodule's own git
// directory looks. So both the files and the commands' directory are seen from
// the outermost superproject's work-tree (gitProbes.placement), and a submodule in
// a worktree re-roots into that worktree like any other file in it.
//
// "Same repository" is git's own answer — the common git directory — not a path
// heuristic. Every question is put to git, never answered from a spelling: the
// work-tree roots and relative directories git prints are compared with each
// other, so a symlinked workspace, a case-insensitive volume or a relative
// common directory cannot make two spellings of one place disagree.
//
// When git cannot answer for a directory (a checkout owned by another user without
// a safe.directory entry, or no git on the daemon's PATH), the nearest .git is read
// directly instead (diskProbe) and the same rule applied to what it says. Such an
// answer is good enough to leave a command where it is — the behaviour before
// re-rooting existed, with preflight's "no git safety net" warning already saying
// git could not vouch for the file — but never to MOVE one: a run that would need
// re-rooting on a guess is refused with git's own reason.

// mutationReroot records that one resolved command was moved to another
// work-tree, for the report and for a baseline refusal to say where it ran.
//
// Concurrency: an immutable value once built.
type mutationReroot struct {
	// from is the work-tree root the command would have run in; dir is the
	// directory it runs in instead (the destination work-tree plus the same
	// relative directory).
	from, dir string
}

// planDirNote is runDirNote for a plan. A command that was re-rooted must not be
// described by runDirNote, which would call its directory the workspace root or
// the configured working_dir — both false for a directory the command was moved
// to, and the wrong place to send a reader looking for why a baseline is red.
func (t *MutationTest) planDirNote(ctx context.Context, plan mutationPlan, cmd TaskCommand, step int) string {
	if len(cmd.Steps) == 0 {
		return ""
	}
	for _, r := range plan.reroots {
		if r.dir != cmd.WorkingDir {
			continue
		}
		if step < 0 || step >= len(cmd.Steps) {
			step = 0
		}
		return fmt.Sprintf(" It ran `%s` in %s — the work-tree that contains the mutated file, not the workspace's (%s): "+
			"the stored commands were re-rooted there.", strings.Join(cmd.Steps[step], " "), r.dir, r.from)
	}
	return t.runDirNote(ctx, cmd, step)
}

// gitTree identifies the git work-tree that contains a directory, in git's own
// spelling of every path.
//
// Concurrency: an immutable value once built.
type gitTree struct {
	// top is the work-tree root, as git prints it (symlinks resolved).
	top string
	// prefix is the directory's path relative to top, as git prints it: "" at the
	// root, otherwise slash-separated with a trailing slash.
	prefix string
	// common is the repository's common git directory. Every work-tree of one
	// repository reports the same value; two repositories never do.
	common string
	// linked is true for a work-tree made by `git worktree add` (its git directory
	// is not the common one), false for a repository's main work-tree.
	linked bool
	// super is the work-tree root of the superproject when this work-tree is a
	// submodule checkout, as git prints it; "" otherwise.
	super string
}

// gitPlace is git's answer for one directory.
type gitPlace int

const (
	// placeTree: the directory is in a git work-tree, described by gitProbe.tree.
	placeTree gitPlace = iota
	// placeNoRepo: git says the directory is in no repository at all.
	placeNoRepo
	// placeUnknown: git could not answer; gitProbe.reason says why.
	placeUnknown
)

// gitProbe is the result of asking git about one directory.
//
// Concurrency: an immutable value once built.
type gitProbe struct {
	place gitPlace
	tree  gitTree
	// reason is git's own words when it could not answer. With approx set, place
	// and tree are diskProbe's reading of the .git link rather than git's answer.
	reason string
	approx bool
}

// gitNotARepository is how git starts the message for a directory in no
// repository, with or without a mount-point ceiling. The probe runs git with
// LC_ALL=C so the words are these whatever the daemon's locale.
const gitNotARepository = "not a git repository (or any "

// probeGitDir asks git which work-tree contains dir, in ONE rev-parse call.
// Errors that are not "no repository" come back as placeUnknown with git's own
// words, never as "no repository": a checkout git refuses to read (dubious
// ownership) is not the same thing as a directory outside every checkout.
func probeGitDir(ctx context.Context, dir string) gitProbe {
	cmd := exec.CommandContext(ctx, "git", gitNoOptionalLocks, "-C", dir, "rev-parse", //nolint:gosec // G204: argv is package literals plus a directory plumb resolved itself, passed as -C's value
		"--show-toplevel", "--show-prefix", "--git-dir", "--git-common-dir", "--show-superproject-working-tree")
	cmd.Env = withEnvVar(os.Environ(), "LC_ALL", "C")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			msg := strings.TrimSpace(string(ee.Stderr))
			if strings.Contains(msg, gitNotARepository) {
				return gitProbe{place: placeNoRepo}
			}
			return gitProbe{place: placeUnknown, reason: gitFirstLine(msg)}
		}
		return gitProbe{place: placeUnknown, reason: err.Error()}
	}
	// Four lines — the second empty at the work-tree root — plus a fifth naming the
	// superproject for a submodule (git prints nothing for that option otherwise):
	// split, do not trim.
	lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if (len(lines) != 4 && len(lines) != 5) || !filepath.IsAbs(lines[0]) || lines[2] == "" || lines[3] == "" ||
		(len(lines) == 5 && !filepath.IsAbs(lines[4])) {
		return gitProbe{place: placeUnknown, reason: fmt.Sprintf("unexpected `git rev-parse` output %q", out)}
	}
	// git prints --git-dir and --git-common-dir relative to its working directory
	// when it can, and that directory is dir with every symlink resolved — the
	// kernel's view after chdir — so a relative answer is joined to that, never to
	// dir as spelled ("../../.git" from under an in-repo symlink names a different
	// repository when joined to the link's spelling).
	physical := paths.Canonical(dir)
	abs := func(p string) string {
		if !filepath.IsAbs(p) {
			p = filepath.Join(physical, p)
		}
		return paths.Canonical(p)
	}
	gitDir, common := abs(lines[2]), abs(lines[3])
	tree := gitTree{top: filepath.Clean(lines[0]), prefix: lines[1], common: common, linked: gitDir != common}
	if len(lines) == 5 {
		tree.super = filepath.Clean(lines[4])
	}
	return gitProbe{place: placeTree, tree: tree}
}

// gitFirstLine returns s up to its first newline, uncut: git's first line names the
// path it is complaining about, and that is the part worth keeping.
func gitFirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// gitProbes memoises probeGitDir for one rerootPlan call, so twenty mutants in one
// package cost one git process, not twenty, and the compile and test commands —
// which normally share a directory — cost one between them.
//
// Concurrency: not safe for concurrent use; one rerootPlan call owns it.
type gitProbes map[string]gitProbe

func (g gitProbes) of(ctx context.Context, dir string) gitProbe {
	if p, ok := g[dir]; ok {
		return p
	}
	p := probeGitDir(ctx, dir)
	if p.place == placeUnknown && ctx.Err() == nil {
		p = diskProbe(dir, p.reason)
	}
	g[dir] = p
	return p
}

// maxSubmoduleDepth bounds the climb through nested superprojects.
const maxSubmoduleDepth = 16

// placement is git's answer for dir seen from its OUTERMOST superproject (see the
// file comment): a directory in a submodule is reported as the superproject's
// work-tree, with the prefix extended by the submodule's path inside it. A
// superproject git cannot identify makes the answer unknown — the submodule's own
// answer would hide the very twin the climb exists to find.
func (g gitProbes) placement(ctx context.Context, dir string) gitProbe {
	p := g.of(ctx, dir)
	for depth := 0; p.place == placeTree && p.tree.super != ""; depth++ {
		sub := p.tree
		sp := g.of(ctx, sub.super)
		rel, err := filepath.Rel(sp.tree.top, sub.top)
		switch {
		case depth == maxSubmoduleDepth:
			return gitProbe{place: placeUnknown, reason: "submodules nested more than " + strconv.Itoa(maxSubmoduleDepth) + " deep"}
		case sp.place != placeTree:
			return gitProbe{place: placeUnknown, reason: firstNonEmpty(sp.reason, "git could not identify "+sub.super+", the superproject of "+sub.top)}
		case err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)):
			return gitProbe{place: placeUnknown, reason: "the submodule " + sub.top + " is not inside its superproject " + sp.tree.top}
		}
		sp.tree.prefix = filepath.ToSlash(rel) + "/" + sub.prefix // sp is a copy: the memo keeps git's own answer
		sp.approx = sp.approx || p.approx
		p = sp
	}
	return p
}

// commandDir is the directory a resolved command runs in: its working_dir, else
// the workspace root. runStep, runDirNote and rerootPlan all use it, so the
// directory re-rooting reasons about is the one the command really runs in.
func (t *MutationTest) commandDir(ctx context.Context, cmd TaskCommand) string {
	if cmd.WorkingDir != "" {
		return cmd.WorkingDir
	}
	if t.deps.WorkspaceFn != nil {
		return t.deps.WorkspaceFn(ctx)
	}
	return ""
}

// rerootPlan applies the rule in the file comment to both commands of the plan.
// It returns the plan unchanged when every file is where the commands run.
func (t *MutationTest) rerootPlan(ctx context.Context, plan mutationPlan, targets []mutationTarget) (mutationPlan, error) {
	probes := gitProbes{}
	files := make([]gitProbe, len(targets))
	for i, tgt := range targets {
		files[i] = probes.placement(ctx, filepath.Dir(tgt.path))
	}
	if err := ctx.Err(); err != nil {
		return plan, fmt.Errorf("mutation_test: cancelled while finding the work-tree of each mutated file; nothing was run or mutated: %w", err)
	}
	for _, cmd := range []*TaskCommand{&plan.compile, &plan.test} {
		dir := t.commandDir(ctx, *cmd)
		if dir == "" {
			continue // no directory to reason about; runStep runs it as it always has
		}
		run := probes.placement(ctx, dir)
		if err := ctx.Err(); err != nil {
			return plan, fmt.Errorf("mutation_test: cancelled while finding the work-tree of the commands' directory; nothing was run or mutated: %w", err)
		}
		dest, err := mutantsTree(targets, files, dir, run)
		if err != nil {
			return plan, err
		}
		if dest == nil {
			continue
		}
		moved, err := rerootCommand(ctx, probes, *cmd, dir, run.tree, *dest, targets[0].display)
		if err != nil {
			return plan, err
		}
		*cmd = moved
		plan.reroots = append(plan.reroots, mutationReroot{from: run.tree.top, dir: moved.WorkingDir})
	}
	return plan, nil
}

// mutantsTree returns the work-tree the command running in dir (git's answer: run)
// must be moved to, nil when it stays where it is, or a refusal saying why neither
// is safe. files[i] is git's answer for targets[i]'s directory.
func mutantsTree(targets []mutationTarget, files []gitProbe, dir string, run gitProbe) (*gitTree, error) {
	var dest *gitTree
	destFile := ""
	stays := "" // a file that keeps the command where it is, for the span refusal
	for i, f := range files {
		display := targets[i].display
		move, err := placeFile(f, display, dir, run)
		if err != nil {
			return nil, err
		}
		switch {
		case !move:
			stays = display
		case dest != nil && !sameGitPath(dest.top, f.tree.top):
			return nil, spanError(destFile, dest.top, display, f.tree.top)
		default:
			tree := f.tree
			dest, destFile = &tree, display
		}
		if dest != nil && stays != "" {
			return nil, spanError(destFile, dest.top, stays, "the tree the commands already run in, or none")
		}
	}
	return dest, nil
}

// placeFile applies the rule in the file comment to one mutated file (git's
// answer: f): move is true when the command must be moved to f's work-tree, false
// when the file is fine where the command already runs; an error is a refusal.
func placeFile(f gitProbe, display, dir string, run gitProbe) (move bool, err error) {
	switch {
	case f.place == placeUnknown:
		return false, fmt.Errorf("mutation_test: git could not identify the work-tree of %s (%s), and its .git could not be read either, "+
			"so there is no telling whether the commands in %s would test this copy of the file or another. The run is refused. "+
			"Nothing was run. Fix what git reports (for a checkout owned by another user, `git config --global --add safe.directory <path>`) "+
			"and retry", display, f.reason, dir)
	case f.place == placeNoRepo:
		return false, nil
	case run.place == placeUnknown:
		return false, fmt.Errorf("mutation_test: git could not identify the work-tree of the commands' directory %s (%s), so there is no "+
			"telling whether they would test %s or another copy of it. The run is refused. Nothing was run. Fix what git reports and retry",
			dir, run.reason, display)
	case run.place == placeTree && sameGitPath(f.tree.top, run.tree.top):
		return false, nil
	case run.place == placeTree && sameGitPath(f.tree.common, run.tree.common):
		if f.approx || run.approx {
			return false, fmt.Errorf("mutation_test: %s is in work-tree %s, not the one the commands run in (%s), so they would have to be "+
				"moved there — but git could not confirm either tree (%s), and plumb does not move trusted commands on a guess. "+
				"Nothing was run. Fix what git reports and retry", display, f.tree.top, dir, firstNonEmpty(f.reason, run.reason))
		}
		return true, nil
	case f.tree.linked:
		return false, strandedError(display, f.tree, dir, run)
	default:
		// The main work-tree of another repository: a submodule, a sibling
		// module. Nothing else holds a copy of it, so commands that reach it
		// test it, exactly as before re-rooting existed.
		return false, nil
	}
}

// firstNonEmpty returns the first of its arguments that is not "".
func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// sameGitPath reports whether two absolute paths git (or diskProbe) produced name
// the same place. They are compared exactly, except on macOS, whose default volume
// is case-insensitive while one spelling may carry the client's case and the
// other the disk's.
func sameGitPath(a, b string) bool {
	return a == b || (runtime.GOOS == "darwin" && strings.EqualFold(a, b))
}

// spanError refuses mutants that need the commands in two places at once.
func spanError(fileA, treeA, fileB, treeB string) error {
	return fmt.Errorf("mutation_test: the mutants span more than one git work-tree — %s is in %s but %s is in %s. "+
		"The stored commands run in one directory, so at least one of them would be tested against the wrong tree and read SURVIVED "+
		"whatever the tests assert. Nothing was run. Run one mutation_test call per work-tree", fileA, treeA, fileB, treeB)
}

// strandedError refuses a file in a linked worktree the commands cannot be moved
// into, because they do not run in that worktree's repository.
func strandedError(display string, tree gitTree, dir string, run gitProbe) error {
	where := "a directory in no git repository"
	switch run.place {
	case placeTree:
		where = "work-tree " + run.tree.top + ", which belongs to a different repository"
	case placeUnknown:
		where = "a directory git could not identify (" + run.reason + ")"
	case placeNoRepo:
	}
	return fmt.Errorf("mutation_test: %s is in the linked git worktree %s, but the commands run in %s (%s). "+
		"That worktree's files have another copy — its repository's main work-tree — which commands run from there would reach "+
		"instead, reading SURVIVED whatever the tests assert; and plumb moves the commands only between work-trees of the repository "+
		"they run in, never into a repository the workspace's trusted commands were not configured for. Nothing was run. "+
		"Open a session in %s (or point [tasks.<lang>] working_dir inside it)", display, tree.top, dir, where, tree.top)
}

// rerootCommand moves one resolved command from its directory in the from
// work-tree to the same relative directory in dest, refusing when that move is
// not safe. file names a mutated file for the messages.
func rerootCommand(ctx context.Context, probes gitProbes, cmd TaskCommand, dir string, from, dest gitTree, file string) (TaskCommand, error) {
	moved := filepath.Join(dest.top, filepath.FromSlash(from.prefix))
	rel := strings.TrimSuffix(from.prefix, "/")
	if rel == "" {
		rel = "."
	}
	if info, err := os.Stat(moved); err != nil || !info.IsDir() {
		return cmd, fmt.Errorf("mutation_test: %s is in work-tree %s, but the commands would have run in %s (relative directory %q under %s), "+
			"and that work-tree has no such directory. Running them elsewhere would not test this file. Nothing was run",
			file, dest.top, dir, rel, from.top)
	}
	// The same relative directory must BE that directory of the destination
	// work-tree once every symlink is followed: on another branch a component of it
	// can be a link leading back into the tree being left, or out of the
	// workspace altogether, and a command run there tests whatever it leads to.
	got := probes.placement(ctx, moved)
	if err := ctx.Err(); err != nil {
		return cmd, fmt.Errorf("mutation_test: cancelled while checking the directory the commands would move to; nothing was run or mutated: %w", err)
	}
	if got.place == placeUnknown || got.approx {
		return cmd, fmt.Errorf("mutation_test: %s is in work-tree %s, but git could not confirm that %s is that work-tree's %q (%s), "+
			"and plumb does not move trusted commands on a guess. Nothing was run. Fix what git reports and retry",
			file, dest.top, moved, rel, got.reason)
	}
	if got.place != placeTree || got.tree.top != dest.top || got.tree.prefix != from.prefix {
		return cmd, fmt.Errorf("mutation_test: %s is in work-tree %s, but the matching directory there, %s, resolves to %s — not "+
			"that work-tree's %q (a symlink on its branch?). Commands run there would test whatever it leads to. Nothing was run",
			file, dest.top, moved, paths.Canonical(moved), rel)
	}
	if arg, ok := argNamingTree(cmd, from.top, dest.top); ok {
		return cmd, fmt.Errorf("mutation_test: %s is in work-tree %s, but a stored command argument names a path in the tree it would be moved away from "+
			"(%q, under %s). Moving the working directory would leave that argument pointing at the wrong tree, so the run is refused. "+
			"Use a path relative to the working directory in the command or in test_target", file, dest.top, arg, from.top)
	}
	cmd.WorkingDir = moved
	return cmd, nil
}

// argNamingTree finds an argv element holding an absolute path that lies in the
// work-tree being left (fromTop) rather than the one being entered (destTop).
// "Lies in" is component-wise containment of the symlink-resolved path, and when
// one work-tree is nested inside the other the more specific root decides: from a
// main checkout into its nested worktree, a path in the worktree is fine and any
// other path under the main checkout — a sibling worktree included — is not; the
// other way round, a path in the worktree being left is refused even though the
// main checkout contains it too. Relative arguments move with the directory, which
// is the point, and are never refused.
//
// Absolute paths are looked for in the whole argument, after a flag's `=`, and in
// each element of a list-separator-joined value. A path spelled through a symlink
// is resolved first. Best effort by construction: a path assembled at run time
// (inside a script, or from a relative escape) cannot be seen from the argv.
func argNamingTree(cmd TaskCommand, fromTop, destTop string) (string, bool) {
	from, dest := paths.Canonical(fromTop), paths.Canonical(destTop)
	for _, argv := range cmd.Steps {
		for _, arg := range argv {
			for _, p := range absolutePathsIn(arg) {
				c := paths.Canonical(p)
				inFrom, inDest := pathWithin(from, c), pathWithin(dest, c)
				if inFrom && (!inDest || len(from) > len(dest)) {
					return arg, true
				}
			}
		}
	}
	return "", false
}

// absolutePathsIn returns every absolute path an argument spells: the argument
// itself, the value after a flag's first "=", and each element of either joined
// by the list separator.
func absolutePathsIn(arg string) []string {
	candidates := []string{arg}
	if _, v, ok := strings.Cut(arg, "="); ok {
		candidates = append(candidates, v)
	}
	var out []string
	for _, c := range candidates {
		for _, p := range filepath.SplitList(c) {
			if filepath.IsAbs(p) {
				out = append(out, p)
			}
		}
	}
	return out
}

// pathWithin is withinRoot for comparing a path with a work-tree root git
// printed. On macOS the default volume is case-insensitive while git prints the
// on-disk case, so a differently cased spelling of the same place must still
// count as inside; folding case can only make the check refuse more, never less.
func pathWithin(root, p string) bool {
	if runtime.GOOS == "darwin" {
		root, p = strings.ToLower(root), strings.ToLower(p)
	}
	return withinRoot(root, p)
}

// diskProbe reads the nearest .git at or above dir straight from the disk — the
// fallback for when git cannot answer (reason is git's words). A .git DIRECTORY
// marks a main work-tree whose common directory is that .git. A .git FILE is a
// link, "gitdir: <dir>": the linked directory belongs to a worktree exactly when
// it holds a `commondir` file naming the common directory (relative to it), which
// is how git itself tells a worktree's git directory from a submodule's. No .git
// anywhere above is no repository. A link that cannot be read is placeUnknown.
func diskProbe(dir, reason string) gitProbe {
	for d := paths.Canonical(dir); ; {
		dotGit := filepath.Join(d, ".git")
		if info, err := os.Lstat(dotGit); err == nil {
			tree, ok := diskTree(d, dotGit, info)
			if !ok {
				return gitProbe{place: placeUnknown, reason: reason}
			}
			return gitProbe{place: placeTree, tree: tree, reason: reason, approx: true}
		}
		parent := filepath.Dir(d)
		if parent == d {
			return gitProbe{place: placeNoRepo, reason: reason, approx: true}
		}
		d = parent
	}
}

// diskTree describes the work-tree rooted at top from its .git entry (info), or
// reports false when a .git link cannot be read.
func diskTree(top, dotGit string, info os.FileInfo) (gitTree, bool) {
	if info.IsDir() {
		return gitTree{top: top, common: paths.Canonical(dotGit)}, true
	}
	gitDir, ok := readGitLink(dotGit, top)
	if !ok {
		return gitTree{}, false
	}
	data, err := readGoConfigFile(filepath.Join(gitDir, "commondir"))
	if errors.Is(err, os.ErrNotExist) {
		// A submodule's git directory, not a worktree's — unless it is a submodule
		// checked out INSIDE a linked worktree (<common>/worktrees/<id>/modules/…),
		// whose twin is the main checkout's copy. Without git to name the
		// superproject, that shape is the evidence, and it counts as linked.
		return gitTree{top: top, common: gitDir, linked: inWorktreeModules(gitDir)}, true
	}
	if err != nil {
		return gitTree{}, false
	}
	common := strings.TrimSpace(string(data))
	if !filepath.IsAbs(common) {
		common = filepath.Join(gitDir, common)
	}
	return gitTree{top: top, common: paths.Canonical(common), linked: true}, true
}

// inWorktreeModules reports whether gitDir lies under a "worktrees/<id>/modules/"
// segment: the git directory of a submodule checked out in a linked worktree.
func inWorktreeModules(gitDir string) bool {
	parts := strings.Split(filepath.ToSlash(gitDir), "/")
	for i := 0; i+2 < len(parts); i++ {
		if parts[i] == "worktrees" && parts[i+2] == "modules" {
			return true
		}
	}
	return false
}

// readGitLink reads a .git file's "gitdir: <dir>" line and returns the directory
// it names, made absolute against dir (where the .git file is) and resolved.
func readGitLink(dotGit, dir string) (string, bool) {
	data, err := readGoConfigFile(dotGit)
	if err != nil {
		return "", false
	}
	target, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	if !ok {
		return "", false
	}
	target = strings.TrimSpace(target)
	if target == "" {
		return "", false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(dir, target)
	}
	return paths.Canonical(target), true
}
