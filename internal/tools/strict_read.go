package tools

import (
	"fmt"
	"os"
	"time"
)

// strictEnabled resolves strict mode for one call: the configured StrictModeFn
// (the resolved per-workspace [edits].strict merged with PLUMB_STRICT_EDITS by
// the daemon) when wired, else the env-only fallback used by tests and headless
// dev.
func strictEnabled(fn StrictModeFn) bool {
	if fn != nil {
		return fn()
	}
	return strictModeEnabled()
}

// requireStrictRead enforces strict mode's read-before-write contract for a
// write by tool to path: the file must have been read in this daemon session and
// must not have changed since. Callers check strictEnabled first; this function
// assumes strict mode is on.
//
// A nil ReadTracker fails closed (Mtime returns the zero time), which is the
// right answer for a safety knob that was explicitly switched on: a session with
// no read tracking cannot prove it read anything.
//
// Every write that carries agent-authored content passes through here —
// edit_file and the four symbol-edit tools alike. rename_symbol is the one
// deliberate exemption; the reason is recorded on preflightTargets
// (internal/tools/rename_symbol.go).
func requireStrictRead(reads *ReadTracker, tool, path string) error {
	recorded := reads.Mtime(path)
	if recorded.IsZero() {
		return fmt.Errorf(
			"%s: strict mode: %q has not been read in this daemon session — call read_file first",
			tool, path,
		)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s: stat %q: %w", tool, path, err)
	}
	if !info.ModTime().Equal(recorded) {
		return fmt.Errorf(
			"%s: strict mode: %q has changed since you read it\n"+
				"  recorded mtime: %s\n"+
				"  current mtime:  %s\n"+
				"  Re-read the file and try again",
			tool, path, recorded.Format(time.RFC3339Nano), info.ModTime().Format(time.RFC3339Nano),
		)
	}
	return nil
}
