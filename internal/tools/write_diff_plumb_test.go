package tools

// write_diff_plumb_test.go — the `.plumb/` writers that gained a response diff:
// write_memory, delete_memory and git_init's context.md. They carry their own
// history hook rather than a tool's WriteDeps, so their diff arrives through the
// exported ResponseDiffSuffix seam; these tests pin that seam and the policy
// behind it at those sites.
//
// agent_config is the fourth, and it lives in the connection layer — it is
// covered in internal/cli, where a connection exists to render it.

import (
	"context"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/history"
)

func execWriteMemory(t *testing.T, deps WriteDeps, name, content, ws string) string {
	t.Helper()
	out, err := NewWriteMemory(nil).WithWriteDeps(deps).Execute(context.Background(),
		jsonArgs(map[string]any{"name": name, "content": content, "workspace": ws}))
	if err != nil {
		t.Fatalf("write_memory: %v", err)
	}
	return out
}

func execDeleteMemory(t *testing.T, deps WriteDeps, name, ws string) string {
	t.Helper()
	out, err := NewDeleteMemory(nil).WithWriteDeps(deps).Execute(context.Background(),
		jsonArgs(map[string]any{"name": name, "workspace": ws}))
	if err != nil {
		t.Fatalf("delete_memory: %v", err)
	}
	return out
}

func TestMemoryWritesShowTheirDiff(t *testing.T) {
	dir := t.TempDir()
	deps := WriteDeps{ShowWriteDiff: true}

	created := execWriteMemory(t, deps, "note", "line one\nline two\n", dir)
	if !strings.Contains(created, "+line one") || !strings.Contains(created, "+line two") {
		t.Fatalf("a memory creation rendered no added lines:\n%s", created)
	}

	updated := execWriteMemory(t, deps, "note", "line one\nline two changed\n", dir)
	if !strings.Contains(updated, "-line two") || !strings.Contains(updated, "+line two changed") {
		t.Fatalf("a memory overwrite rendered neither side:\n%s", updated)
	}

	deleted := execDeleteMemory(t, deps, "note", dir)
	if !strings.Contains(deleted, "-line two changed") {
		t.Fatalf("a memory deletion rendered no removed lines:\n%s", deleted)
	}
}

func TestMemoryWritesHonourTheKnobAndTheSensitiveGate(t *testing.T) {
	dir := t.TempDir()

	// The knob: the same call with show_write_diff off shows no content, and the
	// positive control below proves the fixture had some to show.
	quiet := execWriteMemory(t, WriteDeps{}, "note", "secret-ish body\n", dir)
	if strings.Contains(quiet, "--- a/") || strings.Contains(quiet, "secret-ish body") {
		t.Fatalf("show_write_diff off still rendered content:\n%s", quiet)
	}
	shown := execWriteMemory(t, WriteDeps{ShowWriteDiff: true}, "note", "shown body\n", dir)
	if !strings.Contains(shown, "+shown body") {
		t.Fatalf("the positive control rendered nothing either:\n%s", shown)
	}

	// The gate, through the REAL matcher: a memory file is <name>.md, so a glob
	// on that name withholds it — and the body must not appear anywhere.
	secretive := WriteDeps{ShowWriteDiff: true, SensitivePathFn: realGate(dir, "*.md")}
	out := execWriteMemory(t, secretive, "private", "do-not-show-this\n", dir)
	if !strings.Contains(out, withheldSensitiveNote) {
		t.Fatalf("a withheld memory rendered %q:\n%s", out, out)
	}
	if strings.Contains(out, "do-not-show-this") {
		t.Fatalf("a withheld memory leaked its body:\n%s", out)
	}
	if strings.Contains(out, relayDiffNote) {
		t.Fatalf("a withheld diff still asked the agent to relay it:\n%s", out)
	}
}

func TestGitInitMarkerShowsItsDiffButOnlyWhenItWrites(t *testing.T) {
	dir := t.TempDir()
	deps := WriteDeps{ShowWriteDiff: true}

	suffix, err := createPlumbMarker(context.Background(), dir, deps)
	if err != nil {
		t.Fatalf("createPlumbMarker: %v", err)
	}
	if !strings.Contains(suffix, "--- a/") || !strings.Contains(suffix, "+") {
		t.Fatalf("the created .plumb/context.md rendered no added lines:\n%s", suffix)
	}

	// Second run: the marker exists, nothing is written, so nothing is shown.
	// A diff here would claim a change that did not happen.
	again, err := createPlumbMarker(context.Background(), dir, deps)
	if err != nil {
		t.Fatalf("second createPlumbMarker: %v", err)
	}
	if again != "" {
		t.Fatalf("an unchanged .plumb/context.md rendered %q, want nothing", again)
	}
}

func TestResponseDiffSuffixRelaysOnceAndOnlyForContent(t *testing.T) {
	deps := WriteDeps{ShowWriteDiff: true, RelayDiff: true}
	dir := t.TempDir()
	suffix := ResponseDiffSuffix(context.Background(), deps, dir+"/m.md",
		history.Side{}, history.SideFromBytes([]byte("body\n")))
	if !strings.Contains(suffix, "+body") {
		t.Fatalf("suffix rendered no diff:\n%s", suffix)
	}
	if got := strings.Count(suffix, relayDiffNote); got != 1 {
		t.Fatalf("relay note appears %d times, want exactly 1:\n%s", got, suffix)
	}
	// Nothing to show → nothing at all, not even a bare newline.
	if got := ResponseDiffSuffix(context.Background(), deps, dir+"/m.md",
		history.SideFromBytes([]byte("same\n")), history.SideFromBytes([]byte("same\n"))); got != "" {
		t.Fatalf("an unchanged file produced %q, want \"\"", got)
	}
}
