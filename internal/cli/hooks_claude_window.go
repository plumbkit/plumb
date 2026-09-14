package cli

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/config"
)

// How long the Claude Code Stop-hook watcher watches, and where that answer
// comes from. Split from hooks_claude.go, which owns the handlers and the watch
// loop itself; everything here decides a duration and nothing here polls.

const (
	claudeWakeWindowDefault   = 300 * time.Second
	claudeWakeIntervalDefault = 7 * time.Second
	// claudeWakePeerWindowDefault is the ceiling the base window may extend to
	// while a live peer shares the workspace. See wakeWindowPair.
	claudeWakePeerWindowDefault = 3600 * time.Second
	// claudeStopTimeoutSlack keeps the client's own hook timeout above the
	// watcher's ceiling: a timeout at or below it would kill the watcher before
	// it finished watching, which looks exactly like a wake that never fires.
	claudeStopTimeoutSlack = 30 * time.Second
)

// wakeWindowPair is how long a watcher may poll: base always, extending toward
// peak for as long as a live peer shares the workspace.
//
// The split exists because the two cases have genuinely different worth. Nobody
// outside this workspace can write to this mailbox uninvited, so a session with
// no peer is holding a resident process to watch for something that cannot
// arrive — it keeps the short window this hook always had. A session that DOES
// have a peer is the case the whole mechanism exists for, and 300s of coverage
// for an agent idle between turns was never enough: mail landing at T+6min woke
// nothing until that session's next turn ended, which for an idle agent may be
// never.
type wakeWindowPair struct {
	base time.Duration
	peak time.Duration // 0 disables the extension entirely
}

// ceiling is the longest a watcher armed under these windows can possibly live,
// and therefore what the installed handler's timeout must cover.
func (w wakeWindowPair) ceiling() time.Duration {
	if w.peak > w.base {
		return w.peak
	}
	return w.base
}

// slideDeadline moves a watch deadline one base window past now, but only while
// a peer could still write and never beyond the ceiling. Returning the deadline
// rather than mutating one keeps the rule readable in isolation: extension is a
// function of this poll's observations, not of accumulated state.
func (w wakeWindowPair) slideDeadline(deadline, hardStop, now time.Time, resolved bool, peers int) time.Time {
	if w.peak <= 0 || !resolved || peers <= 0 {
		return deadline
	}
	if next := now.Add(w.base); next.After(deadline) {
		deadline = next
	}
	if deadline.After(hardStop) {
		return hardStop
	}
	return deadline
}

// withEnv applies the tuning overrides. They are read LAST, so an operator can
// tune a machine without editing config — and so a test can pin both windows
// without depending on whatever global config the developer happens to have.
func (w wakeWindowPair) withEnv() wakeWindowPair {
	w.base = envSeconds("PLUMB_WAKE_WINDOW", w.base)
	// Zero is a meaningful value for the ceiling ("never extend"), so this one
	// cannot use envSeconds, which reads 0 as "unset, keep the fallback".
	w.peak = envSecondsAllowingZero("PLUMB_WAKE_PEER_WINDOW", w.peak)
	return w
}

// collabWakeWindows reads the pair out of a RESOLVED [collab] block. After
// config.Load the value is authoritative — an absent key already carries the
// compiled default — so a zero peak here means the file genuinely asked for
// "no extension" rather than "unset". A non-positive base is the one exception:
// it reads as "use the default", matching every other 0-means-default key in
// [collab].
func collabWakeWindows(c config.CollabConfig) wakeWindowPair {
	w := wakeWindowPair{
		base: time.Duration(c.WakeWindowSeconds) * time.Second,
		peak: time.Duration(c.WakePeerWindowSeconds) * time.Second,
	}
	if w.base <= 0 {
		w.base = claudeWakeWindowDefault
	}
	if w.peak < 0 {
		w.peak = 0
	}
	return w
}

func compiledWakeWindows() wakeWindowPair {
	return wakeWindowPair{base: claudeWakeWindowDefault, peak: claudeWakePeerWindowDefault}
}

// globalWakeWindows resolves the MACHINE-wide pair: compiled defaults, then the
// global config file, then the env overrides. Project config is deliberately not
// consulted — this is the pair the installed handler's timeout is derived from,
// and the installing process has no workspace to read one for.
func globalWakeWindows() wakeWindowPair {
	cfg, err := config.Load()
	if err != nil {
		return compiledWakeWindows().withEnv()
	}
	return collabWakeWindows(cfg.Collab).withEnv()
}

// runtimeWakeWindows resolves the pair for one workspace.
//
// A project may NARROW its windows and may not widen them. The handler that
// runs this hook is installed once, machine-wide, with one timeout derived from
// the global ceiling; a project asking for longer would simply be killed at that
// timeout mid-watch, with nothing in any output saying so. Clamping is the
// honest version of a limit that exists whether or not plumb enforces it.
func runtimeWakeWindows(root string) wakeWindowPair {
	cfg, err := config.Load()
	if err != nil {
		return compiledWakeWindows().withEnv()
	}
	global := collabWakeWindows(cfg.Collab).withEnv()
	if strings.TrimSpace(root) == "" {
		return global
	}
	proj, err := config.LoadProject(cfg, root)
	if err != nil {
		return global
	}
	w := collabWakeWindows(proj.Collab).withEnv()
	ceiling := global.ceiling()
	if w.base > ceiling {
		w.base = ceiling
	}
	if w.peak > ceiling {
		w.peak = ceiling
	}
	return w
}

// wakeInterval is clamped to the base window: a poll gap longer than the watch
// it paces would park the watcher — holding this session's lock, so the session
// cannot arm another — well past its own deadline, since the loop re-checks the
// deadline only after sleeping. The BASE is the right clamp even when a peer can
// extend the deadline, because the extension is decided one poll at a time: a
// watcher that cannot poll within the base window can never observe the peer
// that would have extended it.
func wakeInterval(base time.Duration) time.Duration {
	interval := envSeconds("PLUMB_WAKE_INTERVAL", claudeWakeIntervalDefault)
	if interval > base {
		return base
	}
	return interval
}

// envSeconds reads a whole-second duration override, ignoring anything that is
// not a positive integer — a malformed tuning value must not disable the hook.
func envSeconds(name string, fallback time.Duration) time.Duration {
	if raw := strings.TrimSpace(os.Getenv(name)); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return fallback
}

// envSecondsAllowingZero is envSeconds for a knob where 0 is a real setting
// rather than "unset". Only the peer ceiling is such a knob: 0 there means
// "never extend past the base window", which an operator must be able to ask
// for. Negative and malformed values still fall through to the fallback.
func envSecondsAllowingZero(name string, fallback time.Duration) time.Duration {
	if raw := strings.TrimSpace(os.Getenv(name)); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			return time.Duration(n) * time.Second
		}
	}
	return fallback
}
