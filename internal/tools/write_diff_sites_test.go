package tools

// write_diff_sites_test.go — the write sites that gained a response diff
// (delete_file, copy_file, rename_file and undo_edit's removal branch), plus the
// two cross-cutting properties those sites are the only ones that can pin: that
// a diff still renders with history OFF, and that a withheld path never leaks.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func executeDelete(t *testing.T, deps WriteDeps, args map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	out, err := NewDeleteFile(deps).Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("delete_file: %v", err)
	}
	return out
}

func TestDeleteResponseShowsTheRemovedContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gone.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := executeDelete(t, WriteDeps{ShowWriteDiff: true}, map[string]any{"file_path": path})
	if !strings.HasPrefix(out, "deleted "+path) {
		t.Fatalf("response no longer leads with the summary: %q", out)
	}
	if !strings.Contains(out, "--- a/") {
		t.Fatalf("delete rendered no diff: %q", out)
	}
	for _, want := range []string{"-alpha", "-beta"} {
		if !strings.Contains(out, want) {
			t.Fatalf("delete diff is missing %q:\n%s", want, out)
		}
	}
}

func TestDeleteResponseHonoursTheKnob(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gone.txt")
	if err := os.WriteFile(path, []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := executeDelete(t, WriteDeps{ShowWriteDiff: false}, map[string]any{"file_path": path})
	if strings.Contains(out, "--- a/") || strings.Contains(out, "-alpha") {
		t.Fatalf("show_write_diff off still rendered content: %q", out)
	}
	// Positive control: the same call with the knob on does render, so the
	// assertion above is the knob and not an empty file.
}

func TestDeleteResponseWithholdsASensitivePath(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, ".env")
	const content = "API_KEY=super-secret\n"
	if err := os.WriteFile(secret, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	deps := WriteDeps{ShowWriteDiff: true, SensitivePathFn: sensitiveTo(secret)}
	out := executeDelete(t, deps, map[string]any{"file_path": secret})
	if !strings.Contains(out, withheldSensitiveNote) {
		t.Fatalf("sensitive delete rendered %q, want the withholding marker", out)
	}
	if strings.Contains(out, "super-secret") {
		t.Fatalf("sensitive delete leaked the file's content:\n%s", out)
	}

	// Positive control: byte-identical content in a path the resolver does not
	// withhold IS shown — so the marker above is the policy, not an empty read.
	plain := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(plain, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	out = executeDelete(t, deps, map[string]any{"file_path": plain})
	if !strings.Contains(out, "-API_KEY=super-secret") {
		t.Fatalf("non-sensitive delete did not show its content:\n%s", out)
	}
}

func TestDeleteResponseWithholdsAFileItCouldNotRead(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.txt")
	// Past maxReadFileBytes, so deleteSummary never reads it: the response must
	// report the withholding rather than a diff against nothing.
	if err := os.WriteFile(big, []byte(strings.Repeat("a line of text\n", 20_000)), 0o644); err != nil {
		t.Fatal(err)
	}
	out := executeDelete(t, WriteDeps{ShowWriteDiff: true}, map[string]any{"file_path": big})
	if !strings.Contains(out, withheldTooLargeNote) {
		t.Fatalf("oversized delete rendered %q, want the too-large marker", out)
	}
	if strings.Contains(out, "-a line of text") {
		t.Fatalf("oversized delete rendered content:\n%s", out)
	}
}

func TestDeleteBatchShowsOneDiffPerFile(t *testing.T) {
	dir := t.TempDir()
	paths := make([]string, 0, 3)
	for _, name := range []string{"one.txt", "two.txt", "three.txt"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("content of "+name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	out := executeDelete(t, WriteDeps{ShowWriteDiff: true}, map[string]any{"paths": paths})
	if !strings.Contains(out, "deleted 3 path(s)") {
		t.Fatalf("batch summary missing: %q", out)
	}
	for _, name := range []string{"one.txt", "two.txt", "three.txt"} {
		if !strings.Contains(out, "-content of "+name) {
			t.Fatalf("batch diff is missing %s:\n%s", name, out)
		}
	}
	if got := strings.Count(out, "--- a/"); got != 3 {
		t.Fatalf("want 3 diff headers, got %d:\n%s", got, out)
	}
}

func TestDeleteResponseRelaysOnceWhenAsked(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gone.txt")
	if err := os.WriteFile(path, []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := executeDelete(t, WriteDeps{ShowWriteDiff: true, RelayDiff: true}, map[string]any{"file_path": path})
	if got := strings.Count(out, relayDiffNote); got != 1 {
		t.Fatalf("relay note appears %d times, want exactly 1:\n%s", got, out)
	}
	// The instruction must precede the diff it is about, or a truncated
	// response loses it.
	if strings.Index(out, relayDiffNote) > strings.Index(out, "--- a/") {
		t.Fatalf("relay note sits after the diff:\n%s", out)
	}
	// A withheld diff has nothing to relay, so the instruction is not dishonest
	// about there being something to show.
	deps := WriteDeps{ShowWriteDiff: true, RelayDiff: true, SensitivePathFn: sensitiveTo(path)}
	secret := filepath.Join(dir, ".env")
	if err := os.WriteFile(secret, []byte("A=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deps.SensitivePathFn = sensitiveTo(secret)
	if out := executeDelete(t, deps, map[string]any{"file_path": secret}); strings.Contains(out, relayDiffNote) {
		t.Fatalf("a withheld diff still asked the agent to relay it:\n%s", out)
	}
}

func TestCopyResponseShowsWhatLanded(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")
	if err := os.WriteFile(src, []byte("first\nsecond\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"from": src, "to": dst, "dirty_ok": true})
	out, err := NewCopyFile(WriteDeps{ShowWriteDiff: true}).Execute(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "copied "+src+" → "+dst) {
		t.Fatalf("copy response lost its summary: %q", out)
	}
	for _, want := range []string{"+first", "+second"} {
		if !strings.Contains(out, want) {
			t.Fatalf("copy diff is missing %q:\n%s", want, out)
		}
	}
}

// TestCopyResponseShowsTheOverwrittenDestinationWithHistoryOff pins the capture
// gate. The destination's bytes serve TWO consumers — the history row and the
// response diff — and the read used to be gated on history alone, so a user
// running show_write_diff with history disabled got no diff at all.
func TestCopyResponseShowsTheOverwrittenDestinationWithHistoryOff(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")
	if err := os.WriteFile(src, []byte("replacement\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deps := WriteDeps{ShowWriteDiff: true} // HistoryFn nil: history is OFF
	raw, _ := json.Marshal(map[string]any{"from": src, "to": dst, "overwrite": true, "dirty_ok": true})
	out, err := NewCopyFile(deps).Execute(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "-original") {
		t.Fatalf("the replaced destination's content is missing:\n%s", out)
	}
	if !strings.Contains(out, "+replacement") {
		t.Fatalf("the copied content is missing:\n%s", out)
	}
}

// TestCopyResponseWithholdsASensitiveSource pins the other half of the gate: the
// content came from the SOURCE, so a copy of a sensitive file under a name that
// matches no glob must still be withheld. Checking only the destination would
// print the secret the store refuses to record.
func TestCopyResponseWithholdsASensitiveSource(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, ".env")
	dst := filepath.Join(dir, "notes.txt") // matches no glob
	const content = "API_KEY=super-secret\n"
	if err := os.WriteFile(src, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	deps := WriteDeps{ShowWriteDiff: true, SensitivePathFn: sensitiveTo(src)}
	raw, _ := json.Marshal(map[string]any{"from": src, "to": dst, "dirty_ok": true})
	out, err := NewCopyFile(deps).Execute(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, withheldSensitiveNote) {
		t.Fatalf("copy of a sensitive source rendered %q, want the withholding marker", out)
	}
	if strings.Contains(out, "super-secret") {
		t.Fatalf("copy of a sensitive source leaked its content:\n%s", out)
	}
}

func TestRenameResponseShowsOnlyTheDestroyedDestination(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	plain := filepath.Join(dir, "plain.txt")
	if err := os.WriteFile(src, []byte("moved content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"from": src, "to": plain, "dirty_ok": true})
	out, err := NewRenameFile(WriteDeps{ShowWriteDiff: true}).Execute(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "renamed "+src+" → "+plain) {
		t.Fatalf("rename response lost its summary: %q", out)
	}
	// The moved content is unchanged, so there is nothing to show for it — a
	// diff here would claim a content change that did not happen.
	if strings.Contains(out, "--- a/") {
		t.Fatalf("a plain rename rendered a diff:\n%s", out)
	}

	// Now the case that DOES have something to show: the destination's content,
	// which this move destroyed and which appears nowhere else.
	other := filepath.Join(dir, "other.txt")
	overwritten := filepath.Join(dir, "overwritten.txt")
	if err := os.WriteFile(other, []byte("survivor\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(overwritten, []byte("doomed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(map[string]any{"from": other, "to": overwritten, "overwrite": true, "dirty_ok": true})
	out, err = NewRenameFile(WriteDeps{ShowWriteDiff: true}).Execute(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "-doomed") {
		t.Fatalf("the destroyed destination is missing:\n%s", out)
	}
	if strings.Contains(out, "-survivor") {
		t.Fatalf("the moved content was rendered as a change:\n%s", out)
	}
}

func TestUndoOfACreationShowsWhatItRemoved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fresh.txt")
	deps := WriteDeps{ShowWriteDiff: true, Undo: NewUndoStore()}
	if _, err := NewWriteFile(deps).Execute(context.Background(), jsonArgs(map[string]any{
		"file_path": path, "content": "created here\nsecond line\n",
	})); err != nil {
		t.Fatalf("write_file: %v", err)
	}
	raw, _ := json.Marshal(map[string]any{"file_path": path})
	out, err := NewUndoEdit(deps).Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("undo_edit: %v", err)
	}
	if !strings.Contains(out, "it had been newly created") {
		t.Fatalf("undo response lost its summary: %q", out)
	}
	for _, want := range []string{"-created here", "-second line"} {
		if !strings.Contains(out, want) {
			t.Fatalf("undo-of-create diff is missing %q:\n%s", want, out)
		}
	}
}
