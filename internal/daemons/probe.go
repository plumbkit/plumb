package daemons

import (
	"fmt"
	"os"
	"slices"
	"sort"
)

// probe is everything the census asks the operating system. It is a struct of
// functions rather than an interface, the same shape separateenv.Probe uses, so
// a test can answer only the two questions it cares about: a field left nil is a
// question this build cannot ask, and the census reports it as unknown instead
// of inventing an answer.
//
// Every function must be read-only. The authority lock in particular cannot be
// probed by taking it — acquireDaemonLock opens with O_CREATE|O_RDWR, so a
// "check" that succeeded would hold the lock for an instant, and a concurrent
// `plumb serve` would then wait out its whole 30 s deadline and fail with
// errDaemonHeldButSilent.
type probe struct {
	// processes returns this user's processes whose own argv reads as a plumb
	// daemon, plus a reason for each fact it could not read. A nil function means
	// this build cannot read a process table at all.
	processes func(uid int) (found []Daemon, unknown []string)
	// owners returns which of candidates has the file at path open.
	//
	// "Has it open" is the census's whole notion of ownership, and on Linux it is
	// the only correct way to ask who holds the flock: /proc/locks names the
	// process that ACQUIRED a lock, and #649 hands the daemon's lifetime lock
	// over inherited from the launcher — so the flock entry keeps naming `plumb
	// serve` long after the daemon owns it (measured: /proc/locks named the
	// launcher while the daemon's fd 3 was the only open descriptor). The daemon,
	// by contrast, holds the file open for its whole life, on both platforms.
	owners func(path string, candidates []int) ([]int, error)
	// lockHeld reports whether the file at path is held by a flock, without saying
	// by whom. It exists as a safety net for the ownership question above: on a
	// platform where ownership is asked only about known candidates, a lock that
	// is held while no candidate has it open is a fact worth reporting as unknown
	// rather than letting it read as "nobody holds the authority". Nil where the
	// platform cannot answer.
	lockHeld func(path string) (bool, error)
}

// osProbe is the real probe for this platform. Options are passed in because a
// platform may need to know every path the census will ask about before it is
// asked: darwin answers all of them with one `lsof` call, and a call per path
// costs a process launch each (~150 ms).
func osProbe(opts Options) probe { return platformProbe(opts) }

// Census reads the account's daemons through the operating system's own view.
func Census(opts Options) Report { return census(osProbe(opts), opts) }

// census combines what the platform knows into one report. It never fails: a
// source it cannot read becomes an Unknown reason, because a caller that got an
// error would print nothing where a caller that gets a report can still say
// "cannot tell".
func census(p probe, opts Options) Report {
	r := Report{Authority: opts.Authority}
	if p.processes == nil {
		r.Unknown = append(r.Unknown, "this build cannot read the process table")
	} else {
		found, unknown := p.processes(os.Getuid())
		r.Daemons = append(r.Daemons, found...)
		r.Unknown = append(r.Unknown, unknown...)
	}
	if opts.Authority == "" {
		r.Unknown = append(r.Unknown, "no launch-authority lock was named, so no daemon could be confirmed as the authority holder")
	} else {
		// Only an ANSWERED question can make "no holder" a fact about the daemon;
		// a question the census could not answer is recorded as a gap, so the
		// verdict does not accuse a daemon of losing a lock nobody could read.
		r.AuthorityUnattributed = !r.mark(p, opts.Authority, func(d *Daemon) {
			d.HoldsAuthority = true
			d.raiseTo(IdentityAuthority)
		})
		r.reportUnidentifiedHolder(p, opts.Authority)
	}
	for _, path := range opts.Sockets {
		if path == "" {
			continue
		}
		reading := SocketReading{Path: path}
		reading.Answered = r.mark(p, path, func(d *Daemon) {
			d.OwnsSocket = append(d.OwnsSocket, path)
			d.raiseTo(IdentitySocket)
		})
		reading.Owners = r.socketOwners(path)
		r.Sockets = append(r.Sockets, reading)
	}
	// A legacy lifetime lock is asked about the same way and for the same reason
	// the authority is: it is a file a daemon holds open for its whole life, so its
	// holders are daemons whatever their argv says, and a daemon of an older
	// binary's runtime directory is exactly what E1 wants named.
	for _, path := range opts.LegacyLocks {
		r.mark(p, path, func(d *Daemon) {
			d.HoldsLegacyLock = append(d.HoldsLegacyLock, path)
			d.raiseTo(IdentityLifetimeLock)
		})
	}
	sort.Slice(r.Daemons, func(i, j int) bool { return r.Daemons[i].PID < r.Daemons[j].PID })
	return r
}

// socketOwners lists the processes marked as holding path open, by pid.
func (r *Report) socketOwners(path string) []int {
	var pids []int
	for _, d := range r.Daemons {
		if slices.Contains(d.OwnsSocket, path) {
			pids = append(pids, d.PID)
		}
	}
	return pids
}

// mark asks the platform which candidate processes hold path, records them, and
// reports whether the question was ANSWERED: true when the platform answered,
// naming holders or naming nobody, false when this build cannot ask or the source
// could not be read. A question that was not answered becomes an Unknown reason —
// never silence, and never the answer "nobody holds it".
func (r *Report) mark(p probe, path string, record func(*Daemon)) bool {
	if path == "" {
		return false
	}
	if p.owners == nil {
		r.Unknown = append(r.Unknown, "this build cannot tell which process holds "+path)
		return false
	}
	pids, err := p.owners(path, r.pids())
	if err != nil {
		r.Unknown = append(r.Unknown, fmt.Sprintf("cannot tell which process holds %s: %v", path, err))
		return false
	}
	for _, pid := range pids {
		record(r.ensure(pid))
	}
	return true
}

// reportUnidentifiedHolder turns a real gap into a flagged unknown: ownership is
// asked only about processes the census already found (a full descriptor scan on
// every daemon_info call is not affordable), so a lock held by a process that
// looks like nothing at all would otherwise read as "no daemon holds the
// authority" — which is a violation report against an innocent daemon.
func (r *Report) reportUnidentifiedHolder(p probe, authority string) {
	for _, d := range r.Daemons {
		if d.HoldsAuthority {
			return
		}
	}
	if p.lockHeld == nil {
		return
	}
	held, err := p.lockHeld(authority)
	switch {
	case err != nil:
		r.Unknown = append(r.Unknown, fmt.Sprintf("cannot tell whether %s is held: %v", authority, err))
	case held:
		r.AuthorityHeldByUnidentified = true
		r.Unknown = append(r.Unknown, authority+" is held by a process this census could not identify as a plumb daemon")
	}
}

// pids lists the candidates found so far, for a probe that only asks about
// processes already under suspicion.
func (r *Report) pids() []int {
	out := make([]int, 0, len(r.Daemons))
	for _, d := range r.Daemons {
		out = append(out, d.PID)
	}
	return out
}

// ensure returns the entry for pid, adding a bare one when the process was not
// in the argv list. That case is real and worth naming: a daemon started from a
// renamed or rebuilt binary holds its files while matching no argv, and it must
// still be counted.
func (r *Report) ensure(pid int) *Daemon {
	for i := range r.Daemons {
		if r.Daemons[i].PID == pid {
			return &r.Daemons[i]
		}
	}
	r.Daemons = append(r.Daemons, Daemon{PID: pid})
	return &r.Daemons[len(r.Daemons)-1]
}

// raiseTo records the strongest evidence found for a process.
func (d *Daemon) raiseTo(id Identity) {
	if id > d.Identity {
		d.Identity = id
	}
}
