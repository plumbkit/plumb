package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestLogDir_DarwinUsesLibraryLogs is the regression guard for the macOS log
// location. Logs are the one role where the macOS convention (~/Library/Logs)
// diverges from every XDG base (which fold "state" into ~/Library/Application
// Support), so a refactor routing logs through StateDir silently relocates them.
// This pins the contract per-OS so that cannot recur unnoticed.
func TestLogDir_DarwinUsesLibraryLogs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got := LogDir()

	if runtime.GOOS == "darwin" {
		want := filepath.Join(home, "Library", "Logs", appDir)
		if got != want {
			t.Errorf("LogDir() = %q, want %q (macOS logs belong in ~/Library/Logs, not Application Support)", got, want)
		}
		if strings.Contains(got, "Application Support") || strings.Contains(got, "Caches") {
			t.Errorf("LogDir() = %q must not resolve under Application Support or Caches on macOS", got)
		}
		return
	}

	// Non-macOS: logs follow the XDG state dir, which is the correct place there.
	if got != StateDir() {
		t.Errorf("LogDir() = %q, want StateDir() %q on %s", got, StateDir(), runtime.GOOS)
	}
}

// TestLogDir_LinuxHonoursXDGStateHome confirms the Linux path follows
// $XDG_STATE_HOME. Only meaningful off darwin (darwin takes the Library/Logs
// branch regardless of XDG_STATE_HOME).
func TestLogDir_LinuxHonoursXDGStateHome(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("darwin uses ~/Library/Logs, not XDG_STATE_HOME")
	}
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)

	want := filepath.Join(state, appDir)
	if got := LogDir(); got != want {
		t.Errorf("LogDir() = %q, want %q", got, want)
	}
}

// TestConfigDir_HonoursTempXDGWithoutHijackSignal pins the no-regression case:
// an XDG_CONFIG_HOME under a temp dir with NO TSM_ORIG companion is honoured
// verbatim, exactly as before the hijack guard existed. This is what keeps
// legitimate temp-dir usage (CI, tests, sandboxes) working.
func TestConfigDir_HonoursTempXDGWithoutHijackSignal(t *testing.T) {
	unsetEnv(t, "TSM_ORIG_XDG_CONFIG_HOME") // neutralise any ambient tsm companion
	tmp := t.TempDir()                      // under a temp root on every OS
	t.Setenv("XDG_CONFIG_HOME", tmp)

	want := filepath.Join(tmp, appDir)
	if got := ConfigDir(); got != want {
		t.Errorf("ConfigDir() = %q, want %q (temp XDG with no hijack signal must be honoured)", got, want)
	}
}

// TestConfigDir_RecoversTSMHijack covers the tsm case from the field: tsm
// rewrites XDG_CONFIG_HOME to a per-session temp dir and stashes the real value
// in TSM_ORIG_XDG_CONFIG_HOME. plumb must recover the stashed original rather
// than land config in the throwaway dir.
func TestConfigDir_RecoversTSMHijack(t *testing.T) {
	hijacked := tempPath("tsm-501", ".fish", "plumb-4") // tsm's per-session temp dir
	// A plausible real config home, deliberately not under any temp root.
	orig := filepath.Join(string(filepath.Separator), "home", "fake", ".config")
	t.Setenv("XDG_CONFIG_HOME", hijacked)
	t.Setenv("TSM_ORIG_XDG_CONFIG_HOME", orig)

	want := filepath.Join(orig, appDir)
	if got := ConfigDir(); got != want {
		t.Errorf("ConfigDir() = %q, want recovered %q", got, want)
	}
}

// TestConfigDir_HijackWithTempOriginFallsBackToDefault covers the degenerate
// case where even the stashed original is under a temp root: plumb drops the
// override and resolves the OS-native default rather than trusting either temp
// path.
func TestConfigDir_HijackWithTempOriginFallsBackToDefault(t *testing.T) {
	hijacked := tempPath("tsm-501", "plumb-4")
	origTemp := tempPath("tsm-501", "orig")
	home := filepath.Join(string(filepath.Separator), "home", "fake")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", hijacked)
	t.Setenv("TSM_ORIG_XDG_CONFIG_HOME", origTemp)

	got := ConfigDir()
	if strings.Contains(got, hijacked) || strings.Contains(got, origTemp) {
		t.Errorf("ConfigDir() = %q must not resolve under either temp path %q / %q", got, hijacked, origTemp)
	}
	if !strings.Contains(got, home) {
		t.Errorf("ConfigDir() = %q, expected the OS-native default under HOME %q", got, home)
	}
}

// TestRecoveredHijacks_ReportsRecovery confirms a recovered hijack is recorded
// and surfaced (the daemon log and `plumb config show` read it from here).
func TestRecoveredHijacks_ReportsRecovery(t *testing.T) {
	reloadMu.Lock()
	recovered = map[string]Hijack{} // isolate from any recovery recorded earlier in the run
	reloadMu.Unlock()

	hijacked := tempPath("tsm-501", ".fish", "plumb-4")
	orig := filepath.Join(string(filepath.Separator), "home", "fake", ".config")
	t.Setenv("XDG_CONFIG_HOME", hijacked)
	t.Setenv("TSM_ORIG_XDG_CONFIG_HOME", orig)

	_ = ConfigDir() // trigger resolution, which records the recovery

	var found *Hijack
	for _, h := range RecoveredHijacks() {
		if h.EnvVar == "XDG_CONFIG_HOME" {
			hc := h
			found = &hc
		}
	}
	if found == nil {
		t.Fatalf("RecoveredHijacks() missing an XDG_CONFIG_HOME entry")
	}
	if found.From != hijacked {
		t.Errorf("From = %q, want %q", found.From, hijacked)
	}
	if found.To != orig {
		t.Errorf("To = %q, want %q", found.To, orig)
	}
}

// TestUnderTempDir is a focused table for the temp-root classifier. Paths are
// built from os.TempDir() (not t.TempDir()) because the Makefile sets GOTMPDIR,
// which redirects t.TempDir() away from the system temp root that underTempDir
// keys on.
func TestUnderTempDir(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{os.TempDir(), true},
		{tempPath("sub", "dir"), true},
		{"/var/folders/bk/abc/T/tsm-501/.fish/plumb-4", true},
		{"/tmp/tsm-501/plumb-4", true},
		{filepath.Join(string(filepath.Separator), "home", "fake", ".config"), false},
		{"", false},
	}
	for _, c := range cases {
		if got := underTempDir(c.path); got != c.want {
			t.Errorf("underTempDir(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

// tempPath joins parts under the system temp root (os.TempDir), the directory a
// session-manager hijack actually targets — independent of GOTMPDIR.
func tempPath(parts ...string) string {
	return filepath.Join(append([]string{os.TempDir()}, parts...)...)
}

// unsetEnv clears key for the duration of the test, restoring its prior state on
// cleanup. t.Setenv cannot unset, and the suite may run inside a tsm session
// that already exports the TSM_ORIG_* companions under test.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	prev, had := os.LookupEnv(key)
	_ = os.Unsetenv(key)
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, prev)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}
