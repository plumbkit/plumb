//go:build linux

package tools

import (
	"os"
	"os/exec"
	"path/filepath"
)

// sandbox_linux.go confines a command with bubblewrap (bwrap): the whole
// filesystem is bind-mounted read-only, then the temp/cache set (+ the workspace
// when allow_writes) is re-bound read-write, and the network namespace is
// unshared when deny_network is set. bwrap needs no container daemon (it is the
// mechanism Flatpak uses). When bwrap is absent the command runs unsandboxed and
// the status reports why.
//
// NOTE: exercised by a //go:build integration test against a real bwrap; the
// unit test asserts the generated argv shape. Linux support is validated in CI.

// sandboxWrap prefixes argv with a bwrap invocation implementing the write jail.
func sandboxWrap(argv []string, opts SandboxOpts) ([]string, SandboxStatus) {
	bin, err := exec.LookPath("bwrap")
	if err != nil {
		return argv, SandboxStatus{Reason: "bwrap not found on PATH"}
	}
	return buildBwrapArgv(bin, argv, opts), SandboxStatus{Active: true, Mechanism: "bwrap"}
}

// buildBwrapArgv assembles the bwrap command line. Pure, so it is asserted in
// tests without invoking bwrap. Later --bind entries override the initial
// read-only bind of /, making just the writable set read-write.
func buildBwrapArgv(bin string, argv []string, opts SandboxOpts) []string {
	out := []string{
		bin,
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		"--tmpfs", "/tmp",
	}
	if opts.AllowWrites && opts.WorkspaceRoot != "" {
		out = append(out, "--bind", opts.WorkspaceRoot, opts.WorkspaceRoot)
	}
	for _, d := range writableLinuxDirs() {
		if d != "" {
			// --bind-try: bind read-write if it exists, else skip silently.
			out = append(out, "--bind-try", d, d)
		}
	}
	if opts.DenyNetwork {
		out = append(out, "--unshare-net")
	}
	out = append(out, "--")
	out = append(out, argv...)
	return out
}

// writableLinuxDirs is the temp/cache set kept read-write so toolchains work:
// the user cache dir (Go's build cache: ~/.cache/go-build) and the Go module
// cache. /tmp is already a tmpfs from the base argv.
func writableLinuxDirs() []string {
	var dirs []string
	if d, err := os.UserCacheDir(); err == nil && d != "" {
		dirs = append(dirs, d)
	}
	dirs = append(dirs, linuxGoModCache())
	return dirs
}

func linuxGoModCache() string {
	if v := os.Getenv("GOMODCACHE"); v != "" {
		return v
	}
	gopath := os.Getenv("GOPATH")
	if gopath == "" {
		if home, err := os.UserHomeDir(); err == nil {
			gopath = filepath.Join(home, "go")
		}
	}
	if list := filepath.SplitList(gopath); len(list) > 0 && list[0] != "" {
		gopath = list[0]
	}
	if gopath == "" {
		return ""
	}
	return filepath.Join(gopath, "pkg", "mod")
}
