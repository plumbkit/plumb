package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// edit_file_sensitive_hint_test.go covers PLAN-456, the #588 D2 residual: a
// not-found old_string on a path whose content the write response withholds must
// not leak that content through the near-match hint. The hint is an ERROR
// response, and the transcript leaks either way — which is the whole reason the
// withholding exists.

const hintSecretValue = "TOKEN=abc123-secret"

// hintFixture writes three lines containing hintSecretValue at rel inside dir and
// returns the file's path.
func hintFixture(t *testing.T, dir, rel string) string {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("first line\n"+hintSecretValue+"\nlast line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// nearMissEdit runs one edit_file whose old_string differs from the file by a
// single typo, so the closest-match window is found.
func nearMissEdit(t *testing.T, deps WriteDeps, path string) error {
	t.Helper()
	_, err := NewEditFile(deps).Execute(context.Background(), mustJSON(map[string]any{
		"file_path": path,
		"edits": []map[string]string{{
			"old_string": "first line\nTOKEN=abc123-secrat\nlast line\n",
			"new_string": "replacement\n",
		}},
	}))
	return err
}

func TestEditFile_SensitivePathWithholdsTheNearMatchContent(t *testing.T) {
	dir := t.TempDir()
	env := hintFixture(t, dir, "secrets/app.env")
	deps := WriteDeps{Reads: NewReadTracker(), SensitivePathFn: realGate(dir, "secrets/*")}

	err := nearMissEdit(t, deps, env)
	if err == nil {
		t.Fatal("a near-miss old_string must fail the edit")
	}
	msg := err.Error()
	if strings.Contains(msg, hintSecretValue) {
		t.Errorf("the near-match hint leaked the file's content:\n%s", msg)
	}
	// What must SURVIVE is the retry route: the line number and the RANGE-mode
	// call, neither of which reveals content.
	for _, want := range []string{"closest match in the file is near line", "start_line", withheldSensitiveNote} {
		if !strings.Contains(msg, want) {
			t.Errorf("hint is missing %q — the retry must stay possible:\n%s", want, msg)
		}
	}
}

// TestEditFile_NonSensitivePathStillShowsTheNearMatchContent is the positive
// control: the same fixture, the same typo, a glob that does not match — the
// hint must still show the file's lines, so the gate cannot pass by suppressing
// the hint everywhere.
func TestEditFile_NonSensitivePathStillShowsTheNearMatchContent(t *testing.T) {
	dir := t.TempDir()
	plain := hintFixture(t, dir, "app.env")
	deps := WriteDeps{Reads: NewReadTracker(), SensitivePathFn: realGate(dir, "secrets/*")}

	err := nearMissEdit(t, deps, plain)
	if err == nil {
		t.Fatal("a near-miss old_string must fail the edit")
	}
	if !strings.Contains(err.Error(), hintSecretValue) {
		t.Errorf("a path the gate does not match must still show the file's content:\n%s", err)
	}
}

// TestEditFile_NoSensitiveResolverShowsTheNearMatchContent pins the unwired
// case: a bare WriteDeps{} withholds nothing, exactly as the response path does.
func TestEditFile_NoSensitiveResolverShowsTheNearMatchContent(t *testing.T) {
	dir := t.TempDir()
	env := hintFixture(t, dir, "secrets/app.env")
	deps := WriteDeps{Reads: NewReadTracker()}

	err := nearMissEdit(t, deps, env)
	if err == nil {
		t.Fatal("a near-miss old_string must fail the edit")
	}
	if !strings.Contains(err.Error(), hintSecretValue) {
		t.Errorf("with no sensitive-path resolver nothing is withheld:\n%s", err)
	}
}

// TestClosestMatchDiff_WithholdDropsOnlyTheContent is the unit pair, and the
// mutation witness: the two results come from the SAME inputs and differ only by
// the gate, so removing the gate call in notFoundError turns the sensitive case
// above red rather than silently green.
func TestClosestMatchDiff_WithholdDropsOnlyTheContent(t *testing.T) {
	content := "first line\n" + hintSecretValue + "\nlast line\n"
	searched := "first line\nTOKEN=abc123-secrat\nlast line\n"

	shown := closestMatchDiff(content, searched, "app.env", false)
	if !strings.Contains(shown, hintSecretValue) {
		t.Fatalf("witness: the ungated diff must contain the content the gate removes:\n%s", shown)
	}
	hidden := closestMatchDiff(content, searched, "secrets/app.env", true)
	if strings.Contains(hidden, hintSecretValue) {
		t.Errorf("the withheld hint leaked content:\n%s", hidden)
	}
	if strings.Contains(hidden, "current file content; suggestion only") {
		t.Errorf("the withheld hint must not advertise a diff it did not render:\n%s", hidden)
	}
	for _, want := range []string{"closest match in the file is near line", "start_line", "end_line", withheldSensitiveNote} {
		if !strings.Contains(hidden, want) {
			t.Errorf("withheld hint is missing %q:\n%s", want, hidden)
		}
	}
}
