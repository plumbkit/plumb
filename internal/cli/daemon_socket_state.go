package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/plumbkit/plumb/internal/daemons"
)

// The socket path as the rest of the world sees it (PLAN-529).
//
// A daemon publishes the socket it serves, and the identity of that socket file,
// in the account authority's metadata — written through the held fd, never by
// replacing the lock file. That record is what makes the daemon's own hygiene
// visible from outside without dialling the socket or taking a lock: `plumb debug
// daemons` and `plumb doctor` compare the file at the path with the one the
// record names, and say so when a live daemon's socket has been deleted or
// replaced. That is the state that used to be silent — the daemon kept running
// and nothing dialled it — and the state 64 leaked daemons reached.

// daemonSocketPublicationFreshness is how old a published record may be and still
// count as evidence that its holder is running. The daemon refreshes the record
// on every hygiene tick (daemonSocketCheckInterval), so a live holder's record is
// at most one interval old; three leaves room for a slow tick or a briefly
// suspended machine.
//
// The freshness rule is what separates a live holder from a record left behind by
// a daemon that died whose pid the kernel has since recycled: without it, an
// unrelated process that inherited the pid would make a deleted socket path read
// as "gone while the daemon is alive".
const daemonSocketPublicationFreshness = 3 * daemonSocketCheckInterval

// socketPublication is the socket record a live authority holder left.
type socketPublication struct {
	pid  int
	path string
	// id is the identity of the socket file the holder bound. Unknown (zero) in
	// a record written before PLAN-529, or when the holder could not read it.
	id fileIdentity
}

// daemonSocketPublication reads the socket the authority holder published, when
// that record is worth trusting: it names a live pid, a path and a socket, and it
// was refreshed recently enough to be about a running daemon.
//
// Reading the authority lock file is not taking it: this is a plain read that
// creates nothing and locks nothing, and it is deliberately how the census learns
// about the socket. A probe that took the lock for an instant would make a
// concurrent `plumb serve` wait out its whole start deadline and then fail, which
// is the one thing the census must never do.
func daemonSocketPublication() (socketPublication, bool) {
	path, err := authorityPath()
	if err != nil {
		// A test binary with no account seam cannot resolve the authority, and a
		// release build on a host whose account database cannot answer cannot
		// either. A reader simply has no record then — never an error to print.
		return socketPublication{}, false
	}
	m, ok := readAuthority(path)
	if !ok || m.Socket == "" || m.PID <= 0 {
		return socketPublication{}, false
	}
	if !processAlive(m.PID) || time.Since(m.Published) > daemonSocketPublicationFreshness {
		return socketPublication{}, false
	}
	return socketPublication{
		pid:  m.PID,
		path: m.Socket,
		id:   fileIdentity{dev: m.SocketDev, ino: m.SocketIno, ctime: m.SocketCtime},
	}, true
}

// socketPathState is one socket path as a process other than the daemon sees it.
type socketPathState struct {
	// Path is the socket path.
	Path string
	// Exists is what the filesystem says about the path.
	Exists bool
	// Unreadable records that the path could not be inspected at all, so nothing
	// is claimed about it.
	Unreadable bool
	// Publisher is the pid of the live daemon that published this path as the
	// socket it serves, or 0 when no live daemon published it.
	Publisher int
	// IdentityKnown reports that the publisher recorded which file it bound.
	IdentityKnown bool
	// IdentityMatches reports that the file at Path is still that one.
	IdentityMatches bool
	// OwnedBy lists the daemons the census found holding the path open.
	OwnedBy []int
	// Answered reports whether the census could ask which candidates hold the
	// path. False is "cannot tell", never "nobody".
	Answered bool
}

// fault is the state that must not read as healthy: a path a live daemon
// published that no longer holds the socket it bound. It is empty when the path
// is fine or when this process has no evidence either way — an absence of
// evidence is never turned into an accusation.
func (s socketPathState) fault() string {
	if s.Unreadable || s.Publisher == 0 {
		return ""
	}
	switch {
	case !s.Exists:
		return fmt.Sprintf("gone while plumb daemon %d is alive and serving it", s.Publisher)
	case s.IdentityKnown && !s.IdentityMatches:
		return fmt.Sprintf("replaced — the file there is not the socket plumb daemon %d bound, and that daemon deliberately will not remove it", s.Publisher)
	default:
		return ""
	}
}

// note is what is worth saying about a path with no fault: a socket file nothing
// serves, or a question the census could not answer. A note is information, not a
// verdict — a machine whose daemon was killed leaves a socket file behind, and
// the next `plumb serve` clears it.
func (s socketPathState) note() string {
	if s.fault() != "" {
		return ""
	}
	switch {
	case s.Unreadable:
		return "cannot be inspected"
	case s.Publisher != 0:
		return "" // a live daemon published it and the file is the one it bound
	case !s.Exists:
		return ""
	case !s.Answered:
		return "present; the census could not tell which daemon serves it"
	case len(s.OwnedBy) == 0:
		return "present, but no plumb daemon of this account holds it open — a stale or foreign socket"
	default:
		return ""
	}
}

// daemonSocketStates is the socket half of the census: one state per socket path
// the census was given, plus the path the authority holder published when that is
// not one of them — a daemon whose environment resolved elsewhere is still the
// account's daemon, and the socket it published is the one clients reach.
func daemonSocketStates(report daemons.Report) []socketPathState {
	pub, published := daemonSocketPublication()
	states := make([]socketPathState, 0, len(report.Sockets)+1)
	seen := make(map[string]bool, len(report.Sockets))
	for _, reading := range report.Sockets {
		seen[reading.Path] = true
		states = append(states, socketPathStateFor(reading.Path, reading.Answered, reading.Owners, pub, published))
	}
	if published && !seen[pub.path] {
		states = append(states, socketPathStateFor(pub.path, false, nil, pub, true))
	}
	return states
}

// socketPathStateFor builds one state: what the filesystem says about the path,
// what a live daemon published about it, and what the census could establish about
// who holds it open.
func socketPathStateFor(path string, answered bool, owners []int, pub socketPublication, published bool) socketPathState {
	st := socketPathState{Path: path, Answered: answered, OwnedBy: owners}
	if published && pub.path == path {
		st.Publisher = pub.pid
		st.IdentityKnown = pub.id.known()
	}
	info, err := os.Lstat(path)
	switch {
	case err == nil:
		st.Exists = true
		// matches, not equality: a record written before the change time was
		// published compares on (device, inode) alone (see fileIdentity.matches).
		st.IdentityMatches = st.IdentityKnown && identityOf(info).matches(pub.id)
	case errors.Is(err, fs.ErrNotExist):
	default:
		st.Unreadable = true
	}
	return st
}
