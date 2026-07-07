//go:build darwin

package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// sandbox_darwin.go confines a command with macOS Seatbelt via sandbox-exec.
// The SBPL profile is a write jail (INTEGRITY-only, not confidentiality: reads
// stay permissive): `(allow default)` then `(deny file-write*)` then re-allow
// writes to a temp/cache set (+ the workspace when allow_writes), then a final
// `(deny file-write*)` on plumb's own runtime dir (<cache>/plumb, which sits
// inside the allowed cache dir) so a command cannot clobber plumb.sock/pid/lock,
// with `(deny network*)` appended when deny_network is set. SBPL is last-match-
// wins, so the trailing allow/deny rules override the permissive default.
//
// sandbox-exec is deprecated on macOS 15 but functional; when it is absent the
// command runs unsandboxed and the status reports why. The write set was
// validated against real `go build`/`go test` on macOS (they write the build
// cache and $TMPDIR) while a write to ~/.ssh is refused.

// sandboxWrap prefixes argv with sandbox-exec and the generated profile.
func sandboxWrap(argv []string, opts SandboxOpts) ([]string, SandboxStatus) {
	bin, err := exec.LookPath("sandbox-exec")
	if err != nil {
		return argv, SandboxStatus{Reason: "sandbox-exec not found on PATH"}
	}
	prefix := []string{
		bin,
		"-D", "WS=" + seatbeltParam(opts.WorkspaceRoot),
		"-D", "TMP=" + seatbeltParam(tempDir()),
		"-D", "CACHES=" + seatbeltParam(cachesDir()),
		"-D", "GOMOD=" + seatbeltParam(goModCache()),
		"-D", "PLUMBDIR=" + plumbRuntimeDir(),
		"-p", buildSeatbeltProfile(opts.AllowWrites && opts.WorkspaceRoot != "", opts.DenyNetwork),
	}
	wrapped := make([]string, 0, len(prefix)+len(argv))
	wrapped = append(wrapped, prefix...)
	wrapped = append(wrapped, argv...)
	return wrapped, SandboxStatus{Active: true, Mechanism: "sandbox-exec"}
}

// buildSeatbeltProfile renders the SBPL write-jail profile. allowWorkspaceWrites
// adds the workspace root to the writable set; denyNetwork appends a network
// denial. It is a pure function so it can be asserted in tests without invoking
// sandbox-exec.
func buildSeatbeltProfile(allowWorkspaceWrites, denyNetwork bool) string {
	var b strings.Builder
	b.WriteString("(version 1)\n")
	b.WriteString("(allow default)\n")
	b.WriteString("(deny file-write*)\n")
	b.WriteString("(allow file-write*")
	if allowWorkspaceWrites {
		b.WriteString(" (subpath (param \"WS\"))")
	}
	// Temp dirs (both the $TMPDIR the process sees and the canonical macOS
	// locations), the user cache dir (Go's build cache lives here), the Go module
	// cache, and /dev (ptys, /dev/null). Everything else is read-only.
	b.WriteString(" (subpath (param \"TMP\"))")
	b.WriteString(" (subpath \"/private/var/folders\")")
	b.WriteString(" (subpath \"/private/tmp\")")
	b.WriteString(" (subpath \"/tmp\")")
	b.WriteString(" (subpath (param \"CACHES\"))")
	b.WriteString(" (subpath (param \"GOMOD\"))")
	b.WriteString(" (subpath \"/dev\"))\n")
	// Re-deny writes to plumb's own runtime dir (inside the allowed cache dir): a
	// sandboxed command must not be able to delete/replace plumb.sock/pid/lock.
	b.WriteString("(deny file-write* (subpath (param \"PLUMBDIR\")))\n")
	if denyNetwork {
		b.WriteString("(deny network*)\n")
	}
	return b.String()
}

// seatbeltParam guards against an empty -D value, which would make the profile's
// (subpath "") invalid and break every sandboxed run. A blank path collapses to
// a harmless sentinel that matches nothing writable.
func seatbeltParam(p string) string {
	if strings.TrimSpace(p) == "" {
		return "/dev/null"
	}
	return p
}

func tempDir() string { return os.TempDir() }

// plumbRuntimeDir is <UserCacheDir>/plumb, where the daemon keeps plumb.sock,
// plumb.pid, and its locks. It is denied writes even though it sits inside the
// (writable) cache dir.
func plumbRuntimeDir() string {
	if d, err := os.UserCacheDir(); err == nil && d != "" {
		return filepath.Join(d, "plumb")
	}
	// A non-matching sentinel (nothing lives under a device file), so the deny is
	// a no-op rather than accidentally denying /dev/null. Only hit if UserCacheDir
	// fails, which is very rare.
	return "/dev/null/plumb"
}

func cachesDir() string {
	if d, err := os.UserCacheDir(); err == nil && d != "" {
		return d
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, "Library", "Caches")
	}
	return ""
}

// goModCache resolves the Go module cache without shelling out to `go env`:
// GOMODCACHE, else the first GOPATH entry's pkg/mod, else ~/go/pkg/mod.
func goModCache() string {
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
