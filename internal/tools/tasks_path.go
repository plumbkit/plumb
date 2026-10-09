package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/plumbkit/plumb/internal/paths"
)

// tasks_path.go implements run_task's optional `path`: run the stored command in
// another directory — in practice a git work-tree of the same repository, which
// is where an agent reviews or fixes a branch without disturbing the checkout it
// is working in (PLAN-494). Work in a worktree used to fall back to native `make`
// and `go test` in a shell, losing run_task's bounded output and trust gate.
//
// The move is deliberately narrow in two directions.
//
// Resolution stays where it was: the language, the [tasks.<lang>] command and
// its trust all come from the pinned workspace's config, never from the
// destination. A work-tree is a checkout of some branch, so resolving there
// would let that branch supply its own commands and have them run as trusted —
// the trust hash would bind to the wrong file.
//
// The destination stays inside the workspace, through the SAME boundary every
// other plumb tool uses (symlinks resolved, no lexical-only ".."): a stored
// trusted command may not run wherever it likes on the machine. An earlier
// version also accepted any work-tree of the same repository wherever it lived,
// which review round 1 refused (PLAN-494 B1) — the boundary is the invariant
// every other tool holds, and a [tasks.<lang>] working_dir could already point
// anywhere inside it anyway.
//
// The git questions — which work-tree contains this directory, whether it is a
// linked work-tree, which repository it belongs to across checkouts — are asked
// with mutation_test's primitives (probeGitDir, gitTree, pathWithin in
// mutationtest_root.go), so both tools agree on what a work-tree is rather than
// each asking git its own way.

// rerootForPath returns cmd with its working directory moved into path.
//
// path may be absolute, or relative to the pinned WORKSPACE — the same base every
// other plumb path argument uses. It must exist, be a directory, and lie inside the
// workspace. Whether it is a git work-tree only decides the extra care taken around
// it: the {workspace} root follows it into the same work-tree, and an argument
// naming an absolute path in the tree being LEFT is refused, because that argument
// would be left pointing at the wrong checkout.
func (t *Tasks) rerootForPath(ctx context.Context, ws string, cmd TaskCommand, path string) (TaskCommand, error) {
	target, err := resolveTaskPath(ws, path)
	if err != nil {
		return cmd, err
	}
	// The gate. checkBoundary is the same resolver the file tools and the git
	// tool use, and it is what makes "inside the workspace" mean the filesystem's
	// answer rather than a lexical one.
	if err := t.deps.checkBoundary(ctx, target); err != nil {
		return cmd, fmt.Errorf("run_task: %w", err)
	}
	dest := probeGitDir(ctx, target)
	runDir := runDirOf(ws, cmd)
	if dest.place == placeTree {
		target = filepath.Join(dest.tree.top, filepath.FromSlash(dest.tree.prefix))
		// The cross-tree checks need BOTH ends placed. A run directory outside any
		// work-tree (a plain directory inside the workspace) has nothing to compare
		// and nothing to re-root, so the move is simply the directory change.
		if from := probeGitDir(ctx, runDir); from.place == placeTree {
			if arg, ok := argNamingTree(cmd, from.tree.top, dest.tree.top); ok {
				return cmd, fmt.Errorf("run_task: the stored %s command names the path %q, which is in the work-tree being left (%s); "+
					"running it in %s would leave that argument pointing at the wrong tree. Use a path relative to the working directory, or run it where it is. Nothing was run",
					cmd.Slot, arg, from.tree.top, dest.tree.top)
			}
			cmd.Root = rerootedRoot(cmd.Root, from.tree.top, dest.tree.top)
		}
	} else {
		// Not a work-tree, so git cannot spell the path for us: canonicalise it
		// ourselves, or the same directory can be reported (and used) as both /var/...
		// and /private/var/... depending on the caller's TMPDIR — which is how this was
		// found, running the tool through the daemon rather than from a shell.
		target = paths.Canonical(target)
	}
	cmd.WorkingDir = target
	cmd.Notes = append(cmd.Notes, fmt.Sprintf("running in %s (path): resolution language, command and trust are the pinned workspace's (%s)",
		target, ws))
	return cmd, nil
}

// resolveTaskPath turns the caller's path into an absolute directory: relative
// paths resolve against the pinned workspace, like every other plumb path
// argument. Every failure names what was wrong with the path — and which base it
// was resolved against — rather than letting the child fail on a chdir.
//
// It deliberately does NOT resolve against the directory the command would have
// run in, which was the first rule here. Dogfooding found the difference: with
// `[tasks.go] working_dir = "plumb"`, `path: "plumb-wt-497"` resolved to
// plumb/plumb-wt-497 and was refused, while the caller plainly meant the work-tree
// beside the submodule checkout — which is the case this feature exists for.
func resolveTaskPath(ws, path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("run_task: path is empty; omit it to run in the workspace")
	}
	target := path
	if !filepath.IsAbs(target) {
		target = filepath.Join(ws, path)
	}
	target = filepath.Clean(target)
	info, err := os.Stat(target)
	switch {
	case err != nil:
		return "", fmt.Errorf("run_task: path %s does not exist (asked for %q, relative to the workspace %s)", target, path, ws)
	case !info.IsDir():
		return "", fmt.Errorf("run_task: path %s is not a directory", target)
	}
	return target, nil
}

// runDirOf is the directory the command would run in before any re-rooting: its
// resolved working_dir, else the workspace root, matching Tasks.run.
func runDirOf(ws string, cmd TaskCommand) string {
	if cmd.WorkingDir != "" {
		return cmd.WorkingDir
	}
	return ws
}
