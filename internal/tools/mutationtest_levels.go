package tools

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
)

// mutationtest_levels.go compares directories repository by repository: each
// directory's chain of answers from its own work-tree out to its outermost
// superproject, and the innermost repository two chains share. See
// mutationtest_root.go's file comment for the rule these serve.

// maxSubmoduleDepth bounds the climb through nested superprojects.
const maxSubmoduleDepth = 16

// placement is git's answer for dir seen from its OUTERMOST superproject (see the
// file comment): the last element of its chain.
func (g gitProbes) placement(ctx context.Context, dir string) gitProbe {
	c := g.chain(ctx, dir)
	return c[len(c)-1]
}

// chain is git's answer for dir followed by its answer at each enclosing
// superproject, innermost first: a directory in a submodule is reported as the
// superproject's work-tree, with the prefix extended by the submodule's path
// inside it. A superproject git cannot identify ends the chain with an unknown
// answer — the submodule's own answer would hide the very twin the climb exists
// to find.
func (g gitProbes) chain(ctx context.Context, dir string) []gitProbe {
	p := g.of(ctx, dir)
	out := []gitProbe{p}
	for depth := 0; p.place == placeTree && p.tree.super != ""; depth++ {
		sub := p.tree
		sp := g.of(ctx, sub.super)
		rel, err := filepath.Rel(sp.tree.top, sub.top)
		switch {
		case depth == maxSubmoduleDepth:
			return append(out, gitProbe{place: placeUnknown, reason: "submodules nested more than " + strconv.Itoa(maxSubmoduleDepth) + " deep"})
		case sp.place != placeTree:
			return append(out, gitProbe{place: placeUnknown, reason: firstNonEmpty(sp.reason, "git could not identify "+sub.super+", the superproject of "+sub.top)})
		case err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)):
			return append(out, gitProbe{place: placeUnknown, reason: "the submodule " + sub.top + " is not inside its superproject " + sp.tree.top})
		}
		sp.tree.prefix = filepath.ToSlash(rel) + "/" + sub.prefix // sp is a copy: the memo keeps git's own answer
		sp.approx = sp.approx || p.approx
		p = sp
		out = append(out, p)
	}
	return out
}

// sharedLevel finds the innermost repository that git confirms holds the commands'
// directory (dir) and every mutated file's directory, somewhere along their chains,
// and returns their answers at that level. Answers taken there compare like with
// like; the destination is re-checked at the same level (atLevel).
func (g gitProbes) sharedLevel(ctx context.Context, dir string, targets []mutationTarget) (gitProbe, []gitProbe, bool) {
	chains := make([][]gitProbe, len(targets))
	for i, tgt := range targets {
		chains[i] = g.chain(ctx, filepath.Dir(tgt.path))
	}
	for _, run := range g.chain(ctx, dir) {
		if run.place != placeTree || run.approx {
			break
		}
		files := make([]gitProbe, len(targets))
		ok := true
		for i := range chains {
			files[i], ok = atCommon(chains[i], run.tree.common)
			if !ok {
				break
			}
		}
		if ok {
			return run, files, true
		}
	}
	return gitProbe{}, nil, false
}

// atCommon returns the confirmed answer in chain for the repository whose common
// git directory is common.
func atCommon(chain []gitProbe, common string) (gitProbe, bool) {
	for _, p := range chain {
		if p.place == placeTree && !p.approx && sameRepo(p.tree.common, common) {
			return p, true
		}
	}
	return gitProbe{}, false
}

// atLevel is placement for a command moved at a shared level: dir's answer for
// the repository whose common git directory is common, or its outermost answer
// when that repository does not hold dir (which then fails the re-check).
func (g gitProbes) atLevel(common string) func(context.Context, string) gitProbe {
	return func(ctx context.Context, dir string) gitProbe {
		c := g.chain(ctx, dir)
		if p, ok := atCommon(c, common); ok {
			return p
		}
		return c[len(c)-1]
	}
}

// sameRepo reports whether two common git directories are the same repository for
// re-rooting: equal, or the same submodule of one superproject. A submodule
// checked out inside a superproject's LINKED worktree keeps its git directory
// under that worktree's (<git>/worktrees/<id>/modules/<name>), while the main
// checkout's copy is <git>/modules/<name>; the two hold copies of the same files,
// so a command in one reaches the other's twin. repoKey folds the first form into
// the second, at every depth.
func sameRepo(a, b string) bool {
	return sameGitPath(repoKey(a), repoKey(b))
}

// repoKey folds every "worktrees/<id>/modules/" segment of a git directory into
// "modules/". Only the part below the first ".git" component is folded, so a
// directory that merely happens to be named worktrees elsewhere in the path is left
// alone.
func repoKey(gitDir string) string {
	parts := strings.Split(filepath.ToSlash(gitDir), "/")
	start := -1
	for i, p := range parts {
		if p == ".git" {
			start = i
			break
		}
	}
	if start < 0 {
		return gitDir
	}
	out := parts[: start+1 : start+1]
	for i := start + 1; i < len(parts); i++ {
		if parts[i] == "worktrees" && i+2 < len(parts) && parts[i+2] == "modules" {
			i++ // drop "worktrees" and the id; "modules" follows
			continue
		}
		out = append(out, parts[i])
	}
	return filepath.FromSlash(strings.Join(out, "/"))
}

// splitLevels refuses a run whose files, taken together, share no repository with
// the commands' directory, when one of them on its own does and would move the
// commands. The fallback (outermost placement) would keep the commands where they
// are, reading such a file as another repository's main work-tree, and test the
// commands' own copy of it.
func (g gitProbes) splitLevels(ctx context.Context, dir string, targets []mutationTarget) error {
	for i := range targets {
		run, own, ok := g.sharedLevel(ctx, dir, targets[i:i+1])
		if !ok || sameGitPath(run.tree.top, own[0].tree.top) {
			continue
		}
		other := targets[(i+1)%len(targets)].display
		return spanError(targets[i].display, own[0].tree.top, other, "a tree that shares no repository with it")
	}
	return nil
}
