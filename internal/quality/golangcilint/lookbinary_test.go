package golangcilint

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// LookBinary must find golangci-lint in the Go tool bin directory when PATH
// does not have it. The daemon inherits the environment of whichever `plumb
// serve` proxy spawned it — captured when that agent session started — which
// routinely lacks ~/go/bin even though the user's shell has it. `go install`
// puts the binary exactly there, so a PATH-only lookup silently disabled the
// post-write quality findings on a correctly set-up machine.

// stubLookPath makes PATH lookups fail, simulating a daemon PATH without the
// Go tool bin dir.
func stubLookPathMissing(t *testing.T) {
	t.Helper()
	orig := lookPath
	lookPath = func(string) (string, error) { return "", errors.New("not found in $PATH") }
	t.Cleanup(func() { lookPath = orig })
}

// fakeBinary creates an executable file named golangci-lint inside dir.
func fakeBinary(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "golangci-lint")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // test fixture must be executable
		t.Fatal(err)
	}
	return path
}

func TestLookBinary_PATHWins(t *testing.T) {
	orig := lookPath
	lookPath = func(string) (string, error) { return "/usr/bin/golangci-lint", nil }
	t.Cleanup(func() { lookPath = orig })

	// A GOBIN copy exists too, but PATH must win: the pacman/system build is
	// the one the user's shell and CI resolve, and two builds of one linter
	// disagreeing is how phantom lint failures happen.
	gobin := t.TempDir()
	fakeBinary(t, gobin)
	t.Setenv("GOBIN", gobin)

	got, ok := LookBinary()
	if !ok || got != "/usr/bin/golangci-lint" {
		t.Errorf("LookBinary() = (%q, %v), want (/usr/bin/golangci-lint, true)", got, ok)
	}
}

func TestLookBinary_FallsBackToGOBIN(t *testing.T) {
	stubLookPathMissing(t)
	gobin := t.TempDir()
	want := fakeBinary(t, gobin)
	t.Setenv("GOBIN", gobin)
	t.Setenv("GOPATH", "")

	got, ok := LookBinary()
	if !ok || got != want {
		t.Errorf("LookBinary() = (%q, %v), want (%q, true)", got, ok, want)
	}
}

func TestLookBinary_FallsBackToGOPATHBin(t *testing.T) {
	stubLookPathMissing(t)
	gopath := t.TempDir()
	want := fakeBinary(t, filepath.Join(gopath, "bin"))
	t.Setenv("GOBIN", "")
	t.Setenv("GOPATH", gopath)

	got, ok := LookBinary()
	if !ok || got != want {
		t.Errorf("LookBinary() = (%q, %v), want (%q, true)", got, ok, want)
	}
}

// A multi-element GOPATH installs only into the FIRST element's bin.
func TestLookBinary_GOPATHListUsesFirstElement(t *testing.T) {
	stubLookPathMissing(t)
	first, second := t.TempDir(), t.TempDir()
	want := fakeBinary(t, filepath.Join(first, "bin"))
	fakeBinary(t, filepath.Join(second, "bin"))
	t.Setenv("GOBIN", "")
	t.Setenv("GOPATH", first+string(os.PathListSeparator)+second)

	got, ok := LookBinary()
	if !ok || got != want {
		t.Errorf("LookBinary() = (%q, %v), want the first GOPATH element (%q, true)", got, ok, want)
	}
}

// The default ~/go/bin is searched when neither GOBIN nor GOPATH is set — the
// exact shape of the machine where this bug was found.
func TestLookBinary_FallsBackToHomeGoBin(t *testing.T) {
	stubLookPathMissing(t)
	home := t.TempDir()
	want := fakeBinary(t, filepath.Join(home, "go", "bin"))
	t.Setenv("GOBIN", "")
	t.Setenv("GOPATH", "")
	t.Setenv("HOME", home)

	got, ok := LookBinary()
	if !ok || got != want {
		t.Errorf("LookBinary() = (%q, %v), want (%q, true)", got, ok, want)
	}
}

func TestLookBinary_NotFoundAnywhere(t *testing.T) {
	stubLookPathMissing(t)
	t.Setenv("GOBIN", t.TempDir()) // exists but empty
	t.Setenv("GOPATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	if got, ok := LookBinary(); ok {
		t.Errorf("LookBinary() = (%q, true), want not found", got)
	}
}

// A directory named golangci-lint, or a non-executable file, must not be
// mistaken for the binary.
func TestLookBinary_IgnoresDirectoryAndNonExecutable(t *testing.T) {
	stubLookPathMissing(t)
	gobin := t.TempDir()
	if err := os.MkdirAll(filepath.Join(gobin, "golangci-lint"), 0o755); err != nil {
		t.Fatal(err)
	}
	gopath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(gopath, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gopath, "bin", "golangci-lint"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOBIN", gobin)
	t.Setenv("GOPATH", gopath)
	t.Setenv("HOME", t.TempDir())

	if got, ok := LookBinary(); ok {
		t.Errorf("LookBinary() = (%q, true), want not found (dir + non-executable only)", got)
	}
}
