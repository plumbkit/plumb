package tools

// find_replace_largetree_test.go holds the one find_replace test that builds a
// large tree. Split out of find_replace_test.go by behaviour: everything there
// asserts a rule on a handful of files, while this asserts that the rules still
// hold at 300 — which is a different question, has a different runtime, and is
// the only test in the group that -short skips.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFindReplace_LargeTreeProcessesEveryFile exercises the whole path at
// volume: 300 text files (~50 KiB each) under a glob-pruneable subtree, plus a
// 20 MiB plain-text "log" file in a sibling subtree. Skipped under -short.
//
// It used to assert this finished in under 10s. On integration (ubuntu-latest)
// it came in at 10.114552213s — 1.1% over — and failed PR #325, whose entire
// diff was 51 deleted lines of CHANGELOG.md. A wall-clock budget on a shared
// runner is a coin flip weighted by whatever else the runner is doing, and its
// message named three causes ("parallelism, sniff-first, or glob pruning") it
// could not distinguish from ambient load — inviting a hunt for a regression
// that was not there.
//
// The budget was REMOVED, not widened: widening keeps the coin flip. All three
// named causes are already pinned deterministically — glob pruning by
// TestGlobLiteralPrefix and the dirCompatibleWithPrefix table (the SkipDir
// decision itself) plus TestFindReplace_GlobPrunesSiblingDirs; sniff-first and
// the size cap by TestFindReplace_SkipsBinary and the max_file_bytes tests;
// parallelism by the 300-file count here, which a worker pool that loses or
// double-counts files would break.
//
// What remains is what this test is actually good at: at 300 files every one is
// rewritten and nothing outside the glob is touched. The duration is logged as
// a canary, deliberately with no power to fail the job.
func TestFindReplace_LargeTreeProcessesEveryFile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping large-tree test in -short mode")
	}

	dir := t.TempDir()
	mustMkdir := func(p string) {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustMkdir(filepath.Join(dir, "wanted"))
	mustMkdir(filepath.Join(dir, "noise"))

	const nFiles = 300
	body := bytes.Repeat([]byte("NEEDLE alpha beta gamma delta epsilon zeta eta\n"), 1100) // ~50 KiB
	for i := range nFiles {
		path := filepath.Join(dir, "wanted", fmt.Sprintf("f%03d.txt", i))
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Sibling subtree the dir pruner must skip. A 20 MiB plain-text file
	// containing NEEDLE — if pruning fails, the scan will hit this and slow
	// the test significantly.
	bigBody := bytes.Repeat([]byte("NEEDLE noise filler\n"), 1024*1024)
	if err := os.WriteFile(filepath.Join(dir, "noise", "huge.log"), bigBody, 0o644); err != nil {
		t.Fatal(err)
	}

	tool := NewFindReplace()
	args, _ := json.Marshal(map[string]any{
		"path":        dir,
		"pattern":     "NEEDLE",
		"replacement": "HAY",
		"glob":        "wanted/**/*.txt",
		"dry_run":     false,
		"max_files":   nFiles + 10,
	})

	start := time.Now()
	out, err := tool.Execute(context.Background(), args)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	expectedSummary := fmt.Sprintf("%d file(s)", nFiles)
	if !strings.Contains(out, expectedSummary) {
		t.Errorf("expected %q in summary:\n%s", expectedSummary, out)
	}

	// Every target file was actually rewritten, not just counted. The summary and
	// the contents can disagree if a worker drops a file after claiming it.
	for _, i := range []int{0, nFiles / 2, nFiles - 1} {
		p := filepath.Join(dir, "wanted", fmt.Sprintf("f%03d.txt", i))
		got, readErr := os.ReadFile(p)
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		if bytes.Contains(got, []byte("NEEDLE")) {
			t.Errorf("%s still contains NEEDLE — counted as processed but not rewritten", p)
		}
		if !bytes.Contains(got, []byte("HAY")) {
			t.Errorf("%s does not contain the replacement", p)
		}
	}

	// Sibling subtree must remain untouched.
	got, _ := os.ReadFile(filepath.Join(dir, "noise", "huge.log"))
	if !bytes.Equal(got, bigBody) {
		t.Error("sibling huge.log was modified — glob pruning failed")
	}
	// A canary, not an assertion. If this number starts reading in the tens of
	// seconds on a developer machine, something did regress — but a CI runner
	// under load is not evidence of that, which is why it cannot fail the job.
	t.Logf("processed %d files in %s (informational — not asserted; see the comment above)", nFiles, elapsed)
}
