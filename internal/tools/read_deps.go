package tools

// readRecordingTool is implemented by every tool whose Execute call records a
// file read into a ReadTracker — the dependency [edits] strict mode's
// edit_file gate checks before allowing a write. ReadDeps reports whether the
// wiring registerAllTools (internal/cli/conn_register.go) threads through is
// actually non-nil on THIS instance: tracker (the constructor-supplied
// ReadTracker) and readsFor (the PLAN-286 per-agent resolver, which overrides
// tracker when set — see each tool's readTracker method) are the pair that
// matters for strict mode, plus writes (the concurrent-edit-on-read warning)
// and client (the edit-lane hint gate) where this tool carries that dependency
// at all. A tool with no such leg (e.g. read_symbol has no WriteTracker) reports
// true for it — not applicable, not a wiring gap.
//
// This is the compile-time-ish contract PLAN-361 (registration-parity and
// wiring tests) exists to make this class of defect structurally unrepeatable.
// The read_multiple_files defect (PLAN-357) was exactly this: a read-shaped
// tool registered with its tracker/readsFor wiring entirely absent, so
// ReadTracker.Record never ran and every subsequent edit_file failed strict
// mode's "has not been read" gate even immediately after a batch read of that
// same file. TestToolWiringParity (internal/cli) asserts this over every
// read-recording tool via the REAL registerAllTools registration — probing
// the object registration actually built and wired, not a hand-built stand-in
// that could silently drift from it.
type readRecordingTool interface {
	ReadDeps() (tracker, readsFor, writes, client bool)
}

// Compile-time membership: every read-recording tool must implement
// readRecordingTool, so a future one added here without a ReadDeps method
// fails the build rather than silently escaping TestToolWiringParity's
// coverage (internal/cli).
var (
	_ readRecordingTool = (*ReadFile)(nil)
	_ readRecordingTool = (*ReadSymbol)(nil)
	_ readRecordingTool = (*ReadMultipleFiles)(nil)
)
