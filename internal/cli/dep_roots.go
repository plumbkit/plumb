package cli

// dep_roots.go — per-language dependency-root resolvers.
//
// allow_dependency_reads lets read/search tools reach a language toolchain's
// stdlib + dependency cache read-only, so an agent can inspect a dependency's
// source without falling back to the shell. Each language registers a
// depResolver that shells out (bounded by a short timeout), degrades to nil when
// the toolchain binary is absent, and only contributes directories that actually
// exist. Every returned root is AccessRead — writes there are always refused by
// PathPolicy construction.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/tools"
)

// depResolver returns the read-only dependency roots for a language's toolchain
// (stdlib + dependency cache), bounded by a short timeout and degrading to nil
// when the toolchain binary is absent. Every returned root is AccessRead.
type depResolver func(ctx context.Context) []tools.AllowedRoot

// depResolvers maps a [lsp.<lang>] id to its dependency-root resolver. A language
// absent from the map contributes no dependency roots (e.g. typescript: its
// node_modules lives inside the workspace and is already readable).
var depResolvers = map[string]depResolver{
	"go":     computeGoDependencyRoots,
	"zig":    computeZigDependencyRoots,
	"rust":   computeRustDependencyRoots,
	"python": computePythonDependencyRoots,
	"swift":  computeSwiftDependencyRoots,
	"kotlin": computeJVMDependencyRoots,
	"java":   computeJVMDependencyRoots,
}

// depRootTimeout bounds each resolver's toolchain shell-out, mirroring goEnvRoots
// — a resolver must never block attach for long.
const depRootTimeout = 5 * time.Second

// addDirRoot appends path as a read-only root labelled label, but only if it
// exists and is a directory. A blank path or a non-directory is skipped.
func addDirRoot(roots []tools.AllowedRoot, path, label string) []tools.AllowedRoot {
	if path == "" {
		return roots
	}
	// #nosec G703 -- existence check only; path comes from a trusted toolchain
	// query (go/zig/rustc env, sysconfig) and is added solely as a read-only
	// allowlist root, never opened here.
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return append(roots, tools.AllowedRoot{Path: path, Access: tools.AccessRead, Label: label})
	}
	return roots
}

// computeGoDependencyRoots resolves GOMODCACHE and GOROOT (via `go env`, with
// environment/runtime fallbacks) and returns those that exist as read-only
// roots. Never blocks for long: the `go env` call is bounded by a short
// timeout, and a missing `go` binary degrades to the fallbacks.
func computeGoDependencyRoots(ctx context.Context) []tools.AllowedRoot {
	gomodcache, goroot := goEnvRoots(ctx)
	var roots []tools.AllowedRoot
	roots = addDirRoot(roots, gomodcache, "GOMODCACHE")
	roots = addDirRoot(roots, goroot, "GOROOT")
	return roots
}

func goEnvRoots(ctx context.Context) (gomodcache, goroot string) {
	cctx, cancel := context.WithTimeout(ctx, depRootTimeout)
	defer cancel()
	if out, err := exec.CommandContext(cctx, "go", "env", "GOMODCACHE", "GOROOT").Output(); err == nil {
		lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
		if len(lines) >= 1 {
			gomodcache = strings.TrimSpace(lines[0])
		}
		if len(lines) >= 2 {
			goroot = strings.TrimSpace(lines[1])
		}
	}
	if goroot == "" {
		goroot = os.Getenv("GOROOT")
	}
	if gomodcache == "" {
		if v := os.Getenv("GOMODCACHE"); v != "" {
			gomodcache = v
		} else if gp := os.Getenv("GOPATH"); gp != "" {
			gomodcache = filepath.Join(gp, "pkg", "mod")
		}
	}
	return gomodcache, goroot
}

// computeZigDependencyRoots resolves the Zig standard library and global package
// cache via `zig env` (whose output is ZON, not JSON). Degrades to nil when zig
// is absent.
func computeZigDependencyRoots(ctx context.Context) []tools.AllowedRoot {
	cctx, cancel := context.WithTimeout(ctx, depRootTimeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, "zig", "env").Output()
	if err != nil {
		return nil
	}
	libDir, cacheDir := parseZigEnv(out)
	var roots []tools.AllowedRoot
	roots = addDirRoot(roots, libDir, "ZIG_LIB")
	roots = addDirRoot(roots, cacheDir, "ZIG_CACHE")
	return roots
}

// parseZigEnv extracts the .lib_dir and .global_cache_dir string values from
// `zig env` ZON output (e.g. `    .lib_dir = "/path/to/lib",`). It tolerates the
// trailing comma and surrounding quotes and ignores every other key. A blank or
// malformed blob returns empty strings.
func parseZigEnv(out []byte) (libDir, cacheDir string) {
	for _, line := range strings.Split(string(out), "\n") {
		key, val, ok := zigEnvField(line)
		if !ok {
			continue
		}
		switch key {
		case "lib_dir":
			libDir = val
		case "global_cache_dir":
			cacheDir = val
		}
	}
	return libDir, cacheDir
}

// zigEnvField parses one ZON line of the form `.key = "value",` into its key and
// unquoted value. Returns ok=false for any line that is not a quoted-string
// field assignment.
func zigEnvField(line string) (key, val string, ok bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, ".") {
		return "", "", false
	}
	eq := strings.IndexByte(line, '=')
	if eq < 0 {
		return "", "", false
	}
	key = strings.TrimSpace(strings.TrimPrefix(line[:eq], "."))
	rhs := strings.TrimSpace(line[eq+1:])
	rhs = strings.TrimSuffix(rhs, ",")
	rhs = strings.TrimSpace(rhs)
	if len(rhs) < 2 || rhs[0] != '"' || rhs[len(rhs)-1] != '"' {
		return "", "", false
	}
	return key, rhs[1 : len(rhs)-1], true
}

// computeRustDependencyRoots resolves the Rust standard-library source (via
// `rustc --print sysroot`, present only with the rust-src component) and the
// cargo registry source cache. Degrades to nil when rustc is absent; the
// rust-src tree is included only when it exists.
func computeRustDependencyRoots(ctx context.Context) []tools.AllowedRoot {
	var roots []tools.AllowedRoot
	cctx, cancel := context.WithTimeout(ctx, depRootTimeout)
	defer cancel()
	if out, err := exec.CommandContext(cctx, "rustc", "--print", "sysroot").Output(); err == nil {
		sysroot := strings.TrimSpace(string(out))
		if sysroot != "" {
			src := filepath.Join(sysroot, "lib", "rustlib", "src", "rust", "library")
			roots = addDirRoot(roots, src, "RUST_SRC")
		}
	}
	cargoHome := os.Getenv("CARGO_HOME")
	if cargoHome == "" {
		if home, err := os.UserHomeDir(); err == nil {
			cargoHome = filepath.Join(home, ".cargo")
		}
	}
	if cargoHome != "" {
		roots = addDirRoot(roots, filepath.Join(cargoHome, "registry", "src"), "CARGO_REGISTRY")
	}
	return roots
}

// computePythonDependencyRoots resolves the active interpreter's stdlib and
// site-packages via sysconfig. The interpreter is the project venv's python when
// VIRTUAL_ENV is set, else python3/python on PATH. Degrades to nil when no
// interpreter is found.
//
// Limitation: venv-correct only when the daemon's environment carries
// VIRTUAL_ENV or the project's python is first on PATH — the daemon is a shared
// singleton and does not re-activate a per-project venv.
func computePythonDependencyRoots(ctx context.Context) []tools.AllowedRoot {
	interp := pythonInterpreter()
	if interp == "" {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, depRootTimeout)
	defer cancel()
	const script = "import sysconfig,json;p=sysconfig.get_paths();print(json.dumps([p['stdlib'],p['purelib'],p['platlib']]))"
	out, err := exec.CommandContext(cctx, interp, "-c", script).Output()
	if err != nil {
		return nil
	}
	var paths []string
	if err := json.Unmarshal(out, &paths); err != nil || len(paths) < 3 {
		return nil
	}
	stdlib, purelib, platlib := paths[0], paths[1], paths[2]
	var roots []tools.AllowedRoot
	roots = addDirRoot(roots, stdlib, "PYTHON_STDLIB")
	roots = addDirRoot(roots, purelib, "PYTHON_SITE")
	if platlib != purelib {
		roots = addDirRoot(roots, platlib, "PYTHON_SITE")
	}
	return roots
}

// pythonInterpreter picks the interpreter to introspect: the venv's python when
// VIRTUAL_ENV is set, else python3 then python on PATH. Returns "" when none is
// found.
func pythonInterpreter() string {
	if venv := os.Getenv("VIRTUAL_ENV"); venv != "" {
		cand := filepath.Join(venv, "bin", "python")
		// #nosec G703 -- existence check on the venv interpreter only; not opened here.
		if info, err := os.Stat(cand); err == nil && !info.IsDir() {
			return cand
		}
	}
	for _, name := range []string{"python3", "python"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// computeSwiftDependencyRoots resolves the active SDK path via `xcrun
// --show-sdk-path`. Off macOS (no xcrun) the resolver returns nil.
func computeSwiftDependencyRoots(ctx context.Context) []tools.AllowedRoot {
	cctx, cancel := context.WithTimeout(ctx, depRootTimeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, "xcrun", "--show-sdk-path").Output()
	if err != nil {
		return nil
	}
	sdk := strings.TrimSpace(string(out))
	return addDirRoot(nil, sdk, "SWIFT_SDK")
}

// computeJVMDependencyRoots resolves the Gradle module cache, the Maven local
// repository, and JAVA_HOME (when set). It shells out to nothing — these are
// well-known filesystem locations — so it never blocks. JVM dependency *sources*
// are typically shipped zipped (src.zip / -sources.jar), so these roots expose
// the jar/cache layout, not decompiled source — still useful for resource and
// layout inspection.
func computeJVMDependencyRoots(_ context.Context) []tools.AllowedRoot {
	var roots []tools.AllowedRoot
	var home string
	if h, err := os.UserHomeDir(); err == nil {
		home = h
	}
	gradleHome := os.Getenv("GRADLE_USER_HOME")
	if gradleHome == "" && home != "" {
		gradleHome = filepath.Join(home, ".gradle")
	}
	if gradleHome != "" {
		roots = addDirRoot(roots, filepath.Join(gradleHome, "caches", "modules-2"), "GRADLE_CACHE")
	}
	if home != "" {
		roots = addDirRoot(roots, filepath.Join(home, ".m2", "repository"), "MAVEN_REPO")
	}
	roots = addDirRoot(roots, os.Getenv("JAVA_HOME"), "JAVA_HOME")
	return roots
}
