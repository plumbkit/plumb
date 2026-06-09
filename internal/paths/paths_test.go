package paths

import (
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
