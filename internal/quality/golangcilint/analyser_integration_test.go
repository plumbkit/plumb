//go:build integration

package golangcilint_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/quality/golangcilint"
)

// pathModeAbsFlag mirrors the unexported production constant; this file is an
// external test package. TestAnalyse_RequestsAbsolutePaths asserts Analyse
// actually passes it.
const pathModeAbsFlag = "--path-mode=abs"

// TestIntegration_RealBinary drives the real golangci-lint binary on a file
// with a known vet issue and asserts a finding comes back. It guards against a
// silent invocation regression — e.g. the v1 --out-format=json flag (removed in
// v2) that made Analyse return zero findings for every file — and against the
// trailing run-summary golangci-lint writes after the JSON on stdout.
func TestIntegration_RealBinary(t *testing.T) {
	if _, err := exec.LookPath("golangci-lint"); err != nil {
		t.Skip("golangci-lint not on PATH")
	}

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module lintcheck\n\ngo 1.21\n")
	// fmt.Printf verb/argument mismatch — flagged by govet's printf analyser,
	// which is in golangci-lint's default linter set.
	src := filepath.Join(dir, "main.go")
	writeFile(t, src, "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Printf(\"%d\", \"not a number\")\n}\n")

	// golangci-lint resolves the module from the working directory.
	t.Chdir(dir)

	findings, err := golangcilint.New().Analyse(context.Background(), []string{src})
	if err != nil {
		t.Fatalf("Analyse returned error: %v", err)
	}
	if len(findings) == 0 {
		t.Fatal("expected at least one finding from the real golangci-lint binary, got none (flag or output-parsing regression?)")
	}
}

// printfMismatch is a fmt.Printf verb/argument mismatch — flagged by govet's
// printf analyser, which is in golangci-lint's default linter set.
const printfMismatch = "package deep\n\nimport \"fmt\"\n\nfunc Bad() {\n\tfmt.Printf(\"%d\", \"not a number\")\n}\n"

// TestIntegration_BelowModuleRootFindingSurvives is the behavioural guard on
// the stale-cache check: a REAL finding in a file BELOW the module root must
// reach the caller untouched.
//
// It is built around the measured fact that broke the first implementation.
// golangci-lint does NOT report paths relative to its own working directory in
// general: with a config file present the default relative-path-mode is `cfg`
// (relative to the config file's directory), and only with no config at all is
// it `wd`. So a run anchored in the file's own directory still reports
// "sub/deep/bad.go", and resolving that against the run directory produces a
// doubled path that exists nowhere — which made every real finding stat as
// missing and be replaced by a bogus stale-cache report.
//
// The first subtest pins that path shape from the real binary, so this stays
// honest if golangci-lint ever changes it; the second proves Analyse survives
// it. plumb's own repository has a .golangci.yml at the module root, so the
// config-present case is the production case.
func TestIntegration_BelowModuleRootFindingSurvives(t *testing.T) {
	if _, err := exec.LookPath("golangci-lint"); err != nil {
		t.Skip("golangci-lint not on PATH")
	}

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module lintcheck\n\ngo 1.21\n")
	writeFile(t, filepath.Join(dir, ".golangci.yml"), "version: \"2\"\n")
	src := filepath.Join(dir, "sub", "deep", "bad.go")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, src, printfMismatch)

	// Analyse anchors the run at the file's own directory, so reproduce that.
	runDir := filepath.Dir(src)

	t.Run("raw path shape is not relative to the run directory", func(t *testing.T) {
		out := runLint(t, runDir, src) // no --path-mode
		got := issueFilenames(t, out)
		if len(got) == 0 {
			t.Fatal("no issues from the real binary; expected the printf mismatch")
		}
		if filepath.IsAbs(got[0]) {
			t.Fatalf("path is absolute without --path-mode=abs: %q", got[0])
		}
		if resolved := filepath.Join(runDir, got[0]); fileExists(resolved) {
			t.Fatalf("path %q resolves under the run directory (%q); the premise that "+
				"output paths are run-directory-relative would then hold and this guard is moot",
				got[0], runDir)
		}
		t.Logf("measured: golangci-lint reported %q from cwd %q — NOT relative to it", got[0], runDir)
	})

	t.Run("--path-mode=abs reports the file's real absolute path", func(t *testing.T) {
		out := runLint(t, runDir, src, pathModeAbsFlag)
		got := issueFilenames(t, out)
		if len(got) == 0 {
			t.Fatal("no issues from the real binary")
		}
		if got[0] != src {
			t.Errorf("path = %q, want the absolute source path %q", got[0], src)
		}
	})

	t.Run("Analyse passes the real finding through", func(t *testing.T) {
		findings, err := golangcilint.New().Analyse(context.Background(), []string{src})
		if err != nil {
			t.Fatalf("Analyse returned error: %v", err)
		}
		if len(findings) == 0 {
			t.Fatal("expected at least one finding, got none")
		}
		for _, f := range findings {
			if f.Code == golangcilint.StaleCacheCode {
				t.Fatalf("a real finding in a file that exists was replaced by the stale-cache "+
					"signal — findings were destroyed: %+v", findings)
			}
		}
		if findings[0].File != src {
			t.Errorf("File = %q, want %q", findings[0].File, src)
		}
	})
}

// TestIntegration_PathPrefixDoesNotSurviveAbsPathMode pins the interaction the
// pathModeAbs doc comment relies on: golangci-lint IGNORES
// output.path-prefix once --path-mode=abs is set, so Analyse's findings carry
// the real absolute source path rather than an absolute-looking but bogus one
// (e.g. "/myprefix/…"). If a future golangci-lint ever applied the prefix on
// top of abs mode instead, the emitted path would be absolute AND not exist on
// disk, pathPresent would judge it missing, and EVERY real finding in any
// project that sets path-prefix would be silently suppressed as a false
// stale-cache signal — the exact catastrophe this analyser exists to prevent.
// This test is the guard: it fails the day that interaction changes.
func TestIntegration_PathPrefixDoesNotSurviveAbsPathMode(t *testing.T) {
	if _, err := exec.LookPath("golangci-lint"); err != nil {
		t.Skip("golangci-lint not on PATH")
	}

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module lintcheck\n\ngo 1.21\n")
	// A prefix that cannot coincide with any real path on disk: if it ever
	// leaked into the reported filename, that filename could never exist.
	writeFile(t, filepath.Join(dir, ".golangci.yml"), "version: \"2\"\noutput:\n  path-prefix: myprefix\n")
	src := filepath.Join(dir, "sub", "deep", "bad.go")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, src, printfMismatch)

	runDir := filepath.Dir(src)

	t.Run("raw path-mode=abs output ignores path-prefix", func(t *testing.T) {
		out := runLint(t, runDir, src, pathModeAbsFlag)
		got := issueFilenames(t, out)
		if len(got) == 0 {
			t.Fatal("no issues from the real binary; expected the printf mismatch")
		}
		if got[0] != src {
			t.Fatalf("path = %q, want the real absolute source path %q — output.path-prefix leaked into "+
				"--path-mode=abs output; this is the interaction pathModeAbs's doc comment says was measured "+
				"to NOT happen", got[0], src)
		}
		t.Logf("measured: golangci-lint reported %q with output.path-prefix=myprefix set — unaffected", got[0])
	})

	t.Run("Analyse passes the real finding through", func(t *testing.T) {
		findings, err := golangcilint.New().Analyse(context.Background(), []string{src})
		if err != nil {
			t.Fatalf("Analyse returned error: %v", err)
		}
		if len(findings) == 0 {
			t.Fatal("expected at least one finding, got none")
		}
		for _, f := range findings {
			if f.Code == golangcilint.StaleCacheCode {
				t.Fatalf("a real finding was replaced by the stale-cache signal with output.path-prefix "+
					"set — path-prefix leaked into the reported path and pathPresent judged it missing: %+v", findings)
			}
		}
		if findings[0].File != src {
			t.Errorf("File = %q, want the real absolute source path %q (path-prefix must not alter it under --path-mode=abs)",
				findings[0].File, src)
		}
	})
}

// runLint invokes the real golangci-lint in dir and returns its stdout.
func runLint(t *testing.T, dir, target string, extra ...string) []byte {
	t.Helper()
	args := append([]string{"run", "--output.json.path=stdout"}, extra...)
	cmd := exec.Command("golangci-lint", append(args, target)...) //nolint:gosec // fixed argv, test-only
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	_ = cmd.Run() // non-zero exit means issues were found, which is what we want
	if stdout.Len() == 0 {
		t.Fatalf("golangci-lint wrote nothing to stdout (args %v); stderr: %s", args, stderr.String())
	}
	return stdout.Bytes()
}

// issueFilenames decodes the leading JSON document and returns each issue's
// filename. golangci-lint appends a human-readable summary after the JSON.
func issueFilenames(t *testing.T, data []byte) []string {
	t.Helper()
	var out struct {
		Issues []struct {
			Pos struct{ Filename string }
		}
	}
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&out); err != nil {
		t.Fatalf("decoding golangci-lint output: %v\n%s", err, data)
	}
	names := make([]string, 0, len(out.Issues))
	for _, i := range out.Issues {
		names = append(names, i.Pos.Filename)
	}
	return names
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
