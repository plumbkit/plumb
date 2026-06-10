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
// This is the same check edit_file applies; write_file uses it so a full-content
// overwrite gets the same guarantee a targeted edit already had.
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
