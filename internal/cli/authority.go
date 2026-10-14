package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/plumbkit/plumb/internal/account"
)

// The account authority (PLAN-527).
//
// #649 made the daemon's lifetime lock the right to launch one: serve takes it
// before starting a child and hands it over, so a launcher that finds it held
// never starts a second daemon. That protocol was scoped to the runtime
// directory, and the runtime directory came from $HOME — so every HOME override
// was still a legal second singleton, which is how 64 daemons leaked.
//
// The account authority moves that lock out of the environment's reach:
//
//   - it lives under the ACCOUNT's data directory, which is not in the runtime
//     subtree and cannot be moved by a client's HOME or XDG;
//   - every managed launch takes it BEFORE cmd.Start, in every mode, so a client
//     whose environment resolves elsewhere still cannot start a daemon beside
//     the one that holds it;
//   - the daemon holds it for its whole life and writes its own metadata through
//     the held fd, so a launcher that finds it held can attach to the daemon that
//     holds it instead of failing;
//   - the runtime lifetime lock stays exactly where it was, so an old binary's
//     launcher still loses to the same file it always did.
//
// Two locks, always, in one order: authority first, then the runtime lifetime
// lock. Anything that takes them in the other order could deadlock against a
// launcher that is mid-handover.

// authorityMetadata is what the holder publishes through the held fd. It is
// written, never renamed into place: replacing the lock file's inode would let
// a second daemon lock a different inode at the same path and believe it holds
// the authority.
type authorityMetadata struct {
	PID int `json:"pid"`
	// Published is when this holder last wrote its metadata — within a second or
	// two of its start. Together with a liveness check it is how a reader tells a
	// live holder from a pid the kernel has since recycled.
	Published time.Time `json:"published"`
	// Version is the daemon's build version.
	Version string `json:"version"`
	// Executable is the binary the holder is running.
	Executable string `json:"executable"`
	// Socket is the MCP socket the holder is listening on. A launcher that finds
	// the authority held dials THIS path, whatever its own environment resolved,
	// which is how a client under a different HOME still reaches the one daemon.
	Socket string `json:"socket"`
	// SocketDev and SocketIno identify the SOCKET FILE the holder bound at
	// Socket: the device and inode os.Stat reports for it (PLAN-529). A reader
	// compares them with the file at Socket, so a path that was deleted or
	// replaced under a live daemon is reported as exactly that instead of reading
	// as healthy — which is how `plumb debug daemons` and `plumb doctor` see the
	// defect, without dialling anything or taking a lock. Zero when the holder
	// could not read them, and zero in records written by an older daemon.
	SocketDev uint64 `json:"socket_dev,omitempty"`
	SocketIno uint64 `json:"socket_ino,omitempty"`
	// SocketCtime is the socket inode's change time, in nanoseconds since the
	// epoch — the kernel metadata that tells the socket this holder created from a
	// different file that later inherited the same (device, inode) on a
	// filesystem that reuses freed inodes (PLAN-529). Absent, not zero, in a
	// record written before that field existed; a reader then compares device and
	// inode alone, which is all such a record can support.
	SocketCtime int64 `json:"socket_ctime,omitempty"`
}

// authorityPath returns the account authority lock path.
//
// It fails when the account cannot be resolved, and every caller must fail
// closed on that: a substituted path would be a second authority, which is the
// defect this exists to remove.
func authorityPath() (string, error) {
	u, err := account.Current()
	if err != nil {
		return "", fmt.Errorf("resolving the account for the authority lock: %w", err)
	}
	return u.AuthorityLockPath(), nil
}

// acquireAuthority takes the account authority lock without blocking, creating
// the file (and its directory) when it does not exist yet. A held lock is
// errDaemonAlreadyRunning, so callers treat "somebody holds the authority" and
// "another daemon is running" as the same event, which is what they are.
func acquireAuthority() (*os.File, error) {
	path, err := authorityPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating the authority lock directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening the authority lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errDaemonAlreadyRunning
		}
		return nil, fmt.Errorf("acquiring the authority lock: %w", err)
	}
	return f, nil
}

// authorityHeld reports whether somebody currently holds the account authority,
// without keeping it: the probe takes the lock and releases it at once.
//
// It is what tells a RECORD from a HOLDER. The authority file is never unlinked,
// so it still carries the last holder's metadata after that holder is gone; only
// the lock says whether anybody is there now. Taking it for the length of this
// call is deliberate: the alternative is trusting a record whose pid the kernel
// has since handed to somebody else, and `plumb restart` SIGTERMs and SIGKILLs
// what it is told to stop.
func authorityHeld() bool {
	f, err := acquireAuthority()
	if err != nil {
		return errors.Is(err, errDaemonAlreadyRunning)
	}
	_ = f.Close()
	return false
}

// publishAuthority writes the holder's metadata through the held fd, then leaves
// the offset at the start so a later writer overwrites rather than appends.
func publishAuthority(f *os.File, m authorityMetadata) error {
	if m.Published.IsZero() {
		m.Published = time.Now()
	}
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encoding the authority metadata: %w", err)
	}
	data = append(data, '\n')
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("truncating the authority lock: %w", err)
	}
	if _, err := f.WriteAt(data, 0); err != nil {
		return fmt.Errorf("writing the authority metadata: %w", err)
	}
	return nil
}

// publishAuthorityMetadata records this daemon as the authority's holder,
// through the held fd: who it is, the MCP socket it is about to serve, and the
// identity of that socket file. A launcher that finds the authority held reads
// that record and dials the socket, which is how a client whose HOME or XDG
// resolves elsewhere reaches the one daemon instead of failing; a reader that is
// only diagnosing compares the identity with what is at the path (PLAN-529).
//
// The daemon calls it again when its socket guard re-binds the path, so the
// record always names the socket the holder serves now — and never a file that
// replaced it, which the holder deliberately leaves alone.
//
// A failure is a warning, not fatal. The record is what lets a differently
// rooted client attach; the daemon still serves every client whose environment
// resolves as its own does, which is what it did before there was a record.
func publishAuthorityMetadata(locks daemonLocks, socketPath string, socketID fileIdentity) {
	if locks.Authority == nil {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		exe = "" // best effort: the record still names the pid and the socket
	}
	if err := publishAuthority(locks.Authority, authorityMetadata{
		PID:         os.Getpid(),
		Version:     Version,
		Executable:  exe,
		Socket:      socketPath,
		SocketDev:   socketID.dev,
		SocketIno:   socketID.ino,
		SocketCtime: socketID.ctime,
	}); err != nil {
		slog.Warn("daemon: could not publish the authority metadata", "err", err)
	}
}

// readAuthority reads the holder's metadata, or reports that there is none. A
// missing or unreadable file is not an error: it means this daemon has not
// published yet (or a lock file exists with nobody holding it), and a launcher
// then falls back to its bounded redial.
func readAuthority(path string) (authorityMetadata, bool) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the path is the account's own authority lock
	if err != nil {
		return authorityMetadata{}, false
	}
	var m authorityMetadata
	if err := json.Unmarshal(data, &m); err != nil || m.PID <= 0 {
		return authorityMetadata{}, false
	}
	return m, true
}

// attachSocket returns the socket a launcher should dial to reach the current
// holder of the authority, and whether it could be trusted.
//
// The LOCK is the trust anchor, not the socket's directory: only a process this
// user runs can take the account authority and write its record through the held
// fd, so the checks below are the ones that make the dial safe — the recorded pid
// is alive, the recorded path is absolute, that path IS a unix socket, and this
// user OWNS it. A stale record from a daemon that has since died, a path that is
// not a socket, or a socket somebody else bound, falls through to the caller's
// own sockets — never dialled, because metadata is a record of where a daemon
// said it was, not an instruction.
//
// The record used to have to sit under a runtime root plumb could have chosen
// (this process's own, or the account's). That was a sanity gate and not the
// safety property, and it stranded real clients: with the account's directory
// gate off — a release build, where account.AuthorityEnabled is false — a daemon
// launched under a HOME-derived runtime root is under neither, so a client whose
// HOME differed from the daemon's found the authority held, refused the only
// socket that could have reached it, waited out its bounded wait and exited with
// no daemon at all. The gate bought nothing either: a same-user process that can
// bind a socket in a plumb-shaped root can equally bind the path this client
// dials first (l.dial), which has never been checked, and a socket under a root
// this user does not own fails the ownership check below whatever directory it
// is in.
func attachSocket(authorityPath string) (string, bool) {
	m, ok := readAuthority(authorityPath)
	if !ok || m.Socket == "" {
		return "", false
	}
	if !processAlive(m.PID) {
		return "", false
	}
	// Absolute because a record is a path, not a fragment: a relative one would
	// resolve against this client's working directory, which the daemon that
	// recorded it knows nothing about. It cannot strand a real client — every
	// socket plumb records comes from a runtime directory, which is absolute.
	if !filepath.IsAbs(m.Socket) {
		return "", false
	}
	info, err := os.Stat(m.Socket)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return "", false
	}
	if !ownedByCurrentUser(info) {
		return "", false
	}
	return m.Socket, true
}

// dialAuthoritySocket dials the recorded socket, if there is one worth dialling.
func dialAuthoritySocket(authorityPath string) (net.Conn, bool) {
	socket, ok := attachSocket(authorityPath)
	if !ok {
		return nil, false
	}
	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		return nil, false
	}
	return conn, true
}
