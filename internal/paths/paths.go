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
// Concurrency: safe for concurrent use. xdg resolves its base directories once
// at import from the environment; plumb and its tests may change XDG_*/HOME at
// runtime, so each accessor refreshes via xdg.Reload under a mutex (these are
// startup/occasional calls, never a hot path).
package paths

import (
	"path/filepath"
	"sync"

	"github.com/adrg/xdg"
)

// appDir is the per-application subdirectory placed under each base directory.
const appDir = "plumb"

var reloadMu sync.Mutex

// base refreshes the xdg base directories from the current environment and
// returns the one selected by pick. Serialised because xdg.Reload mutates
// package-global state.
func base(pick func() string) string {
	reloadMu.Lock()
	defer reloadMu.Unlock()
	xdg.Reload()
	return pick()
}

// ConfigDir returns plumb's config directory (e.g. ~/.config/plumb on Linux,
// ~/Library/Application Support/plumb on macOS).
func ConfigDir() string { return filepath.Join(base(func() string { return xdg.ConfigHome }), appDir) }

// DataDir returns plumb's persistent-data directory (sessions, stats).
func DataDir() string { return filepath.Join(base(func() string { return xdg.DataHome }), appDir) }

// StateDir returns plumb's state directory (logs and other regenerable state).
func StateDir() string { return filepath.Join(base(func() string { return xdg.StateHome }), appDir) }

// CacheDir returns plumb's cache directory (socket, pid, locks, heap profiles).
func CacheDir() string { return filepath.Join(base(func() string { return xdg.CacheHome }), appDir) }
