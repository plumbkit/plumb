package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// tempRuntimeDir makes a directory that satisfies every check the spec puts on
// $XDG_RUNTIME_DIR: absolute, a directory, mode 0700, owned by us.
func tempRuntimeDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "run")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRuntimeDir_PrefersXDGRuntimeDir(t *testing.T) {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		t.Skip("XDG_RUNTIME_DIR has no meaning on this platform")
	}
	run := tempRuntimeDir(t)
	t.Setenv("XDG_RUNTIME_DIR", run)

	if got, want := RuntimeDir(), filepath.Join(run, appDir); got != want {
		t.Fatalf("RuntimeDir() = %q, want %q", got, want)
	}
}

// The cache dir is the fallback on purpose, and NOT os.TempDir: on macOS
// $TMPDIR differs between a GUI-app launch and a terminal launch, so a socket
// there would move depending on how the client started plumb.
func TestRuntimeDir_FallsBackToCacheDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))

	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := RuntimeDir(), filepath.Join(cache, appDir); got != want {
		t.Fatalf("RuntimeDir() = %q, want %q", got, want)
	}
}

// Each of these is a check the spec requires of the CONSUMER, and each failure
// has to mean "fall back", never "use it anyway": the directory holds the
// daemon socket, so trusting a world-readable or someone-else's directory would
// hand that socket to whoever owns it.
func TestRuntimeDir_RejectsAnUnusableXDGRuntimeDir(t *testing.T) {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		t.Skip("XDG_RUNTIME_DIR has no meaning on this platform")
	}
	home := t.TempDir()

	loose := filepath.Join(t.TempDir(), "loose")
	if err := os.Mkdir(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	cases := map[string]string{
		"empty":              "",
		"relative":           "run/user/1000",
		"missing":            filepath.Join(t.TempDir(), "does-not-exist"),
		"not a directory":    notADir,
		"world-readable 755": loose,
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", home)
			t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
			t.Setenv("XDG_RUNTIME_DIR", value)

			cache, err := os.UserCacheDir()
			if err != nil {
				t.Fatal(err)
			}
			if got, want := RuntimeDir(), filepath.Join(cache, appDir); got != want {
				t.Fatalf("RuntimeDir() = %q, want the cache fallback %q", got, want)
			}
		})
	}
}

// RuntimeDir resolves only. The TUI's liveness check and both sandboxes call it
// for a path, and creating a directory as a side effect of asking where one
// would be is how a "is the daemon up?" probe ends up making the answer look
// different.
func TestRuntimeDir_CreatesNothing(t *testing.T) {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		t.Skip("XDG_RUNTIME_DIR has no meaning on this platform")
	}
	run := tempRuntimeDir(t)
	t.Setenv("XDG_RUNTIME_DIR", run)

	got := RuntimeDir()
	if _, err := os.Stat(got); !os.IsNotExist(err) {
		t.Fatalf("RuntimeDir() created %q (stat err = %v)", got, err)
	}
}

func TestLegacyRuntimeDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))

	// Nothing legacy when the runtime dir has not moved: the caller uses "" to
	// mean "do not go looking for an older daemon".
	t.Setenv("XDG_RUNTIME_DIR", "")
	if got := LegacyRuntimeDir(); got != "" {
		t.Fatalf("LegacyRuntimeDir() = %q, want \"\" when RuntimeDir already IS the cache dir", got)
	}

	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		return
	}
	t.Setenv("XDG_RUNTIME_DIR", tempRuntimeDir(t))
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := LegacyRuntimeDir(), filepath.Join(cache, appDir); got != want {
		t.Fatalf("LegacyRuntimeDir() = %q, want %q", got, want)
	}
}
