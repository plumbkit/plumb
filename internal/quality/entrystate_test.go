package quality

import (
	"path/filepath"
	"strings"
	"testing"
)

// The four statuses are the four answers a user needs, and they were all one
// answer before: silence. Each case below is a shape someone actually types.

func TestClassifyEntry_ImplementedAndResolved(t *testing.T) {
	stubLookPathMissing(t)
	home := t.TempDir()
	want := fakeBinary(t, filepath.Join(home, ".local", "bin"), "ruff")
	t.Setenv("VIRTUAL_ENV", "")
	t.Setenv("HOME", home)

	got := ClassifyEntry("ruff", nil)
	if got.Status != EntryOK {
		t.Fatalf("Status = %v, want EntryOK (reason: %s)", got.Status, got.Reason)
	}
	if got.Binary != want {
		t.Errorf("Binary = %q, want %q", got.Binary, want)
	}
	if got.Language() != "python" {
		t.Errorf("Language() = %q, want python", got.Language())
	}
	if got.Reason != "" {
		t.Errorf("an OK entry needs no reason, got %q", got.Reason)
	}
}

// A supported analyser whose binary is nowhere is the user's to fix, so the
// reason must name every directory that was searched — "not found on PATH" alone
// is what left the golangci-lint case undiagnosable.
func TestClassifyEntry_ImplementedButBinaryMissing(t *testing.T) {
	stubLookPathMissing(t)
	t.Setenv("VIRTUAL_ENV", "")
	t.Setenv("HOME", t.TempDir())

	got := ClassifyEntry("ruff", nil)
	if got.Status != EntryBinaryMissing {
		t.Fatalf("Status = %v, want EntryBinaryMissing", got.Status)
	}
	if !got.Status.Blocking() {
		t.Error("a missing binary is blocking: installing it would make the analyser run")
	}
	if !strings.Contains(got.Reason, ".local/bin") {
		t.Errorf("reason must name the directories searched, got %q", got.Reason)
	}
	if !got.Known {
		t.Error("a registry name stays Known even when its binary is absent")
	}
}

// A recognised tool with no adapter is answered from the registry WITHOUT
// consulting the filesystem. Installing eslint does not make plumb run eslint,
// so reporting it as "not found" would send the user to do useless work.
func TestClassifyEntry_KnownButUnimplementedIgnoresTheFilesystem(t *testing.T) {
	dir := t.TempDir()
	fakeBinary(t, dir, "eslint")
	orig := lookPath
	lookPath = func(string) (string, error) { return filepath.Join(dir, "eslint"), nil }
	t.Cleanup(func() { lookPath = orig })

	got := ClassifyEntry("eslint", nil)
	if got.Status != EntryUnsupported {
		t.Fatalf("Status = %v, want EntryUnsupported even though the binary is installed", got.Status)
	}
	if got.Status.Blocking() {
		t.Error("an unsupported tool is not blocking: nothing the user installs will help")
	}
	if got.Language() != "typescript" {
		t.Errorf("Language() = %q, want typescript", got.Language())
	}
	if got.Binary != "" {
		t.Errorf("an unsupported entry must not report a binary plumb will not run, got %q", got.Binary)
	}
}

// An unrecognised name that IS an executable: plumb can see the thing, it just
// has no adapter for it. Yellow, not red.
func TestClassifyEntry_UnknownButExecutable(t *testing.T) {
	dir := t.TempDir()
	orig := lookPath
	lookPath = func(string) (string, error) { return filepath.Join(dir, "mylinter"), nil }
	t.Cleanup(func() { lookPath = orig })
	fakeBinary(t, dir, "mylinter")

	got := ClassifyEntry("mylinter", nil)
	if got.Status != EntryUnsupported {
		t.Fatalf("Status = %v, want EntryUnsupported", got.Status)
	}
	if got.Known {
		t.Error("Known must be false for a name with no registry row")
	}
}

// An unrecognised name that is not an executable either: almost always a typo.
func TestClassifyEntry_UnknownAndAbsent(t *testing.T) {
	stubLookPathMissing(t)

	got := ClassifyEntry("golangci-lnt", nil)
	if got.Status != EntryUnknown {
		t.Fatalf("Status = %v, want EntryUnknown", got.Status)
	}
	if !got.Status.Blocking() {
		t.Error("a name that resolves to nothing is blocking")
	}
}

// The case that prompted all of this: pasting the absolute path of a tool plumb
// DOES support. It is rejected either way — a path must never become an argv —
// but the reason has to name the spelling that works, or the user is left
// guessing why an executable they can see is not being run.
func TestClassifyEntry_PathToASupportedToolSuggestsTheName(t *testing.T) {
	stubLookPathMissing(t)
	dir := t.TempDir()
	path := fakeBinary(t, dir, "ruff")

	got := ClassifyEntry(path, nil)
	if got.Known {
		t.Error("a path is not a registry name, however familiar its basename")
	}
	if got.Status != EntryUnsupported {
		t.Fatalf("Status = %v, want EntryUnsupported for an executable path", got.Status)
	}
	if !strings.Contains(got.Reason, `"ruff"`) {
		t.Errorf("reason must name the working spelling, got %q", got.Reason)
	}
	if !strings.Contains(got.Reason, "names, not paths") {
		t.Errorf("reason must say why the path was rejected, got %q", got.Reason)
	}
}

// The same path, but the file is not there. The user's first question is "does
// this exist?", so that is the answer they get — with the naming hint kept,
// because it is still the fix.
func TestClassifyEntry_MissingPathIsUnknownAndStillSuggestsTheName(t *testing.T) {
	stubLookPathMissing(t)

	got := ClassifyEntry(filepath.Join(t.TempDir(), "nope", "ruff"), nil)
	if got.Status != EntryUnknown {
		t.Fatalf("Status = %v, want EntryUnknown for a path that does not exist", got.Status)
	}
	if !strings.Contains(got.Reason, `"ruff"`) {
		t.Errorf("reason should still name the working spelling, got %q", got.Reason)
	}
}

// A ~-prefixed path is expanded before it is stat'd, because that is how a path
// gets written into a config file meant to survive being copied between
// machines.
func TestClassifyEntry_TildePathIsExpanded(t *testing.T) {
	stubLookPathMissing(t)
	home := t.TempDir()
	fakeBinary(t, filepath.Join(home, ".local", "bin"), "ruff")
	t.Setenv("HOME", home)

	got := ClassifyEntry("~/.local/bin/ruff", nil)
	if got.Status != EntryUnsupported {
		t.Fatalf("Status = %v, want the executable-path answer, not the missing-file one "+
			"(reason: %s)", got.Status, got.Reason)
	}
}

// The [quality.bin] override participates in classification, so the Settings
// pane and doctor agree with what the runner will actually execute.
func TestClassifyEntry_OverrideResolvesAnOtherwiseMissingBinary(t *testing.T) {
	stubLookPathMissing(t)
	t.Setenv("VIRTUAL_ENV", "")
	t.Setenv("HOME", t.TempDir())
	want := fakeBinary(t, t.TempDir(), "ruff")

	got := ClassifyEntry("ruff", map[string]string{"ruff": want})
	if got.Status != EntryOK {
		t.Fatalf("Status = %v, want EntryOK via the override (reason: %s)", got.Status, got.Reason)
	}
	if got.Binary != want {
		t.Errorf("Binary = %q, want the override %q", got.Binary, want)
	}
}

func TestClassifyEntries_PreservesOrder(t *testing.T) {
	got := ClassifyEntries([]string{"eslint", "ruff", "typo"}, nil)
	if len(got) != 3 {
		t.Fatalf("got %d states, want 3", len(got))
	}
	for i, want := range []string{"eslint", "ruff", "typo"} {
		if got[i].Entry != want {
			t.Errorf("state %d is %q, want %q — the display renders these against the "+
				"configured list, so order is identity", i, got[i].Entry, want)
		}
	}
}
