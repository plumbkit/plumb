package tools

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// TargetStyle says how a package directory should be spelled as a target for
// this workspace's test command.
//
// The decision is made at the cli seam, which can see both the language and the
// user's configured command; this package only renders. That split is
// deliberate: an earlier revision kept a `case "go", "python"` switch here and
// claimed it mirrored the shipped [tasks.<lang>] defaults, but internal/tools
// (Application) cannot import internal/config (Domain), so it could not
// actually derive the rule and nothing stopped the two drifting apart.
type TargetStyle int

const (
	// TargetNone means no target can be spelled correctly. The directory is
	// named and no command is guessed.
	TargetNone TargetStyle = iota
	// TargetGoPackage is Go's recursive package pattern, ./<dir>/...
	TargetGoPackage
	// TargetPath is a plain directory path, as pytest takes.
	TargetPath
)

// TestScope describes how this workspace runs a scoped test command.
//
// The zero value means "nothing is known": directories are named and no command
// is guessed. That is the right answer for a workspace with no language
// attached, and it is what a caller constructing the tool without WithTestScope
// gets — never a Go assumption by default.
type TestScope struct {
	// Language is the workspace's primary language. It is used only to explain
	// the answer in prose; the target shape comes from Style.
	Language string
	// WorkingDir is [tasks.<lang>].working_dir, relative to the workspace root.
	// Empty means the workspace root.
	WorkingDir string
	// Style is how to spell a directory for the resolved test command, or
	// TargetNone when it cannot be spelled.
	Style TargetStyle
}

// pathScoped reports whether a target can be emitted at all.
func (s TestScope) pathScoped() bool { return s.Style != TargetNone }

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
	var target string
	switch scope.Style {
	case TargetGoPackage:
		if rel == "." {
			target = "./..."
		} else {
			target = "./" + rel + "/..."
		}
	case TargetPath:
		target = rel
	default:
		return "", false
	}
	// run_task bounds a target to one shell-safe argument, and that validator
	// lives in this very package. A directory containing a space or a "+" —
	// ordinary in the Python and JavaScript trees topology indexes — would
	// otherwise produce a target refused by the tool this row just told the
	// caller to pass it to. Name the directory instead.
	if !targetPattern.MatchString(target) {
		return "", false
	}
	return target, true
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

// runHeaderSuffix states, once, how to run the rows that follow. It is a suffix
// on the "run these packages (N)" line so a complete answer stays one line plus
// one row per package.
func runHeaderSuffix(scope TestScope) string {
	switch {
	case scope.pathScoped():
		return ` — pass each target to run_task(slot:"test", target:…):`
	case scope.Language == "":
		return " — no language is attached, so no test command is inferred; " +
			"run your own test runner in each directory:"
	default:
		// Deliberately "could not be narrowed to a directory" rather than "takes
		// no target": `cargo test <filter>` does take a positional argument, it
		// just matches test NAMES, and a project may well have configured a
		// perfectly good directory-scoped runner this code declines to guess at.
		// Claiming the command accepts nothing would be its own false statement,
		// in a tool being fixed for making one.
		return fmt.Sprintf(" — the %s test command could not be narrowed to a "+
			"directory, so run it as configured and use these packages to scope by hand:",
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
	if scope.pathScoped() && scope.WorkingDir != "" {
		return dir + " (outside " + scope.WorkingDir + "/)"
	}
	return dir
}
