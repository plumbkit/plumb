package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/plumbkit/plumb/internal/paths"
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
			return g.withKeys(ctx, append(out, gitProbe{place: placeUnknown, reason: "submodules nested more than " + strconv.Itoa(maxSubmoduleDepth) + " deep"}))
		case sp.place != placeTree:
			return g.withKeys(ctx, append(out, gitProbe{place: placeUnknown, reason: firstNonEmpty(sp.reason, "git could not identify "+sub.super+", the superproject of "+sub.top)}))
		case err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)):
			return g.withKeys(ctx, append(out, gitProbe{place: placeUnknown, reason: "the submodule " + sub.top + " is not inside its superproject " + sp.tree.top}))
		}
		sp.tree.prefix = filepath.ToSlash(rel) + "/" + sub.prefix // sp is a copy: the memo keeps git's own answer
		sp.approx = sp.approx || p.approx
		p = sp
		out = append(out, p)
	}
	return g.withKeys(ctx, out)
}

// withKeys sets gitTree.key along a chain, outermost first. The outermost tree,
// and any tree whose superproject git could not describe, is keyed by its common
// git directory, except that a LINKED worktree with no superproject takes the key
// of its repository's main checkout (mainKey): a worktree of a submodule
// repository is that submodule's copy, and only the checkout knows its
// superproject. A submodule checkout's git directory is <superproject's own git
// directory>/modules/<name> (git keeps it there, keyed by the submodule's name,
// whichever of the superproject's work-trees it is checked out in), so its key is
// the superproject's key plus modules/<name>. When the git directory is not there
// (an old checkout with an embedded .git, a git directory read from disk), the
// submodule is keyed by its common directory alone: a distinct repository.
func (g gitProbes) withKeys(ctx context.Context, chain []gitProbe) []gitProbe {
	for i := len(chain) - 1; i >= 0; i-- {
		t := &chain[i].tree
		if chain[i].place != placeTree {
			continue
		}
		t.key = t.common
		if i+1 >= len(chain) || chain[i+1].place != placeTree {
			if t.linked && t.super == "" && !chain[i].approx {
				t.key = g.mainKey(ctx, t.common)
			}
			continue
		}
		super := chain[i+1].tree
		if super.gitDir == "" {
			continue
		}
		name, err := filepath.Rel(filepath.Join(super.gitDir, "modules"), t.common)
		if err == nil && name != "." && name != ".." && !strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			t.key = filepath.Join(super.key, "modules", name)
		}
	}
	return chain
}

// mainKeyPrefix marks gitProbes entries that memoise mainKey, apart from the
// per-directory probes (no directory path starts with a NUL).
const mainKeyPrefix = "\x00main:"

// mainKey is the key of the main checkout of the repository whose common git
// directory is common. A submodule's git directory records its checkout in its own
// core.worktree (relative to the git directory); that checkout knows its
// superproject, so it is keyed through its own chain. Without core.worktree the
// repository is not a submodule's and common is the key. `rev-parse
// --show-toplevel` must not be used here: with --git-dir and no core.worktree git
// takes the CALLER's directory as the work-tree, and would name the daemon's.
func (g gitProbes) mainKey(ctx context.Context, common string) string {
	memo := mainKeyPrefix + common
	if p, ok := g[memo]; ok {
		return p.tree.key
	}
	key := common
	// Seeded before the climb below: a core.worktree cycle (A names B, B names A,
	// or a linked worktree named as its own checkout) then ends at common
	// instead of recursing without end.
	g[memo] = gitProbe{tree: gitTree{key: key}}
	cmd := exec.CommandContext(ctx, "git", gitNoOptionalLocks, "--git-dir", common, "config", "--local", "--get", "core.worktree") //nolint:gosec // G204: argv is package literals plus a git directory git itself printed
	cmd.Env = withEnvVar(os.Environ(), "LC_ALL", "C")
	if out, err := cmd.Output(); err == nil {
		if wt := strings.TrimSpace(string(out)); wt != "" {
			if !filepath.IsAbs(wt) {
				wt = filepath.Join(common, wt)
			}
			// Only a checkout of THIS repository counts. core.worktree is a value in a
			// repository's config, and a stale one (the submodule at that path was
			// swapped for another) or a hostile one names someone else's checkout,
			// whose key would move commands into an unrelated repository.
			c := g.chain(ctx, paths.Canonical(wt))
			if c[0].place == placeTree && !c[0].tree.linked && !c[0].approx && sameGitPath(c[0].tree.common, common) {
				key = c[0].tree.key
			}
		}
	}
	g[memo] = gitProbe{tree: gitTree{key: key}}
	return key
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
			files[i], ok = atKey(chains[i], run.tree.key)
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

// atKey returns the confirmed answer in chain for the repository identified by key
// (gitTree.key).
func atKey(chain []gitProbe, key string) (gitProbe, bool) {
	for _, p := range chain {
		if p.place == placeTree && !p.approx && sameGitPath(p.tree.key, key) {
			return p, true
		}
	}
	return gitProbe{}, false
}

// atLevel is placement for a command moved at a shared level: dir's answer for
// the repository identified by key, or its outermost answer when that repository
// does not hold dir (which then fails the re-check).
func (g gitProbes) atLevel(key string) func(context.Context, string) gitProbe {
	return func(ctx context.Context, dir string) gitProbe {
		c := g.chain(ctx, dir)
		if p, ok := atKey(c, key); ok {
			return p
		}
		return c[len(c)-1]
	}
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
		for j := range targets {
			if _, _, shared := g.sharedLevel(ctx, dir, targets[j:j+1]); !shared {
				other = targets[j].display // the file that keeps no repository in common with the commands
				break
			}
		}
		return spanError(targets[i].display, own[0].tree.top, other, "a tree that shares no repository with the commands")
	}
	return nil
}
