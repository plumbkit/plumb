package quality

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// LookBinary must find an analyser in its ecosystem's install directory when
// PATH does not have it. The daemon inherits the environment of whichever
// `plumb serve` proxy spawned it — captured when that agent session started —
// which routinely lacks the directory the tool was installed into. That silently
// disabled post-write findings on a correctly set-up machine twice: once for
// golangci-lint in ~/go/bin, and again for ruff in ~/.local/bin. These tests
// were ported here from internal/quality/golangcilint when the search became
// per-ecosystem, so the Go cases below are the original regression tests.

// stubLookPathMissing makes PATH lookups fail, simulating a daemon PATH without
// the tool's install directory.
func stubLookPathMissing(t *testing.T) {
	t.Helper()
	orig := lookPath
	lookPath = func(string) (string, error) { return "", errors.New("not found in $PATH") }
	t.Cleanup(func() { lookPath = orig })
}

// fakeBinary creates an executable file named name inside dir.
func fakeBinary(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // test fixture must be executable
		t.Fatal(err)
	}
	return path
}

// goTool and pyTool are the registry rows the tests resolve against, taken from
// the registry rather than hand-built so a change to either row's Binary or
// Ecosystem is exercised here rather than silently diverging.
func goTool(t *testing.T) Tool {
	t.Helper()
	return mustTool(t, "golangci-lint")
}

func pyTool(t *testing.T) Tool {
	t.Helper()
	return mustTool(t, "ruff")
}

func mustTool(t *testing.T, name string) Tool {
	t.Helper()
	tool, ok := ToolByName(name)
	if !ok {
		t.Fatalf("registry has no %q row", name)
	}
	return tool
}

func TestLookBinary_PATHWins(t *testing.T) {
	orig := lookPath
	lookPath = func(string) (string, error) { return "/usr/bin/golangci-lint", nil }
	t.Cleanup(func() { lookPath = orig })

	gobin := t.TempDir()
	fakeBinary(t, gobin, "golangci-lint")
	t.Setenv("GOBIN", gobin)

	got, ok := LookBinary(goTool(t), "")
	if !ok || got != "/usr/bin/golangci-lint" {
		t.Errorf("LookBinary() = (%q, %v), want (/usr/bin/golangci-lint, true)", got, ok)
	}
}

func TestLookBinary_FallsBackToGOBIN(t *testing.T) {
	stubLookPathMissing(t)
	gobin := t.TempDir()
	want := fakeBinary(t, gobin, "golangci-lint")
	t.Setenv("GOBIN", gobin)
	t.Setenv("GOPATH", "")

	got, ok := LookBinary(goTool(t), "")
	if !ok || got != want {
		t.Errorf("LookBinary() = (%q, %v), want (%q, true)", got, ok, want)
	}
}

func TestLookBinary_FallsBackToGOPATHBin(t *testing.T) {
	stubLookPathMissing(t)
	gopath := t.TempDir()
	want := fakeBinary(t, filepath.Join(gopath, "bin"), "golangci-lint")
	t.Setenv("GOBIN", "")
	t.Setenv("GOPATH", gopath)

	got, ok := LookBinary(goTool(t), "")
	if !ok || got != want {
		t.Errorf("LookBinary() = (%q, %v), want (%q, true)", got, ok, want)
	}
}

// A multi-element GOPATH installs only into the FIRST element's bin.
func TestLookBinary_GOPATHListUsesFirstElement(t *testing.T) {
	stubLookPathMissing(t)
	first, second := t.TempDir(), t.TempDir()
	want := fakeBinary(t, filepath.Join(first, "bin"), "golangci-lint")
	fakeBinary(t, filepath.Join(second, "bin"), "golangci-lint")
	t.Setenv("GOBIN", "")
	t.Setenv("GOPATH", first+string(os.PathListSeparator)+second)

	got, ok := LookBinary(goTool(t), "")
	if !ok || got != want {
		t.Errorf("LookBinary() = (%q, %v), want the first GOPATH element (%q, true)", got, ok, want)
	}
}

// The default ~/go/bin is searched when neither GOBIN nor GOPATH is set — the
// exact shape of the machine where this bug was found.
func TestLookBinary_FallsBackToHomeGoBin(t *testing.T) {
	stubLookPathMissing(t)
	home := t.TempDir()
	want := fakeBinary(t, filepath.Join(home, "go", "bin"), "golangci-lint")
	t.Setenv("GOBIN", "")
	t.Setenv("GOPATH", "")
	t.Setenv("HOME", home)

	got, ok := LookBinary(goTool(t), "")
	if !ok || got != want {
		t.Errorf("LookBinary() = (%q, %v), want (%q, true)", got, ok, want)
	}
}

// ~/.local/bin is where `uv tool install` and `pip install --user` put ruff, and
// the directory whose absence from the daemon's PATH made a configured ruff look
// like a plumb limitation. This is the Python half of the golangci-lint bug.
func TestLookBinary_FallsBackToUserLocalBinForPython(t *testing.T) {
	stubLookPathMissing(t)
	home := t.TempDir()
	want := fakeBinary(t, filepath.Join(home, ".local", "bin"), "ruff")
	t.Setenv("VIRTUAL_ENV", "")
	t.Setenv("HOME", home)

	got, ok := LookBinary(pyTool(t), "")
	if !ok || got != want {
		t.Errorf("LookBinary() = (%q, %v), want (%q, true)", got, ok, want)
	}
}

// An active virtualenv's ruff beats the user-level one: a project that pins its
// own linter version means that one, and reporting findings from a different
// version than the project's own `ruff check` would be worse than none.
func TestLookBinary_VirtualEnvBeatsUserLocalBin(t *testing.T) {
	stubLookPathMissing(t)
	home := t.TempDir()
	venv := t.TempDir()
	want := fakeBinary(t, filepath.Join(venv, "bin"), "ruff")
	fakeBinary(t, filepath.Join(home, ".local", "bin"), "ruff")
	t.Setenv("VIRTUAL_ENV", venv)
	t.Setenv("HOME", home)

	got, ok := LookBinary(pyTool(t), "")
	if !ok || got != want {
		t.Errorf("LookBinary() = (%q, %v), want the virtualenv's ruff (%q, true)", got, ok, want)
	}
}

// The [quality.bin] override is the user stating the answer, so it beats PATH.
// An order that let PATH win would make the setting advisory, which is the
// opposite of what someone reaches for it to do.
func TestLookBinary_OverrideBeatsPATH(t *testing.T) {
	orig := lookPath
	lookPath = func(string) (string, error) { return "/usr/bin/ruff", nil }
	t.Cleanup(func() { lookPath = orig })

	want := fakeBinary(t, t.TempDir(), "ruff")

	got, ok := LookBinary(pyTool(t), want)
	if !ok || got != want {
		t.Errorf("LookBinary() = (%q, %v), want the override (%q, true)", got, ok, want)
	}
}

// The override expands ~ and $VARs, because that is how a path gets written into
// a config file that is meant to survive being copied between machines — and
// [lsp.<lang>] command already behaves this way.
func TestLookBinary_OverrideExpandsHome(t *testing.T) {
	stubLookPathMissing(t)
	home := t.TempDir()
	want := fakeBinary(t, filepath.Join(home, ".local", "bin"), "ruff")
	t.Setenv("HOME", home)

	got, ok := LookBinary(pyTool(t), "~/.local/bin/ruff")
	if !ok || got != want {
		t.Errorf("LookBinary() = (%q, %v), want (%q, true)", got, ok, want)
	}
}

// A stale override must not disable an analyser that is sitting on PATH: the
// user's intent was "use this tool", and failing the whole lookup would punish a
// config that has merely gone out of date on one machine.
func TestLookBinary_StaleOverrideFallsThroughToPATH(t *testing.T) {
	orig := lookPath
	lookPath = func(string) (string, error) { return "/usr/bin/ruff", nil }
	t.Cleanup(func() { lookPath = orig })

	got, ok := LookBinary(pyTool(t), filepath.Join(t.TempDir(), "nope", "ruff"))
	if !ok || got != "/usr/bin/ruff" {
		t.Errorf("LookBinary() = (%q, %v), want the PATH fallback (/usr/bin/ruff, true)", got, ok)
	}
}

func TestLookBinary_NotFoundAnywhere(t *testing.T) {
	stubLookPathMissing(t)
	t.Setenv("GOBIN", t.TempDir()) // exists but empty
	t.Setenv("GOPATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	if got, ok := LookBinary(goTool(t), ""); ok {
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

	if got, ok := LookBinary(goTool(t), ""); ok {
		t.Errorf("LookBinary() = (%q, true), want not found (dir + non-executable only)", got)
	}
}

// The same rule applies to an override: pointing [quality.bin] at a directory
// must not hand a directory to exec.
func TestLookBinary_OverrideIgnoresDirectory(t *testing.T) {
	stubLookPathMissing(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("VIRTUAL_ENV", "")

	if got, ok := LookBinary(pyTool(t), t.TempDir()); ok {
		t.Errorf("LookBinary() = (%q, true), want not found for a directory override", got)
	}
}
