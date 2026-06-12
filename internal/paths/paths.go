// Package paths is the single source of truth for plumb's OS-appropriate base
// directories. It delegates to github.com/adrg/xdg — the de-facto cross-platform
// base-directory resolver — so plumb carries no hand-rolled per-OS path logic.
// Every plumb-owned file lives under an app-named subdirectory of the relevant
// base:
//
//   - macOS  : config/data/state → ~/Library/Application Support/plumb,
//     cache → ~/Library/Caches/plumb
//   - Linux  : the XDG base directory spec (XDG_CONFIG_HOME, XDG_DATA_HOME,
//     XDG_STATE_HOME, XDG_CACHE_HOME, with the usual ~/.config etc.
//     fallbacks)
//   - Windows: %AppData% / %LocalAppData% per xdg
//
// Logs are the single exception: macOS keeps user logs in ~/Library/Logs (the
// directory Console.app reads), which no XDG base maps to, so LogDir special-
// cases macOS while Linux/Windows follow the state dir. See LogDir.
//
// Session-manager hijack guard: shell session managers (notably tsm) export an
// XDG_*_HOME pointing at a per-session throwaway temp directory, stashing the
// real value in a TSM_ORIG_<var> companion. Honouring the temp value silently
// relocates plumb's config/data into a directory that vanishes with the session
// — so when that companion is present and the live var resolves under a temp
// root, plumb recovers the stashed original (or falls back to the OS-native
// default). The recovery is recorded once per variable and exposed via
// RecoveredHijacks so callers can surface it in their own idiom — the daemon
// logs it, `plumb config show` renders a banner. See unhijack.
//
// Concurrency: safe for concurrent use. xdg resolves its base directories once
// at import from the environment; plumb and its tests may change XDG_*/HOME at
// runtime, so each accessor refreshes via xdg.Reload under a mutex (these are
// startup/occasional calls, never a hot path).
package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/adrg/xdg"
)

// appDir is the per-application subdirectory placed under each base directory.
const appDir = "plumb"

// Hijack describes a session-manager rewrite of an XDG base variable that plumb
// detected and recovered from.
type Hijack struct {
	EnvVar string // the XDG variable that was rewritten (e.g. XDG_CONFIG_HOME)
	From   string // the throwaway temp value plumb ignored
	To     string // the value plumb used instead ("" ⇒ OS-native default)
}

var (
	reloadMu sync.Mutex
	// recovered records each XDG var plumb has recovered from a hijack, once per
	// variable, so callers can surface it. Guarded by reloadMu (held whenever
	// read or written).
	recovered = map[string]Hijack{}
	// recoveryDisabledInTest turns off hijack recovery while running under
	// `go test`. Tests across the codebase sandbox XDG base dirs into temp
	// directories but inherit the developer shell's TSM_ORIG_* companions, which
	// would otherwise make recovery redirect those sandboxes back to the real
	// base dirs — a test writing a fixture then clobbers the user's real config.
	// Guarded by reloadMu. This package's own recovery tests flip it false via
	// enableRecoveryForTest to exercise the real behaviour with fake values.
	recoveryDisabledInTest = testing.Testing()
)

// RecoveredHijacks returns the session-manager hijacks plumb has detected and
// recovered from so far this process, sorted by variable name. Empty in the
// common case. A caller observes a hijack only after the relevant base accessor
// has run (e.g. config.Load resolves ConfigDir), so query it after resolving
// the paths you care about.
func RecoveredHijacks() []Hijack {
	reloadMu.Lock()
	defer reloadMu.Unlock()
	out := make([]Hijack, 0, len(recovered))
	for _, h := range recovered {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EnvVar < out[j].EnvVar })
	return out
}

// appPath joins appDir onto the resolved base for envVar.
func appPath(envVar string, pick func() string) string {
	return filepath.Join(base(envVar, pick), appDir)
}

// base refreshes the xdg base directories from the current environment and
// returns the one selected by pick. envVar names the XDG override for this base
// so a session-manager hijack of it can be undone before xdg reads it.
// Serialised because xdg.Reload mutates package-global state (and the hijack
// guard mutates the environment around the reload).
func base(envVar string, pick func() string) string {
	reloadMu.Lock()
	defer reloadMu.Unlock()
	defer unhijack(envVar)()
	xdg.Reload()
	return pick()
}

// ConfigDir returns plumb's config directory (e.g. ~/.config/plumb on Linux,
// ~/Library/Application Support/plumb on macOS).
func ConfigDir() string { return appPath("XDG_CONFIG_HOME", func() string { return xdg.ConfigHome }) }

// DataDir returns plumb's persistent-data directory (sessions, stats).
func DataDir() string { return appPath("XDG_DATA_HOME", func() string { return xdg.DataHome }) }

// StateDir returns plumb's state directory (regenerable state). For logs use
// LogDir, which diverges from this on macOS.
func StateDir() string { return appPath("XDG_STATE_HOME", func() string { return xdg.StateHome }) }

// LogDir returns plumb's log directory. Logs are the single role where the macOS
// convention diverges from XDG: macOS keeps user logs in ~/Library/Logs (the
// directory Console.app reads), which no xdg base resolves to — so this
// deliberately special-cases macOS. Linux ($XDG_STATE_HOME) and Windows
// (%LocalAppData%) follow the state dir, the correct place for logs there.
//
//   - macOS  : ~/Library/Logs/plumb
//   - Linux  : $XDG_STATE_HOME/plumb  (fallback ~/.local/state/plumb)
//   - Windows: %LocalAppData%\plumb
func LogDir() string {
	if runtime.GOOS == "darwin" {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "Library", "Logs", appDir)
		}
		// Degenerate (no HOME): fall back to the state dir.
	}
	return StateDir()
}

// CacheDir returns plumb's cache directory (socket, pid, locks, heap profiles).
func CacheDir() string { return appPath("XDG_CACHE_HOME", func() string { return xdg.CacheHome }) }

// unhijack detects a shell session manager having rewritten envVar to a
// throwaway temp directory and, if so, temporarily restores the pre-hijack value
// (or clears the override entirely so xdg falls back to the OS-native default).
// It returns a function that restores the environment to exactly its prior
// state. A no-op when envVar was not hijacked. The caller must hold reloadMu.
func unhijack(envVar string) func() {
	value, isHijacked := recoveredBase(envVar)
	if !isHijacked {
		return func() {}
	}
	recordHijack(envVar, os.Getenv(envVar), value)
	old, had := os.LookupEnv(envVar)
	if value == "" {
		_ = os.Unsetenv(envVar)
	} else {
		_ = os.Setenv(envVar, value)
	}
	return func() {
		if had {
			_ = os.Setenv(envVar, old)
		} else {
			_ = os.Unsetenv(envVar)
		}
	}
}

// recoveredBase decides whether envVar has been hijacked into a temp directory
// by a session manager and, if so, what value to use instead. It only acts when
// the tsm-style TSM_ORIG_<var> companion is present — that companion is the
// unambiguous signal that something rewrote the variable. Returns the
// recovered value (empty ⇒ drop the override and use the OS-native default) and
// whether a recovery applies.
//
// Recovery is disabled by default under `go test` (see recoveryDisabledInTest).
// A test in any package that sandboxes a base dir into a temp directory
// (t.TempDir + t.Setenv) inherits the developer's TSM_ORIG_* companion from a
// tsm-managed shell; without this guard the companion would make recovery
// redirect the sandbox back to the real ~/.config, so a test writing a config
// fixture would clobber the user's actual config. Tests must own their base
// dirs. This package's own recovery tests, which validate the recovery itself
// with controlled fake values, opt back in via enableRecoveryForTest.
func recoveredBase(envVar string) (value string, isHijacked bool) {
	// Read under reloadMu (held by the base() caller), the same lock
	// enableRecoveryForTest takes when flipping the flag.
	if recoveryDisabledInTest {
		return "", false
	}
	orig, hasOrig := os.LookupEnv("TSM_ORIG_" + envVar)
	if !hasOrig {
		return "", false
	}
	cur := os.Getenv(envVar)
	if cur == "" || !underTempDir(cur) {
		return "", false // not (or no longer) pointing at a throwaway dir
	}
	if orig != "" && !underTempDir(orig) {
		return orig, true // recover the stashed pre-hijack value
	}
	return "", true // original unusable too: fall back to the OS-native default
}

// underTempDir reports whether p resolves inside a system temp root.
func underTempDir(p string) bool {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = filepath.Clean(p)
	}
	for _, root := range tempRoots() {
		if root == "" {
			continue
		}
		r, err := filepath.Abs(root)
		if err != nil {
			r = filepath.Clean(root)
		}
		rel, err := filepath.Rel(r, abs)
		if err != nil {
			continue
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		return true
	}
	return false
}

// tempRoots lists the directories plumb treats as throwaway. os.TempDir honours
// $TMPDIR (the per-user /var/folders/.../T sandbox on macOS); the rest are the
// conventional fixed roots, including the /private aliases macOS reports after
// symlink resolution.
func tempRoots() []string {
	return []string{
		os.TempDir(),
		"/tmp",
		"/var/folders",
		"/private/var/folders",
		"/private/tmp",
	}
}

// recordHijack records a recovery once per envVar so RecoveredHijacks can report
// it. The caller must hold reloadMu.
func recordHijack(envVar, from, to string) {
	if _, seen := recovered[envVar]; seen {
		return
	}
	recovered[envVar] = Hijack{EnvVar: envVar, From: from, To: to}
}
