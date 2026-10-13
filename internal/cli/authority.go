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
	"github.com/plumbkit/plumb/internal/paths"
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
// through the held fd: who it is, and the MCP socket it is about to serve. A
// launcher that finds the authority held reads that record and dials the socket,
// which is how a client whose HOME or XDG resolves elsewhere reaches the one
// daemon instead of failing.
//
// A failure is a warning, not fatal. The record is what lets a differently
// rooted client attach; the daemon still serves every client whose environment
// resolves as its own does, which is what it did before there was a record.
func publishAuthorityMetadata(locks daemonLocks, socketPath string) {
	if locks.Authority == nil {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		exe = "" // best effort: the record still names the pid and the socket
	}
	if err := publishAuthority(locks.Authority, authorityMetadata{
		PID:        os.Getpid(),
		Version:    Version,
		Executable: exe,
		Socket:     socketPath,
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
// A recorded socket is used only when every part of it checks out: the recorded
// pid must be alive, the path must be a unix socket this user owns, and it must
// sit under a runtime root plumb could have chosen. A stale record from a daemon
// that has since died, or a path pointing anywhere else, falls through to the
// caller's own sockets — never dialled, because metadata is a record of where a
// daemon said it was, not an instruction.
func attachSocket(authorityPath string) (string, bool) {
	m, ok := readAuthority(authorityPath)
	if !ok || m.Socket == "" {
		return "", false
	}
	if !processAlive(m.PID) {
		return "", false
	}
	if !underKnownRuntimeRoot(m.Socket) {
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

// underKnownRuntimeRoot reports whether path sits under a directory plumb could
// have chosen for a runtime socket: the account's canonical one, or the
// environment-derived one (which a legacy or spoofed-HOME daemon may be using).
// It is a sanity check on metadata, not an authorisation decision — the socket's
// ownership and liveness are what make the dial safe.
func underKnownRuntimeRoot(path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	dir := filepath.Dir(path)
	for _, root := range []string{paths.RuntimeDir(), accountRuntimeDirOrEmpty()} {
		if root == "" {
			continue
		}
		if dir == root || dir == paths.Canonical(root) {
			return true
		}
	}
	return false
}

// accountRuntimeDirOrEmpty is the ACCOUNT's runtime directory — a second place a
// daemon this user owns may have put its socket — or "" when the account cannot
// be resolved.
//
// It resolves nothing itself: the caller canonicalises what it returns (see
// underKnownRuntimeRoot), which is why the name says account, not canonical.
func accountRuntimeDirOrEmpty() string {
	u, err := account.Current()
	if err != nil {
		return ""
	}
	return u.RuntimeDir()
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
