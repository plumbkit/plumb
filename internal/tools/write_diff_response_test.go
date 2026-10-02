package tools

// write_diff_response_test.go — the response-diff policy: what a write response
// shows, what it withholds, and what it asks the agent to do with it. Every
// assertion is paired with a positive control, so a marker that appears for the
// wrong reason cannot pass as the withholding working.

import (
	"context"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/history"
	"github.com/plumbkit/plumb/internal/textdiff"
)

// sensitiveTo returns a resolver that withholds exactly the named paths, the way
// the daemon's [history] sensitive_globs does.
func sensitiveTo(paths ...string) func(context.Context, string) bool {
	set := make(map[string]bool, len(paths))
	for _, p := range paths {
		set[p] = true
	}
	return func(_ context.Context, path string) bool { return set[path] }
}

func TestResponseDiffWithholdsASensitivePath(t *testing.T) {
	const path = "/w/app/.env"
	const before = "API_KEY=old\n"
	const after = "API_KEY=new\n"

	deps := WriteDeps{ShowWriteDiff: true, SensitivePathFn: sensitiveTo(path)}
	got := deps.responseDiff(context.Background(), path, presentSide(before), presentSide(after))
	if got != withheldSensitiveNote {
		t.Fatalf("sensitive path rendered %q, want the withholding marker", got)
	}
	if strings.Contains(got, "API_KEY") {
		t.Fatal("the withheld section leaked the file's content")
	}
	// Positive control: the same bytes under a path that is not sensitive DO
	// render, so the marker above is the resolver's doing and not a dead path.
	plain := WriteDeps{ShowWriteDiff: true, SensitivePathFn: sensitiveTo("/w/app/.env")}
	if got := plain.responseDiff(context.Background(), "/w/app/main.go", presentSide(before), presentSide(after)); !diffShown(got) {
		t.Fatalf("non-sensitive path rendered %q, want a diff", got)
	}
}

func TestGatedDiffLeavesAnEmptyDiffEmpty(t *testing.T) {
	deps := &WriteDeps{ShowWriteDiff: true, SensitivePathFn: sensitiveTo("/w/.env")}
	// A path with nothing to show must not acquire a marker: claiming a
	// withholding when no content existed would be its own false statement.
	if got := deps.gatedDiff(context.Background(), "/w/.env", ""); got != "" {
		t.Fatalf("empty diff became %q", got)
	}
	if got := deps.gatedDiff(context.Background(), "/w/.env", "--- a/x\n+++ b/x\n@@ -1,1 +1,1 @@\n-a\n+b"); got != withheldSensitiveNote {
		t.Fatalf("sensitive rendered diff = %q, want the marker", got)
	}
	if got := deps.gatedDiff(context.Background(), "/w/main.go", "--- a/x\n+++ b/x\n"); !diffShown(got) {
		t.Fatalf("non-sensitive rendered diff = %q, want it unchanged", got)
	}
}

func TestGatedDiffWithoutResolversPassesTheDiffThrough(t *testing.T) {
	// A symbol edit with no write deps is normalised to a zero WriteDeps at its
	// entry point, so "no deps" must mean "no sensitive resolver, no relay" —
	// and the diff must survive rather than vanish.
	var deps WriteDeps
	const diff = "--- a/x\n+++ b/x\n"
	if got := deps.gatedDiff(context.Background(), "/w/.env", diff); got != diff {
		t.Fatalf("zero deps = %q, want the diff unchanged", got)
	}
	if got := deps.relayNoteFor(diff); got != "" {
		t.Fatalf("zero deps relayed %q, want nothing", got)
	}
}

func TestResponseDiffRendersCreatesAndDeletes(t *testing.T) {
	deps := WriteDeps{ShowWriteDiff: true}
	ctx := context.Background()

	created := deps.responseDiff(ctx, "/w/new.go", absentSide(), presentSide("a\nb\n"))
	if !diffShown(created) || !strings.Contains(created, "+a") || !strings.Contains(created, "+b") {
		t.Fatalf("create rendered %q, want every line added", created)
	}
	deleted := deps.responseDiff(ctx, "/w/gone.go", presentSide("a\nb\n"), absentSide())
	if !diffShown(deleted) || !strings.Contains(deleted, "-a") || !strings.Contains(deleted, "-b") {
		t.Fatalf("delete rendered %q, want every line removed", deleted)
	}
	if got := deps.responseDiff(ctx, "/w/same.go", presentSide("a\n"), presentSide("a\n")); got != "" {
		t.Fatalf("unchanged content rendered %q, want nothing", got)
	}
}

func TestResponseDiffWithholdsUnknownAndBinarySides(t *testing.T) {
	deps := WriteDeps{ShowWriteDiff: true}
	ctx := context.Background()

	// A side this call did not read is withheld, NOT rendered as empty: the
	// difference between "the file was empty" and "we did not look".
	if got := deps.responseDiff(ctx, "/w/big.go", unknownSide(), presentSide("x\n")); got != withheldTooLargeNote {
		t.Fatalf("unknown side rendered %q, want the too-large marker", got)
	}
	if got := deps.responseDiff(ctx, "/w/bin.go", presentSide("a\x00b\n"), absentSide()); got != withheldBinaryNote {
		t.Fatalf("binary side rendered %q, want the binary marker", got)
	}
	// Positive control for the binary sniff: a text side of the same shape
	// renders.
	if got := deps.responseDiff(ctx, "/w/txt.go", presentSide("a b\n"), absentSide()); !diffShown(got) {
		t.Fatalf("text side rendered %q, want a diff", got)
	}
}

func TestResponseDiffHonoursTheShowWriteDiffKnob(t *testing.T) {
	off := WriteDeps{ShowWriteDiff: false}
	if got := off.responseDiff(context.Background(), "/w/a.go", presentSide("a\n"), presentSide("b\n")); got != "" {
		t.Fatalf("knob off rendered %q, want nothing", got)
	}
}

func TestBytesSideAndSideOfBoundOversizedContent(t *testing.T) {
	big := make([]byte, maxResponseDiffBytes+1)
	if got := bytesSide(big); got.State != sideUnknown {
		t.Fatalf("oversized bytes = state %v, want unknown", got.State)
	}
	if got := bytesSide(nil); got.State != sideAbsent {
		t.Fatalf("nil bytes = state %v, want absent", got.State)
	}
	if got := bytesSide([]byte{}); got.State != sidePresent {
		t.Fatalf("empty bytes = state %v, want present (an empty file has a real side)", got.State)
	}
	if got := sideOf(history.Side{Exists: true, Size: int64(len(big))}); got.State != sideUnknown {
		t.Fatalf("side with no carried content = state %v, want unknown", got.State)
	}
	if got := sideOf(history.Side{}); got.State != sideAbsent {
		t.Fatalf("zero side = state %v, want absent", got.State)
	}
}

func TestDiffSectionsCapsTheNumberOfFiles(t *testing.T) {
	sections := make([]string, maxResponseDiffFiles+5)
	for i := range sections {
		sections[i] = "--- a/f\n+++ b/f\n@@ -1,1 +1,1 @@\n-a\n+b"
	}
	got := diffSections(sections)
	if len(got) != maxResponseDiffFiles+1 {
		t.Fatalf("diffSections returned %d entries, want %d diffs plus the summary", len(got), maxResponseDiffFiles)
	}
	if last := got[len(got)-1]; !strings.Contains(last, "+5 more file(s)") {
		t.Fatalf("last entry = %q, want the omitted-count summary", last)
	}
	// Positive control: under the cap nothing is dropped and no summary appears.
	small := diffSections(sections[:maxResponseDiffFiles-1])
	if len(small) != maxResponseDiffFiles-1 {
		t.Fatalf("under the cap diffSections returned %d entries, want %d", len(small), maxResponseDiffFiles-1)
	}
}

func TestRelayNoteFiresOnlyForShownContent(t *testing.T) {
	on := &WriteDeps{RelayDiff: true}
	const diff = "--- a/x\n+++ b/x\n"
	if got := on.relayNoteFor(diff); got != relayDiffNote {
		t.Fatalf("relayNoteFor(shown diff) = %q, want the instruction", got)
	}
	// A withheld section has nothing to relay, and an empty one has nothing at
	// all — neither may ask the agent to show anything.
	if got := on.relayNoteFor(withheldSensitiveNote); got != "" {
		t.Fatalf("relayNoteFor(marker) = %q, want nothing", got)
	}
	if got := on.relayNoteFor("", withheldTooLargeNote); got != "" {
		t.Fatalf("relayNoteFor(empty, marker) = %q, want nothing", got)
	}
	off := &WriteDeps{RelayDiff: false}
	if got := off.relayNoteFor(diff); got != "" {
		t.Fatalf("relay off = %q, want nothing", got)
	}
	// The resolver wins over the field, which is how the daemon wires it.
	fn := &WriteDeps{RelayDiff: true, RelayDiffFn: func() bool { return false }}
	if got := fn.relayNoteFor(diff); got != "" {
		t.Fatalf("relay resolver off = %q, want nothing", got)
	}
}

func TestRevertNoteReportsCountsNotDiffs(t *testing.T) {
	got := revertNote([]revertedPath{{path: "/w/a.go", before: "x\ny\n", after: "x\n"}})
	if !strings.Contains(got, "reverted 1 path(s)") || !strings.Contains(got, "/w/a.go (+1 -0)") {
		t.Fatalf("revertNote = %q, want the path with its counts", got)
	}
	if strings.Contains(got, "@@ ") {
		t.Fatalf("revertNote rendered a diff (%q); reverts report counts only", got)
	}
	if !strings.Contains(got, "plumb history") {
		t.Fatalf("revertNote = %q, want a pointer to where the diffs are", got)
	}
	if got := revertNote(nil); got != "" {
		t.Fatalf("revertNote(nil) = %q, want nothing", got)
	}
	// Positive control on the counts: they describe the revert (after → before),
	// not the write it undoes.
	added, removed := textdiff.Counts(textdiff.ComputeExact("x\n", "x\ny\n"))
	if added != 1 || removed != 0 {
		t.Fatalf("counts fixture is wrong: +%d -%d", added, removed)
	}
	err := withRevertNote(context.DeadlineExceeded, []revertedPath{{path: "/w/a.go", before: "a\n", after: "b\n"}})
	if !strings.HasPrefix(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("withRevertNote buried the original error: %q", err)
	}
	if !strings.Contains(err.Error(), "/w/a.go") {
		t.Fatalf("withRevertNote dropped the path: %q", err)
	}
}

func TestIsBinaryContentSniffsTheSameBytesAsSearch(t *testing.T) {
	if isBinaryContent("") || isBinaryContent("plain text\n") {
		t.Fatal("text reported as binary")
	}
	if !isBinaryContent("head\x00tail") {
		t.Fatal("a NUL byte was not reported as binary")
	}
	// The sniff is bounded: a NUL past the sniff window is not scanned, which is
	// the behaviour the search tools share.
	if isBinaryContent(strings.Repeat("a", binarySniffBytes) + "\x00") {
		t.Fatal("a NUL past the sniff window was scanned")
	}
}
