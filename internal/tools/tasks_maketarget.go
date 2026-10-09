package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// tasks_maketarget.go answers the "no command configured for this slot" refusal
// with the thing the workspace itself already has (PLAN-494).
//
// The friction: `run_task slot:"vuln"` was refused with a config recipe while
// the project's own gate — `make vuln`, which is what plumb's CI runs — sat
// right there in the Makefile. The refusal was not wrong; it just stopped one
// step short of the answer the workspace already contained, and the agent's next
// move was raw shell, losing run_task's bounded output.
//
// It deliberately does NOT ship a `vuln` slot default. config_tasks.go's rule is
// that an empty slot means "no command", never a guessed tool that may be
// absent, and govulncheck is not part of the Go toolchain: defaulting it would
// fail on every machine without it. Naming the project's own target fixes the
// whole family of missing slots — any verb the Makefile already defines — with
// no new slot vocabulary and no new dependency.
//
// It lives here rather than at the cli seam because the directory that matters
// is the one the command would RUN IN, and only this layer knows that: review
// round 1 caught the first version looking in the workspace root alone, which
// misses plumb-ops's own case (its `vuln` target is in plumb/Makefile, reached
// through `[tasks.go] working_dir`, or through `path`).

// makefileNames are the files GNU make reads, in its own precedence order.
var makefileNames = []string{"GNUmakefile", "makefile", "Makefile"}

// unconfiguredSlotRemedy returns the sentence the refusal should carry for a slot
// with no command, or "" when the files it would name are not there. dir is the
// directory the command would have run in; path is the caller's `path`, if any,
// which takes precedence because that is where they asked to run.
//
// Best-effort by design: the call is already being refused, so a path that
// cannot be resolved changes nothing except that the remedy falls back to the
// directory the command would otherwise have used.
func (t *Tasks) unconfiguredSlotRemedy(ctx context.Context, ws string, cmd TaskCommand, path, slot string) string {
	dir := runDirOf(ws, cmd)
	if path != "" {
		if resolved, err := resolveTaskPath(ws, path); err == nil && t.deps.checkBoundary(ctx, resolved) == nil {
			dir = resolved
			if dest := probeGitDir(ctx, resolved); dest.place == placeTree {
				dir = filepath.Join(dest.tree.top, filepath.FromSlash(dest.tree.prefix))
			}
		}
	}
	return makeTargetRemedy(dir, slot)
}

// makeTargetRemedy returns the sentence a refusal should carry when dir's
// Makefile defines a target named after the missing slot, or "" when it does
// not. It never inspects anything outside dir.
//
// The candidates are matched against the directory's ACTUAL entries rather than
// read by name: macOS and Windows filesystems are case-insensitive, so reading
// "makefile" finds a file spelled "Makefile" and the remedy would then name a
// file the user does not have.
func makeTargetRemedy(dir, slot string) string {
	if dir == "" || slot == "" {
		return ""
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	present := make(map[string]bool, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			present[e.Name()] = true
		}
	}
	for _, name := range makefileNames {
		if !present[name] {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !makefileHasTarget(string(data), slot) {
			continue
		}
		return fmt.Sprintf("This workspace's %s (in %s) defines a %q target — the project's own gate for this verb: "+
			"run `make %s` in the project shell, or wire it into run_task with [tasks.<lang>] %s = \"make %s\" "+
			"(then `plumb trust`, since that command would come from the project's config).",
			name, dir, slot, slot, slot, slot)
	}
	return ""
}

// makefileHasTarget reports whether a Makefile defines the named target.
//
// A target line is one whose text before the first ":" is exactly the name,
// at column zero (a recipe line is tab-indented, a comment starts with "#"),
// and whose ":" is not part of a ":=" assignment. It is a deliberately
// conservative reading of a file plumb does not own: a false negative costs a
// missing sentence in a refusal, while a false positive would send a caller to a
// target that does not exist.
func makefileHasTarget(src, name string) bool {
	for line := range strings.SplitSeq(src, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || line[0] == '\t' || strings.HasPrefix(line, "#") {
			continue
		}
		head, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.HasPrefix(rest, "=") {
			continue // `name := value` is an assignment, not a target
		}
		if strings.TrimSpace(head) == name {
			return true
		}
	}
	return false
}
