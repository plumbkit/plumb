package tools

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// TestScope describes how this workspace runs a scoped test command.
//
// It is supplied by the cli seam, which owns config, so this package neither
// re-derives the primary language nor re-parses [tasks.<lang>] — and, above all,
// does not assume Go. topology_affected's answer is a set of directories, but
// the caller's next move is run_task, and run_task takes a target relative to
// [tasks.<lang>].working_dir, so the emitted string has to be built from both.
//
// The zero value means "nothing is known": directories are named and no command
// is guessed. That is the correct answer for a workspace with no language
// attached, and it is what a caller constructing the tool without WithTestScope
// gets.
type TestScope struct {
	// Language is the workspace's primary language; "" when none is attached.
	Language string
	// WorkingDir is [tasks.<lang>].working_dir, relative to the workspace root.
	// Empty means the workspace root.
	WorkingDir string
	// ScopedTests reports whether the resolved test command accepts a positional
	// {target}. When false, run_task REFUSES a target outright ("a target was
	// given but the command has no {target} placeholder"), so emitting one would
	// hand the caller a string that cannot be run.
	ScopedTests bool
}

// pathScoped reports whether this workspace's test runner takes a positional
// PATH target that this package knows how to spell.
//
// The language list mirrors the rule the shipped [tasks.<lang>] defaults already
// state, rather than inventing a second one: go and python scope by path, so a
// directory is a meaningful target. rust's `cargo test <filter>` takes a NAME
// substring — handing it a directory silently runs nothing. typescript, swift
// and zig scope through flags whose spelling depends on the project's runner.
// For those, naming the directory and guessing no command is the honest answer;
// a wrong command is worse than none.
func (s TestScope) pathScoped() bool {
	if !s.ScopedTests {
		return false
	}
	switch s.Language {
	case "go", "python":
		return true
	default:
		return false
	}
}

// testTarget renders an indexed directory as a target this workspace's test
// runner accepts, or ok=false when no correct target exists for it.
func testTarget(scope TestScope, dir string) (string, bool) {
	if !scope.pathScoped() {
		return "", false
	}
	rel, ok := rebaseToWorkingDir(dir, scope.WorkingDir)
	if !ok {
		return "", false
	}
	if scope.Language == "go" {
		if rel == "." {
			return "./...", true
		}
		return "./" + rel + "/...", true
	}
	// python: pytest takes a plain path, and "." is a valid whole-tree scope.
	return rel, true
}

// rebaseToWorkingDir re-expresses a workspace-relative directory relative to the
// directory the test command actually runs in.
//
// A target is only correct from the runner's own working directory. This repo
// sets [tasks.go] working_dir = "plumb", so the indexed path
// "plumb/internal/config" has to reach run_task as "./internal/config/..."; the
// workspace-relative form names a directory that does not exist there, which is
// exactly why the plumb-testing skill's prescribed handoff — feed the path to
// run_task — failed. A directory OUTSIDE working_dir has no correct target at
// all, so it reports ok=false rather than emit one that would test the wrong
// tree or fail obscurely.
func rebaseToWorkingDir(dir, workingDir string) (string, bool) {
	d := path.Clean(filepath.ToSlash(strings.TrimSpace(dir)))
	w := path.Clean(filepath.ToSlash(strings.TrimSpace(workingDir)))
	if w == "." {
		return d, true
	}
	if d == w {
		return ".", true
	}
	if strings.HasPrefix(d, w+"/") {
		return strings.TrimPrefix(d, w+"/"), true
	}
	return "", false
}

// runHeaderSuffix states, once, how to run the rows that follow. It is a
// suffix on the "run these packages (N)" line so a complete answer stays one
// line plus one row per package.
func runHeaderSuffix(scope TestScope) string {
	switch {
	case scope.pathScoped():
		return ` — pass each target to run_task(slot:"test", target:…):`
	case scope.Language == "":
		return " — no language is attached, so no test command is inferred; " +
			"run your own test runner in each directory:"
	default:
		// Deliberately "does not scope by directory" rather than "takes no
		// target": `cargo test <filter>` does take a positional argument, it just
		// matches test NAMES. Saying it accepts nothing would be its own false
		// statement, in a tool being fixed for making one.
		return fmt.Sprintf(" — this workspace's %s test command does not scope by "+
			"directory, so run it as configured and use these packages to narrow by hand:",
			scope.Language)
	}
}

// packageRunLabel is what a row leads with: the run_task target where one
// exists, otherwise the directory itself. A directory outside the test
// command's working_dir is marked, because the reason it has no target is not
// obvious from the path alone.
func packageRunLabel(scope TestScope, dir string) string {
	if target, ok := testTarget(scope, dir); ok {
		return target
	}
	if scope.pathScoped() {
		return dir + " (outside " + scope.WorkingDir + "/)"
	}
	return dir
}
