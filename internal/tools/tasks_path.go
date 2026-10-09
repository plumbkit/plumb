package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// tasks_path.go implements run_task's optional `path`: run the stored command in
// another directory — in practice a git work-tree of the same repository, which
// is where an agent reviews or fixes a branch without disturbing the checkout it
// is working in (PLAN-494). Work in a worktree used to fall back to native `make`
// and `go test` in a shell, losing run_task's bounded output and trust gate.
//
// The move is deliberately narrow. RESOLUTION stays where it was: the language,
// the [tasks.<lang>] command and its trust all come from the pinned workspace's
// config, never from the destination. A work-tree is a checkout of some branch,
// so resolving there would let that branch supply its own commands and have them
// run as trusted — the trust hash would bind to the wrong file. Only the
// EXECUTION moves, and with it the {workspace} root the task env expands.
//
// The git questions — which work-tree contains this directory, whether it is a
// linked work-tree, which repository it belongs to across checkouts — are asked
// with mutation_test's primitives (probeGitDir, gitTree, sameGitPath, pathWithin
// in mutationtest_root.go), so both tools agree on what a work-tree is rather
// than each asking git its own way.

// rerootForPath returns cmd with its working directory moved into path.
//
// A destination is accepted when it is the same repository as the directory the
// command would have run in (every work-tree of it qualifies, the linked ones
// included) or when it lies inside the workspace — which is what admits a
// work-tree of a repository the workspace contains, such as a submodule's
// (`<ws>/.git/modules/<name>` is under the workspace, and a checkout of that
// submodule's work-tree is where this batch's own items are built).
func rerootForPath(ctx context.Context, ws string, cmd TaskCommand, path string) (TaskCommand, error) {
	target, err := resolveTaskPath(ws, cmd, path)
	if err != nil {
		return cmd, err
	}
	dest := probeGitDir(ctx, target)
	switch dest.place {
	case placeNoRepo:
		return cmd, fmt.Errorf("run_task: path %s is not in a git work-tree, so there is no repository to run %s in. "+
			"Pass a directory inside a checkout of this workspace's repository", target, cmd.Slot)
	case placeUnknown:
		return cmd, fmt.Errorf("run_task: path %s: git could not say which work-tree it belongs to (%s). Nothing was run",
			target, dest.reason)
	}
	runDir := runDirOf(ws, cmd)
	from := probeGitDir(ctx, runDir)
	if from.place != placeTree {
		reason := from.reason
		if from.place == placeNoRepo {
			reason = "it is not in a git work-tree"
		}
		return cmd, fmt.Errorf("run_task: this workspace's %s command would run in %s, which plumb cannot place in a git work-tree (%s), "+
			"so it cannot tell whether path %s is a work-tree of the same repository. Nothing was run", cmd.Slot, runDir, reason, target)
	}
	// The repository test compares COMMON git directories, not work-tree roots: a
	// linked work-tree's root differs from the main checkout's, which is the whole
	// point of it, while every work-tree of one repository shares a common git
	// directory (mutationtest_root.go's gitTree.common says the same). The key
	// field is deliberately not used here — it is set by gitProbes.chain, which
	// this path does not run, and an unset key compares equal to anything.
	if !sameGitPath(dest.tree.common, from.tree.common) && !pathWithin(ws, dest.tree.top) {
		return cmd, fmt.Errorf("run_task: path %s is in the work-tree %s of another repository (%s), and it is not inside this workspace (%s). "+
			"Resolution and trust bind to the workspace's config, so plumb runs this command only in that repository or inside the workspace. Nothing was run",
			target, dest.tree.top, dest.tree.common, ws)
	}
	if arg, ok := argNamingTree(cmd, from.tree.top, dest.tree.top); ok {
		return cmd, fmt.Errorf("run_task: the stored %s command names the path %q, which is in the work-tree being left (%s); "+
			"running it in %s would leave that argument pointing at the wrong tree. Use a path relative to the working directory, or run it where it is. Nothing was run",
			cmd.Slot, arg, from.tree.top, dest.tree.top)
	}
	moved := filepath.Join(dest.tree.top, filepath.FromSlash(dest.tree.prefix))
	cmd.WorkingDir = moved
	cmd.Root = rerootedRoot(cmd.Root, from.tree.top, dest.tree.top)
	cmd.Notes = append(cmd.Notes, fmt.Sprintf("running in %s (path): resolution language, command and trust are the pinned workspace's (%s)",
		moved, ws))
	return cmd, nil
}

// resolveTaskPath turns the caller's path into an absolute directory, relative
// paths being relative to the directory the command would have run in. Every
// failure names what was wrong with the path rather than letting the child fail
// on a chdir.
func resolveTaskPath(ws string, cmd TaskCommand, path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("run_task: path is empty; omit it to run in the workspace")
	}
	base := runDirOf(ws, cmd)
	target := path
	if !filepath.IsAbs(target) {
		target = filepath.Join(base, path)
	}
	target = filepath.Clean(target)
	info, err := os.Stat(target)
	switch {
	case err != nil:
		return "", fmt.Errorf("run_task: path %s does not exist (asked for %q, relative to %s)", target, path, base)
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
