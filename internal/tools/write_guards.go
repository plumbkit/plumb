package tools

// write_guards.go — optimistic-concurrency and session-aware staleness guards
// shared by write_file and edit_file. Kept in one place so the two write tools
// enforce the same "did this change under me?" semantics with identical wording.

import (
	"fmt"
	"os"
	"time"
)

// verifyExpectedVersion enforces the optional optimistic-concurrency guards
// (expected_mtime / expected_sha) that a caller may pass from a prior read_file
// header. Empty values are skipped. tool names the calling tool for the error
// message. A mismatch is a hard refusal — the file changed since the caller read
// it, so the intended write would clobber an unseen change.
//
// Both write tools funnel through this one helper: write_file calls it directly,
// and edit_file's checkExpectedVersion delegates here (wrapping a failure as an
// edit-logic error), so the two enforce identical semantics and wording.
func verifyExpectedVersion(tool, path, expectedMtime, expectedSha string) error {
	if expectedMtime != "" {
		want, err := time.Parse(time.RFC3339Nano, expectedMtime)
		if err != nil {
			return fmt.Errorf("%s: expected_mtime is not RFC3339Nano: %w", tool, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("%s: stat %q: %w", tool, path, err)
		}
		if !info.ModTime().Equal(want) {
			return fmt.Errorf(
				"%s: file %q was modified since you read it\n"+
					"  expected_mtime: %s\n"+
					"  current mtime:  %s\n"+
					"  Re-read the file and try again",
				tool, path, want.Format(time.RFC3339Nano), info.ModTime().Format(time.RFC3339Nano))
		}
	}
	if expectedSha != "" {
		current, err := fileSHA256(path)
		if err != nil {
			return fmt.Errorf("%s: computing sha256 of %q: %w", tool, path, err)
		}
		if current != expectedSha {
			return fmt.Errorf(
				"%s: file %q content has changed since you read it\n"+
					"  expected sha256: %s\n"+
					"  current  sha256: %s\n"+
					"  Re-read the file and try again",
				tool, path, expectedSha, current)
		}
	}
	return nil
}

// changedSinceSessionRead reports whether this session read path earlier (via
// the per-connection ReadTracker) and its content has changed since — i.e. a
// peer agent or a human edited it after this session's last read, with no
// explicit expected_mtime/expected_sha guard to catch it.
//
// mtime is the cheap first signal: if it has advanced the file definitely
// changed and no hashing is needed. When the mtime did NOT advance the content
// can still differ (a same-tick write, or a tool that preserves mtime), so the
// recorded read SHA is compared as the authoritative check. Hashing therefore
// happens only in the ambiguous case, keeping the common write hash-free.
//
// It returns false when the file was never read this session (so creating or
// blind-writing a brand-new file is never flagged), when reads is nil, on a
// stat error, or when no read SHA was recorded and the mtime did not advance.
// write_file uses it to refuse-with-override; edit_file uses it to warn (its
// str_replace anchor already protects the edited region, but the surrounding
// file may have moved under the caller).
func changedSinceSessionRead(reads *ReadTracker, path string) bool {
	entry, ok := reads.recorded(path)
	if !ok {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if info.ModTime().After(entry.mtime) {
		return true // mtime advanced — definitely changed; no hash needed
	}
	if entry.sha == "" {
		return false // no recorded SHA to fall back on; trust the mtime verdict
	}
	current, err := fileSHA256(path)
	if err != nil {
		return false
	}
	return current != entry.sha
}
