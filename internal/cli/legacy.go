package cli

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/paths"
)

// Legacy migration and fencing (PLAN-530).
//
// An upgrade moves where the daemon lives. PLAN-527 added the ACCOUNT AUTHORITY
// and the canonical runtime directory; PLAN-528 made the launched binary the
// service profile's. Neither moved the daemon an older binary had already
// started: it is still serving at the runtime directory ITS environment
// resolved, which this process may not use at all. The thing that must never
// happen is a second daemon beside it (rule E1), so every launcher and the
// daemon itself ask two questions at start-up:
//
//   - is a daemon from an older build ALIVE at a legacy runtime location? Then
//     one is reused when it is the same build, and refused with an instruction
//     when it is not — never migrated silently, never spawned beside.
//   - is a legacy LIFETIME LOCK held by somebody else? Then an older launcher's
//     daemon owns that location even if its socket is gone (an unlinked socket
//     is exactly the state PLAN-529 exists for), and the launch fails closed.
//
// # One directory, five paths
//
// Everything a daemon publishes lives in ONE runtime directory: the MCP socket,
// the control socket, the pid file, the version file and the lifetime lock. They
// are resolved here by one function (legacyLocationAt) and never by joining a
// directory chosen in one place to a file name chosen in another. serve.go
// records what the other arrangement cost: a process that connected to one
// directory's socket while resolving the rest in another was half-migrated —
// `plumb web` dialled a control socket that was not there, doctor read a version
// file one directory over and called it missing, and `plumb restart` spawned the
// duplicate the redirect was meant to prevent. A compatibility decision read from
// another directory's version file is the same mistake with a worse outcome: it
// can call an incompatible daemon compatible and attach to it.

// legacyLocation is one runtime directory with every path a daemon publishes in
// it, resolved together.
type legacyLocation struct {
	Dir     string
	Socket  string
	Ctrl    string
	PID     string
	Version string
	Lock    string
}

// legacyLocationAt resolves every path of a runtime directory as a set. It
// creates nothing: naming where a legacy daemon might be must not bring the
// directory into existence, which is why this does not go through
// plumbRuntimeDir.
func legacyLocationAt(dir string) legacyLocation {
	return legacyLocation{
		Dir:     dir,
		Socket:  filepath.Join(dir, "plumb.sock"),
		Ctrl:    filepath.Join(dir, "plumb.ctrl.sock"),
		PID:     filepath.Join(dir, "plumb.pid"),
		Version: filepath.Join(dir, "plumb.version"),
		Lock:    filepath.Join(dir, "plumb.daemon.lock"),
	}
}

// currentLocation is the runtime location this process serves from and dials,
// resolved by the same function as a legacy one so the five paths can never be
// mixed between two directories. It is the ONLY place the current socket, control
// socket, pid, version and lifetime lock are named.
//
// It creates the runtime directory, which is what a caller about to bind a socket or
// take a lock needs. A caller that only NAMES these paths — the daemon census, doctor —
// must use currentLocationReadOnly instead: bringing into existence the thing you are
// inspecting is a defect of its own, and this one was proved with a seam root, where
// `plumb debug daemons` and doctor's census left the runtime directory on disk.
func currentLocation() legacyLocation { return legacyLocationAt(plumbRuntimeDir()) }

// currentLocationReadOnly is currentLocation without the side effect: the same five
// paths, resolved from the environment and the account, created nowhere. The census
// exists to report on a runtime directory that may be GONE (PLAN-529's self-heal case),
// so "no runtime directory" is an answer it has to be able to give — never a state it
// may repair by looking.
func currentLocationReadOnly() legacyLocation { return legacyLocationAt(paths.RuntimeDir()) }

// locationOfLock recovers the runtime location a lifetime lock belongs to. A
// launcher hands its daemon lock PATHS (see legacyLocksEnv), and the daemon has
// to resolve the rest of that location's paths — the pid, the version file, the
// sockets it may answer on — from the lock alone, because the environment the
// lock came from is not this process's.
func locationOfLock(lock string) legacyLocation {
	return legacyLocationAt(filepath.Dir(lock))
}

// legacyLocations lists every runtime location a daemon of an OLDER binary — or
// of this binary under a different environment — could be serving from, other
// than the one this process uses.
//
// Two sources, because they are two different histories. The environment-derived
// runtime directory is what an older binary used under this environment; the
// cache-directory location is the one RuntimeDir fell back to before
// $XDG_RUNTIME_DIR was preferred on Linux, and what a Linux box with no usable
// $XDG_RUNTIME_DIR resolves to anyway. Either can equal the location in use, in
// which case there is nothing legacy about it and it is dropped: the common
// unspoofed case, and always macOS without a spoofed HOME.
func legacyLocations() []legacyLocation {
	current := paths.RuntimeDir()
	var out []legacyLocation
	for _, dir := range []string{paths.EnvRuntimeDir(), paths.LegacyRuntimeDir()} {
		if dir == "" || dir == current {
			continue
		}
		if slices.ContainsFunc(out, func(l legacyLocation) bool { return l.Dir == dir }) {
			continue
		}
		out = append(out, legacyLocationAt(dir))
	}
	return out
}

// legacyDaemon is a legacy runtime location where a daemon answered, with the
// version that daemon published in the SAME directory.
type legacyDaemon struct {
	loc legacyLocation
	// running is the version the daemon published, "" when it published none —
	// an older build than the version file itself, which cannot be confirmed
	// compatible and is therefore not.
	running string
}

// Compatible reports whether the daemon at this location is the same build as
// this binary, which is the only state in which reusing it is safe.
//
// The version file is the only evidence available without talking to it, and an
// absent or unreadable one is NOT evidence of compatibility: every "cannot tell"
// here is a refusal, because attaching an upgraded client to a daemon of unknown
// build is how a migration turns into a version skew nobody can see. An empty
// file, a file that cannot be read, and a file naming another build all answer
// the same way.
func (d legacyDaemon) Compatible() bool {
	return d.running != "" && d.running == Version
}

// migrationError is the refusal for a legacy daemon this binary may not use. It
// names the two remedies that exist: `plumb restart` replaces the daemon with
// this build (stop by identity, then start canonical with the profile), and
// `plumb service install` records which binary the account service runs.
func (d legacyDaemon) migrationError() error {
	return fmt.Errorf("a plumb daemon from a different build is already running at %s (%s): "+
		"this plumb will not start a second daemon beside it, and it is not migrated on its own — "+
		"run `plumb restart` to replace it with this build, or `plumb service install` to record which binary the account service runs",
		d.loc.Socket, d.versionNote())
}

// daemonRefusalError is the DAEMON's own refusal, and it is a different sentence
// from migrationError on purpose: a serve that finds a legacy daemon may reuse a
// compatible one, while a daemon that finds one has nothing to add to it at all.
// Two daemons is the defect whatever their builds.
func (d legacyDaemon) daemonRefusalError() error {
	return fmt.Errorf("a plumb daemon is already running at %s (%s): refusing to start a second one beside it — "+
		"run `plumb restart` to replace it with this build, or `plumb stop` to stop it first",
		d.loc.Socket, d.versionNote())
}

// versionNote describes the build a legacy daemon published, in the terms the
// refusals above use: an absent version file is a fact about the daemon, not a
// failure of the reader, and it is never rendered as a version.
func (d legacyDaemon) versionNote() string {
	if d.running == "" {
		return "it published no version; this binary is " + Version
	}
	return "its version: " + d.running + "; this binary is " + Version
}

// legacyLocationsOf returns the runtime locations a set of held legacy locks
// belongs to. A launcher hands over lock PATHS, and the daemon resolves the rest
// of each location — the socket it may answer on, the pid and version files it
// publishes — from the lock alone, because the environment those locks came from
// is not this process's (see legacyLocksEnv).
func legacyLocationsOf(locks []heldLegacyLock) []legacyLocation {
	var out []legacyLocation
	for _, held := range locks {
		if held.Path == "" {
			continue
		}
		loc := locationOfLock(held.Path)
		if slices.ContainsFunc(out, func(l legacyLocation) bool { return l.Dir == loc.Dir }) {
			continue
		}
		out = append(out, loc)
	}
	return out
}

// legacyLocationsIncluding is every location a daemon has to consider: the ones
// its own environment names, and the ones a launcher fenced on its behalf. They
// are different sets in canonical mode — the launcher fenced the CLIENT's runtime
// locations, and a canonical daemon's environment is the service profile's — so a
// daemon that only looked at its own would miss exactly the locations the fence
// exists for.
func legacyLocationsIncluding(locks []heldLegacyLock) []legacyLocation {
	out := legacyLocations()
	for _, loc := range legacyLocationsOf(locks) {
		if slices.ContainsFunc(out, func(l legacyLocation) bool { return l.Dir == loc.Dir }) {
			continue
		}
		out = append(out, loc)
	}
	return out
}

// refuseBesideLegacyDaemon is the daemon's half of the fence: it reports the
// refusal when a legacy runtime location already has a daemon answering on it.
//
// The locks are already held by the time it runs, so it answers the question they
// cannot: a daemon that is ALREADY SERVING there. A launcher's fence stops a
// legacy launcher from starting one; it says nothing about a daemon that was
// started before this process existed. Fail closed — a direct `plumb daemon`
// exits rather than sitting beside it.
func refuseBesideLegacyDaemon(locks []heldLegacyLock) error {
	for _, d := range probeLegacyDaemonsAt(legacyLocationsIncluding(locks)) {
		return d.daemonRefusalError()
	}
	return nil
}

// probeLegacyDaemons dials the MCP socket of every legacy runtime location and
// reports the ones a daemon answered on.
//
// It is read-only in the strict sense PLAN-531 set for the census, and for the
// same reason: it takes NO lock (a probe that took the lifetime lock would make a
// concurrent `plumb serve` see it held, wait out its whole bounded start
// deadline and fail) and creates NOTHING — not the directory, not the socket,
// not a marker file. A dial to a path that is not there is how it finds nothing,
// which is the ordinary case.
func probeLegacyDaemons() []legacyDaemon { return probeLegacyDaemonsAt(legacyLocations()) }

// probeLegacyDaemonsAt is probeLegacyDaemons for an explicit set of locations.
// The daemon uses it for the locations a launcher handed over, which are known
// only by their lock paths and are not part of this process's environment.
func probeLegacyDaemonsAt(locs []legacyLocation) []legacyDaemon {
	var out []legacyDaemon
	for _, loc := range locs {
		conn, err := net.DialTimeout("unix", loc.Socket, time.Second)
		if err != nil {
			continue
		}
		_ = conn.Close()
		out = append(out, legacyDaemon{loc: loc, running: readVersionFile(loc.Version)})
	}
	return out
}

// readVersionFile reads a daemon's published build version. A missing or
// unreadable file is "" — never an error the caller has to distinguish, because
// every caller treats "" as "cannot be confirmed".
func readVersionFile(path string) string {
	data, err := os.ReadFile(path) //nolint:gosec // G304: a runtime directory plumb itself resolved, or the one a launcher handed over
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// attachLegacyDaemon returns a connection to a legacy daemon this process may
// use, or the refusal when one is running that it may not, or (nil, nil) when
// there is no legacy daemon at all.
//
// A COMPATIBLE daemon is reused rather than migrated: it is the same build, so
// every tool it serves is this build's, and stopping it to start an identical one
// would only drop the sessions connected to it. The log says what is pending, so
// the state is visible rather than assumed.
func attachLegacyDaemon() (net.Conn, error) {
	for _, d := range probeLegacyDaemons() {
		if !d.Compatible() {
			return nil, d.migrationError()
		}
		conn, err := net.DialTimeout("unix", d.loc.Socket, time.Second)
		if err != nil {
			continue // it stopped between the probe and this dial
		}
		slog.Info("serve: attached to a daemon at the legacy runtime directory — migration pending; run `plumb restart`",
			"socket", d.loc.Socket, "runtime_dir", d.loc.Dir, "version", d.running)
		return conn, nil
	}
	return nil, nil
}

// legacyLockHeldError reports that the lifetime lock of a legacy runtime
// location is held by somebody else, so an older launcher's daemon owns that
// location even though the authority is free.
//
// It is the fail-closed branch of PLAN-530's handover gap. Every lock a daemon
// holds is released the instant it exits, so between an old daemon dying and its
// successor taking over there is a window an old PROXY can win: it reconnects,
// finds the legacy lifetime lock free, and starts a legacy daemon. A launcher
// that takes the legacy locks before it starts a child closes that window, and a
// launcher that finds one already held has lost the race — so it starts nothing
// and says what to do instead. A duplicate daemon is never the answer.
type legacyLockHeldError struct {
	lock string
}

func (e legacyLockHeldError) Error() string {
	return fmt.Sprintf("a plumb daemon from an older binary holds %s and does not hold the launch authority: "+
		"this plumb will not start a second daemon beside it — run `plumb restart` to replace it with this build "+
		"(or `plumb stop`, then `plumb serve`)", e.lock)
}

// isLegacyLockHeld reports whether err is the held-legacy-lock refusal. The
// launcher's other failure paths must not swallow this one: it is the only one
// whose remedy is an instruction rather than a retry.
func isLegacyLockHeld(err error) bool {
	var held legacyLockHeldError
	return errors.As(err, &held)
}
