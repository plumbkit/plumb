package cli

import (
	"context"
	"os/exec"
	"testing"

	"github.com/plumbkit/plumb/internal/tools"
)

func TestParseZigEnv(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		wantLib   string
		wantCache string
	}{
		{
			name: "realistic zon blob",
			in: `.{
    .zig_exe = "/opt/homebrew/Cellar/zig/0.16.0_1/bin/zig",
    .lib_dir = "/opt/homebrew/Cellar/zig/0.16.0_1/lib/zig",
    .std_dir = "/opt/homebrew/Cellar/zig/0.16.0_1/lib/zig/std",
    .global_cache_dir = "/Users/dev/.cache/zig",
    .version = "0.16.0",
}`,
			wantLib:   "/opt/homebrew/Cellar/zig/0.16.0_1/lib/zig",
			wantCache: "/Users/dev/.cache/zig",
		},
		{
			name:      "empty blob",
			in:        "",
			wantLib:   "",
			wantCache: "",
		},
		{
			name:      "malformed (no quoted values)",
			in:        ".{ .lib_dir = , .global_cache_dir = }",
			wantLib:   "",
			wantCache: "",
		},
		{
			name:      "lib only",
			in:        `    .lib_dir = "/usr/lib/zig",`,
			wantLib:   "/usr/lib/zig",
			wantCache: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lib, cache := parseZigEnv([]byte(tt.in))
			if lib != tt.wantLib {
				t.Errorf("lib = %q, want %q", lib, tt.wantLib)
			}
			if cache != tt.wantCache {
				t.Errorf("cache = %q, want %q", cache, tt.wantCache)
			}
		})
	}
}

// assertReadOnlyRoots checks every resolved root carries the read access level
// and never read-write, and that at least one has the wanted label.
func assertReadOnlyRoots(t *testing.T, roots []tools.AllowedRoot, wantLabel string) {
	t.Helper()
	if len(roots) == 0 {
		t.Fatal("expected at least one dependency root")
	}
	foundLabel := false
	for _, r := range roots {
		if r.Access != tools.AccessRead {
			t.Errorf("root %q has access %v, want AccessRead", r.Path, r.Access)
		}
		if r.Access == tools.AccessReadWrite {
			t.Errorf("root %q must never be read-write", r.Path)
		}
		if r.Label == wantLabel {
			foundLabel = true
		}
	}
	if !foundLabel {
		t.Errorf("no root carried the expected label %q; got %+v", wantLabel, roots)
	}
}

func TestComputeGoDependencyRoots(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go binary not installed")
	}
	roots := computeGoDependencyRoots(context.Background())
	if len(roots) == 0 {
		t.Skip("no Go dependency roots present (no GOROOT/GOMODCACHE on disk)")
	}
	for _, r := range roots {
		if r.Access != tools.AccessRead {
			t.Errorf("Go root %q must be read-only, got %v", r.Path, r.Access)
		}
	}
}

func TestComputeZigDependencyRoots(t *testing.T) {
	if _, err := exec.LookPath("zig"); err != nil {
		t.Skip("zig binary not installed")
	}
	roots := computeZigDependencyRoots(context.Background())
	if len(roots) == 0 {
		t.Skip("zig present but no dependency roots resolved")
	}
	assertReadOnlyRoots(t, roots, "ZIG_LIB")
}

func TestComputeRustDependencyRoots(t *testing.T) {
	if _, err := exec.LookPath("rustc"); err != nil {
		t.Skip("rustc binary not installed")
	}
	roots := computeRustDependencyRoots(context.Background())
	if len(roots) == 0 {
		t.Skip("rustc present but no dependency roots resolved (rust-src/cargo registry absent)")
	}
	for _, r := range roots {
		if r.Access != tools.AccessRead {
			t.Errorf("Rust root %q must be read-only, got %v", r.Path, r.Access)
		}
		if r.Label != "RUST_SRC" && r.Label != "CARGO_REGISTRY" {
			t.Errorf("unexpected Rust root label %q", r.Label)
		}
	}
}

func TestComputePythonDependencyRoots(t *testing.T) {
	if pythonInterpreter() == "" {
		t.Skip("no python interpreter on PATH")
	}
	roots := computePythonDependencyRoots(context.Background())
	if len(roots) == 0 {
		t.Skip("python present but no dependency roots resolved")
	}
	assertReadOnlyRoots(t, roots, "PYTHON_STDLIB")
}

func TestComputeSwiftDependencyRoots(t *testing.T) {
	if _, err := exec.LookPath("xcrun"); err != nil {
		t.Skip("xcrun not present (off macOS)")
	}
	roots := computeSwiftDependencyRoots(context.Background())
	if len(roots) == 0 {
		t.Skip("xcrun present but no SDK path resolved")
	}
	assertReadOnlyRoots(t, roots, "SWIFT_SDK")
}

func TestComputeJVMDependencyRoots(t *testing.T) {
	roots := computeJVMDependencyRoots(context.Background())
	if len(roots) == 0 {
		t.Skip("no JVM dependency roots present (no Gradle/Maven cache, JAVA_HOME unset)")
	}
	for _, r := range roots {
		if r.Access != tools.AccessRead {
			t.Errorf("JVM root %q must be read-only, got %v", r.Path, r.Access)
		}
		switch r.Label {
		case "GRADLE_CACHE", "MAVEN_REPO", "JAVA_HOME":
		default:
			t.Errorf("unexpected JVM root label %q", r.Label)
		}
	}
}
