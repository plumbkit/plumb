// Package daemons answers one question, read-only: how many plumb daemons are
// running on this account, and is the one that should be?
//
// It exists because 64 leaked daemons went unnoticed until somebody ran `ps` by
// hand (PLAN-518). A daemon that has lost its runtime directory is silent: its
// socket is unlinked, so nothing dials it, and nothing complains. The census is
// the positive evidence E1 needs — "exactly one daemon, and it is the one
// holding the launch authority" — and it is deliberately incapable of changing
// anything: it opens no lock (opening one for the authority probe would
// momentarily HOLD it, and a concurrent `plumb serve` would then wait out its
// whole 30 s deadline and fail), removes no stale pid file (which
// `findAllDaemonPIDs` does), and signals nothing.
//
// # Fail closed
//
// An empty listing, an unreadable process entry, or a missing `lsof` is
// *unknown*, never "none": the report carries the reasons in Unknown, and a
// report with any unknown is never OK. A caller that prints "1 daemon" from a
// census that could not read the process table would be repeating the exact
// mistake this package exists to prevent.
//
// # Platform identification
//
// What proves a process is a plumb daemon differs by platform, on purpose:
// Linux reads each process's own argv (after skipping PF_FORKNOEXEC, so a
// forked-but-not-yet-exec'd child cannot impersonate its parent) and confirms
// with the authority lock. darwin has no /proc and Go forks before exec there
// too, so argv alone cannot confirm anything: a darwin daemon is confirmed by
// holding the authority lock or by owning a plumb socket, and an argv match
// that neither confirms nor owns is reported as a candidate rather than
// counted.
//
// Concurrency: safe for concurrent use; nothing here is shared mutable state.
package daemons

import (
	"fmt"
	"sort"
	"time"
)

// Options tells the census what the calling process knows about plumb's layout
// on this account, because the paths are resolved by the caller (the CLI, and
// from PLAN-528 the service profile) and this package deliberately does not
// carry a second copy of that rule.
type Options struct {
	// Authority is the file whose exclusive flock marks the account's launch
	// authority. The daemon holding it is the one clients should reach and the
	// only one E1 allows. "" when the caller cannot name one, which makes every
	// verdict Unknown rather than OK.
	Authority string
	// Sockets are the runtime socket paths that belong to plumb on this
	// account, most-preferred first (the canonical runtime dir, then any legacy
	// location). A process owning one of them is a daemon whatever its argv.
	Sockets []string
}

// Identity says how a process was recognised. It is ordered by strength, and
// Confirmed reports whether the weakest link in this platform's chain was met.
type Identity int

const (
	// IdentityCandidate: the process's own argv reads as `plumb daemon`, but
	// this platform cannot confirm a daemon that way (darwin: Go forks before
	// exec there too, so a forked child still shows its parent's argv).
	IdentityCandidate Identity = iota
	// IdentityArgv: argv says `plumb daemon` and this platform's argv is the
	// process's own (Linux, after the PF_FORKNOEXEC skip).
	IdentityArgv
	// IdentitySocket: the process owns a plumb runtime socket.
	IdentitySocket
	// IdentityAuthority: the process holds the launch authority lock.
	IdentityAuthority
)

// String names the identity for a human reader.
func (i Identity) String() string {
	switch i {
	case IdentityArgv:
		return "argv"
	case IdentitySocket:
		return "socket"
	case IdentityAuthority:
		return "authority"
	default:
		return "candidate"
	}
}

// Daemon is one candidate process with everything the platform could establish
// about it. Fields the platform cannot answer are left at their zero value and
// the reason goes in Report.Unknown — they are never guessed.
type Daemon struct {
	PID        int
	PPID       int
	Start      time.Time // zero when the platform cannot read it
	Executable string    // "" when unknown
	Argv       string    // "" when unknown
	// Identity is the strongest evidence found for this process.
	Identity Identity
	// HoldsAuthority reports that this process holds the flock on
	// Options.Authority.
	HoldsAuthority bool
	// OwnsSocket names the plumb socket paths this process owns, if any.
	OwnsSocket []string
}

// Confirmed reports whether this process is certainly a plumb daemon, as
// opposed to an argv look-alike that this platform cannot confirm.
func (d *Daemon) Confirmed() bool { return d.Identity >= IdentityArgv }

// SocketReading is what the census could establish about one socket path it was
// given. It is separate from a daemon's OwnsSocket because it also records the
// question that could NOT be answered — the difference between "nobody serves
// this path" and "this census cannot tell who does" (PLAN-529).
type SocketReading struct {
	// Path is the socket path the caller asked about.
	Path string
	// Answered reports whether the platform could say which candidate processes
	// hold this path open. False is "cannot tell", never "nobody".
	Answered bool
	// Owners names the candidates that answered yes. Empty with Answered true
	// means, positively, that no plumb daemon of this account is serving the
	// path — a stale or foreign socket at a path plumb owns.
	Owners []int
}

// Report is one reading of the account's daemons.
type Report struct {
	// Daemons is every candidate found, ordered by ascending PID.
	Daemons []Daemon
	// Authority is the lock path inspected ("" when none was named).
	Authority string
	// Sockets is one reading per socket path the caller named, in the order it
	// named them.
	Sockets []SocketReading
	// Unknown lists every reason this picture may be incomplete or a fact could
	// not be read. Non-empty means the report can never read as OK.
	Unknown []string
	// AuthorityHeldByUnidentified records that the authority lock IS held, but by
	// a process the census could not identify as a plumb daemon. It changes the
	// verdict: one daemon that appears not to hold the authority is a violation,
	// but when somebody unidentified holds it the honest answer is that the census
	// cannot tell — reporting a violation there would accuse a daemon that may
	// well be the holder.
	AuthorityHeldByUnidentified bool
	// AuthorityUnattributed records that the census could not attribute the
	// authority lock to anybody at all: it had no way to ask (no ownership probe
	// on this platform), or the source it asked could not be read (a failing
	// lsof, an unreadable descriptor scan). It is NOT the same as "nobody holds
	// it", which the census establishes by asking and being told nobody, and the
	// verdict turns on the difference: an absence of attribution is a gap in the
	// census, never evidence against a daemon, so one daemon that appears not to
	// hold the lock must read as unknown rather than as a violation whose remedy
	// is `plumb stop`.
	AuthorityUnattributed bool
}

// State is the census verdict, in the order of how much it can be trusted.
type State int

const (
	// StateNone: no daemon is running. A legitimate state (nothing has started
	// one yet), not a failure.
	StateNone State = iota
	// StateOK: exactly one confirmed daemon, and it holds the authority.
	StateOK
	// StateViolation: more than one confirmed daemon, or a confirmed daemon
	// that does not hold the authority. This is the E1 breach.
	StateViolation
	// StateUnknown: the picture is incomplete, so it cannot be called OK.
	StateUnknown
)

// String names the state for a human reader.
func (s State) String() string {
	switch s {
	case StateOK:
		return "OK"
	case StateViolation:
		return "VIOLATION"
	case StateUnknown:
		return "UNKNOWN"
	default:
		return "none"
	}
}

// Verdict is the E1 question answered: state, and the facts behind it.
type Verdict struct {
	State State
	// AuthorityPID is the pid holding the authority, 0 when none (or unknown).
	AuthorityPID int
	// Confirmed counts the processes certainly identified as daemons.
	Confirmed int
	// Candidates counts argv look-alikes this platform could not confirm.
	Candidates int
	// Detail is one line naming what was seen and why the state follows.
	Detail string
	// Remedy is what the reader should do next, "" when State is OK.
	Remedy string
}

// Verdict decides the E1 question for this report.
//
// Precedence is deliberate: a violation is reported even when some source was
// unreadable, because two daemons in front of you outweigh a missing lsof; and
// an unreadable source is reported as Unknown even when exactly one daemon was
// seen, because "exactly one" from a census that may have missed processes is
// not evidence. The two exceptions are both gaps in the census rather than facts
// about the daemon, so they read as Unknown instead of as a violation against an
// innocent process: a lock held by a process this census could not identify, and
// a lock it could not attribute at all. `plumb stop` is only ever printed for
// positive evidence — more than one confirmed daemon, or one confirmed daemon the
// census positively established is not the holder.
func (r *Report) Verdict() Verdict {
	var confirmed, candidates []Daemon
	var holder int
	for _, d := range r.Daemons {
		switch {
		case d.HoldsAuthority:
			holder = d.PID
			confirmed = append(confirmed, d)
		case d.Confirmed():
			confirmed = append(confirmed, d)
		default:
			candidates = append(candidates, d)
		}
	}
	v := Verdict{
		AuthorityPID: holder,
		Confirmed:    len(confirmed),
		Candidates:   len(candidates),
	}

	if len(confirmed) > 1 {
		v.State = StateViolation
		v.Detail = fmt.Sprintf("%d plumb daemons are running (expected exactly 1): %s", len(confirmed), pidList(confirmed))
		v.Remedy = remedy(r.Authority)
		return v
	}
	if len(confirmed) == 1 && holder == 0 && !r.AuthorityHeldByUnidentified {
		if r.AuthorityUnattributed {
			// The census could not say who holds the lock, so it cannot say this
			// daemon does not: an absence of attribution is not evidence against
			// it, and accusing it would hand the reader `plumb stop`.
			v.State = StateUnknown
			v.Detail = fmt.Sprintf("cannot tell: plumb daemon %d is running, but the census could not confirm that it holds the launch authority %s — %s",
				confirmed[0].PID, backtick(r.Authority), firstUnknown(r.Unknown))
			v.Remedy = unknownRemedy
			return v
		}
		v.State = StateViolation
		v.Detail = fmt.Sprintf("plumb daemon %d does not hold the launch authority %s", confirmed[0].PID, backtick(r.Authority))
		v.Remedy = remedy(r.Authority)
		return v
	}
	if len(r.Unknown) > 0 {
		v.State = StateUnknown
		v.Detail = fmt.Sprintf("cannot tell: %d confirmed daemon(s), %d candidate(s), and %d unreadable source(s) — %s",
			len(confirmed), len(candidates), len(r.Unknown), firstUnknown(r.Unknown))
		v.Remedy = unknownRemedy
		return v
	}
	if holder != 0 {
		v.State = StateOK
		v.Detail = fmt.Sprintf("1 plumb daemon (%d), holding %s", holder, backtick(r.Authority))
		return v
	}
	v.State = StateNone
	v.Detail = "no plumb daemon is running"
	return v
}

// remedy is the one instruction that actually clears a violation. Stop is safe
// here because the daemon holding the authority is the one every client reaches:
// the survivors are the ones nobody can reach, and they cannot be found by
// scanning for a socket that is still bound.
func remedy(authority string) string {
	if authority == "" {
		return "run `plumb stop` (it stops every daemon of this account), then reconnect; if survivors remain, stop them by pid"
	}
	return "run `plumb stop`, which stops every daemon of this account; the survivors hold no socket anybody can reach, so stop any that remain by pid"
}

// unknownRemedy is what a reader can do about a census that could not read
// everything it asked. It is deliberately not `plumb stop`: an unreadable source
// is a gap in the census, not a fault in a daemon, and the two verdicts that use
// this remedy both mean "nobody can say yet".
const unknownRemedy = "re-run as the account that owns the daemon, on a host where the process table and lsof are readable; the verdict is only trustworthy from a complete reading"

func pidList(ds []Daemon) string {
	pids := make([]int, 0, len(ds))
	for _, d := range ds {
		pids = append(pids, d.PID)
	}
	sort.Ints(pids)
	return fmt.Sprintf("%v", pids)
}

// firstUnknown names the first reason a picture is incomplete. A report with no
// reason recorded is still a report that cannot be trusted, so it says that
// rather than panicking on the empty slice.
func firstUnknown(reasons []string) string {
	if len(reasons) == 0 {
		return "no reason was recorded"
	}
	return reasons[0]
}

// backtick renders a path for a message, or a dash when the caller named none.
func backtick(p string) string {
	if p == "" {
		return "(no authority named)"
	}
	return "`" + p + "`"
}
