package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Socket and runtime-marker hygiene (PLAN-529).
//
// A daemon that loses its runtime directory is the shape that leaked 64 daemons:
// after a kill -9 the pid, socket and version files all survive, a successor
// removes them unconditionally, and a daemon whose socket has been deleted is
// alive but unreachable — nothing dials it, so nothing complains. Two rules
// replace that:
//
//   - a runtime file is removed only while the path still names the file this
//     daemon created there (fileMarker). A file somebody else put in its place is
//     never unlinked, not at shutdown and not by the hygiene check;
//   - the socket path is re-examined for the daemon's whole life (socketGuard):
//     gone while this daemon holds the account authority — nobody else may be
//     serving this account, so it re-binds the path; a different file — never
//     removed, logged loudly, and reported (doctor and `plumb debug daemons` read
//     the identity published in the authority metadata).
//
// Both rules need one primitive: the (device, inode) pair that names a file,
// compared the way os.SameFile compares it. It is read with Lstat, deliberately:
// Stat follows a symlink, and a symlink at a runtime path is exactly a file this
// daemon must not mistake for its own.
//
// Concurrency: fileMarker values are immutable and safe to copy. socketGuard is
// safe for concurrent use; the accept loop and the ticker both reach it.

// daemonSocketCheckInterval is how often a serving daemon re-checks its socket
// paths. Sixty ticks an hour is often enough that a deleted runtime directory is
// repaired long before a client's bounded wait (30 s by default) gives up, and
// rare enough that the check costs nothing. It is a field on socketGuard so a
// test can shorten it.
const daemonSocketCheckInterval = 30 * time.Second

// fileIdentity is what names one file on this host: its device, its inode, and
// the inode's change time.
//
// The change time is the part that makes the claim honest. (Device, inode) alone
// does NOT mean "the file I created": on a filesystem that hands a freed inode
// straight back — measured on ext4, 20 of 20 immediate delete-and-recreate cycles
// at one path reused the same inode — a stranger's file created at this path
// after the daemon's own was removed carries the SAME pair, so a marker that
// compared only those two would delete a file it did not create (exactly what
// PLAN-529's D7 forbids) and would never notice a replaced socket either. The
// change time is kernel metadata that userspace cannot set and that necessarily
// differs for a file created later, so the triple is a claim about THIS file
// rather than about an inode number.
//
// An identity of zero is "unknown" and matches nothing: a marker whose identity
// could not be read must never authorise a removal.
type fileIdentity struct {
	dev uint64
	ino uint64
	// ctime is the inode's change time in nanoseconds since the epoch. Zero when
	// this platform's identityOf cannot read one (see the build-tagged files).
	ctime int64
}

// known reports whether this identity names a file at all.
func (id fileIdentity) known() bool { return id.ino != 0 }

// matches reports whether this identity — a live file — is the other one, which
// is either a marker this daemon recorded or a record it published.
//
// The change time is compared when the OTHER identity carries one. A record
// written before PLAN-529 (or by a build whose platform cannot read a change
// time) has none, and treating that as a mismatch would accuse every healthy
// daemon an older binary started; the fallback there is the weaker (device,
// inode) claim such a record can support.
func (id fileIdentity) matches(other fileIdentity) bool {
	if !id.known() || !other.known() {
		return false
	}
	if id.dev != other.dev || id.ino != other.ino {
		return false
	}
	return other.ctime == 0 || id.ctime == other.ctime
}

// sameInode reports whether this identity and the other name the same inode,
// whatever their change times say.
//
// It is the comparison for a file whose inode this process holds OPEN, and the
// open reference is what makes it sound: an inode cannot be freed and handed out
// again while a descriptor still refers to it, so a same-inode file at a bound
// socket's path is that socket, never a reincarnation of it. Measured on ext4
// with a bound unix listener: the path's inode was 13, a regular file recreated
// there got 14 and a foreign socket bound there got 14, and only after the
// listener closed did a new file get 13 back.
//
// The change time is deliberately NOT part of this claim, because it moves for
// reasons that have nothing to do with ownership: a chmod, a backup tool or any
// other metadata write bumps it on the daemon's OWN socket. Treating that as a
// replacement would make the daemon disown a socket it is serving and report a
// healthy daemon as unreachable (the reviewer's finding against the first
// version of this code). Where the inode is NOT pinned — the removal path, which
// runs after the listener is closed, and the pid and version files, which this
// process never holds open — the change time is exactly the evidence that a
// recreated file inherited the freed inode, and matches() still requires it.
func (id fileIdentity) sameInode(other fileIdentity) bool {
	return id.known() && other.known() && id.dev == other.dev && id.ino == other.ino
}

// fileMarker is one runtime path, remembered as the file this daemon created
// there. It answers the only question the exit path may ask before removing
// anything: does this path still name my file?
type fileMarker struct {
	path string
	id   fileIdentity
}

// markRuntimeFile records the file at path now, or a marker that matches nothing
// when it cannot be read.
func markRuntimeFile(path string) fileMarker {
	info, err := os.Lstat(path)
	if err != nil {
		return fileMarker{path: path}
	}
	return fileMarker{path: path, id: identityOf(info)}
}

// namesInfo reports whether info is the file this marker recorded: same device
// and inode, and the same change time (see fileIdentity.matches). It is the claim
// the REMOVAL path asks, because removing destroys something and the listener is
// already closed by then.
func (m fileMarker) namesInfo(info os.FileInfo) bool {
	return identityOf(info).matches(m.id)
}

// namesBoundSocket reports whether info is the socket this marker recorded, for
// the question the running daemon asks on every check: is the path still mine?
//
// Same device and inode, and still a socket — the change time is not required
// here, because the daemon's own listener holds that inode open for as long as
// the guard is running (sameInode). A file that genuinely replaced the socket
// has a different inode, and that is what this catches. A metadata-only change
// (chmod, a backup tool touching the path) bumps the change time on the daemon's
// own socket and must NOT read as a replacement.
func (m fileMarker) namesBoundSocket(info os.FileInfo) bool {
	return info.Mode()&os.ModeSocket != 0 && identityOf(info).sameInode(m.id)
}

// removeIfOurs removes the path only while it still names the file this daemon
// created there. A missing path is nothing to do; a different file is somebody
// else's and is left in place, loudly.
func (m fileMarker) removeIfOurs(what string) {
	info, err := os.Lstat(m.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return
	case err != nil:
		slog.Warn("daemon: cannot tell whether a runtime file is still this daemon's", "file", what, "path", m.path, "err", err)
		return
	}
	if !m.namesInfo(info) {
		// Somebody replaced it after this daemon created its own. Removing it
		// would be the exact defect the marker exists to prevent: unlinking a
		// file this daemon cannot prove it made.
		slog.Warn("daemon: leaving a runtime file in place — it is not the one this daemon created", "file", what, "path", m.path)
		return
	}
	if err := os.Remove(m.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Warn("daemon: could not remove a runtime file", "file", what, "path", m.path, "err", err)
	}
}

// bindOwnUnixSocket binds a unix socket at path for this daemon to own: an entry
// left behind by a daemon that died is cleared, a LIVE one is refused.
//
// The refusal is the point. Removing whatever happens to be at the path is how a
// socket gets taken out from under a process that is still serving on it, and the
// only evidence that separates "stale leftover" (clearing it is ordinary crash
// recovery) from "live occupant" (clearing it strands a running process) is
// whether anything answers there. So a live socket is never displaced, and a
// daemon that finds one fails to start with a message that names it.
func bindOwnUnixSocket(path string) (*net.UnixListener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating the runtime directory for %s: %w", path, err)
	}
	if err := clearStaleSocket(path); err != nil {
		return nil, err
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w%s", path, err, socketPathLengthHint(path))
	}
	// net's default is to unlink the path when the listener closes — whatever is
	// there by then, which this daemon can no longer prove it created. Its own
	// markers do the removal instead, so the unlink is switched off at bind time
	// and every removal goes through the identity check.
	ln.SetUnlinkOnClose(false)
	// Private whatever the umask: the directory's mode is the first fence, and a
	// socket left group- or world-connectable is a second way in if that fence is
	// ever weaker than it should be (PLAN-549's bridge binds in legacy
	// directories). Done before the guard marks it, so the change time recorded is
	// the final one.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("making the socket %s private: %w", path, err)
	}
	return ln, nil
}

// clearStaleSocket removes the entry at path unless something is listening on it.
// A non-socket entry (a regular file somebody dropped there, or a symlink) is
// never live and is cleared; a socket is cleared only when nothing answers.
func clearStaleSocket(path string) error {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("inspecting %s: %w", path, err)
	}
	if info.Mode()&os.ModeSocket != 0 && socketAlive(path) {
		return fmt.Errorf("refusing to remove or replace the live socket at %s: something is already serving there and it is not this daemon "+
			"(stop that process, or point %s at another directory, then start again)", path, socketPathShortenLever())
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing the stale entry at %s: %w", path, err)
	}
	return nil
}

// socketGuard owns one unix socket path for a daemon's whole life: the listener
// bound at it, the identity of the file this daemon created there, and the two
// answers a deleted or replaced path demands.
//
//	bind(path)  — the path is gone and this daemon holds the authority: re-bind
//	              it, because nobody else may be serving this account;
//	other file  — never remove it; keep serving the connections already open and
//	              say so, once, in the daemon log. The identity published in the
//	              authority metadata is what doctor and `plumb debug daemons` read
//	              to report it from outside.
//
// Concurrency: safe for concurrent use. Every swap happens under the mutex, so a
// caller racing a re-bind simply accepts on whichever listener is current when it
// looks — and Accept re-reads it after an error, which is how the accept loop
// follows a re-bind without knowing one happened.
type socketGuard struct {
	path string
	// holdsAuthority answers whether this daemon holds the account authority.
	holdsAuthority func() bool
	// bind binds the path; a field so a test can drive a failing re-bind.
	bind func(string) (*net.UnixListener, error)
	// accept is the listener's Accept. It is a field so the retry after an accept
	// error — the second place the path is examined — can be driven directly: a
	// real fd-level accept failure other than "closed" cannot be provoked on
	// demand, and a safety branch that no test can reach is a branch nobody has
	// checked.
	accept func(*net.UnixListener) (net.Conn, error)
	// publish records the identity of the socket this daemon now serves.
	// Optional: the control socket has no record anybody reads.
	publish func(path string, id fileIdentity)
	// interval is how often run re-checks the path.
	interval time.Duration

	mu      sync.Mutex
	ln      *net.UnixListener
	marker  fileMarker
	foreign bool
	closed  bool
}

// newSocketGuard binds path and returns the guard that owns it from then on.
// A failure to bind is returned to the caller, which decides whether it is fatal:
// the MCP socket is the daemon's reason to exist, the control socket is not.
func newSocketGuard(path string, holdsAuthority func() bool, publish func(string, fileIdentity)) (*socketGuard, error) {
	ln, err := bindOwnUnixSocket(path)
	if err != nil {
		return nil, err
	}
	g := &socketGuard{
		path:           path,
		holdsAuthority: holdsAuthority,
		bind:           bindOwnUnixSocket,
		accept:         func(ln *net.UnixListener) (net.Conn, error) { return ln.Accept() },
		publish:        publish,
		interval:       daemonSocketCheckInterval,
		ln:             ln,
		marker:         markRuntimeFile(path),
	}
	if publish != nil {
		publish(path, g.marker.id)
	}
	return g, nil
}

// Accept accepts the next connection on the socket this guard currently serves.
//
// An accept error is a second place the path is examined (the ticker being the
// first): a listener that failed for any reason other than this daemon closing it
// may be sitting on a path that has been deleted or replaced, and re-binding
// there means the next Accept reaches clients again. A closed listener is
// returned as-is — re-binding on shutdown would plant a socket nothing would
// accept on.
func (g *socketGuard) Accept() (net.Conn, error) {
	for {
		ln := g.listener()
		if ln == nil {
			return nil, net.ErrClosed
		}
		conn, err := g.accept(ln)
		if err == nil {
			return conn, nil
		}
		if errors.Is(err, net.ErrClosed) {
			return nil, err
		}
		if g.check() {
			continue
		}
		return nil, err
	}
}

// listener returns the listener to accept on, or nil once the guard is closed.
func (g *socketGuard) listener() *net.UnixListener {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil
	}
	return g.ln
}

// run re-checks the path on the interval until ctx is cancelled. It is the only
// thing that notices a path deleted while the daemon is idle in Accept: deleting
// the file does not disturb a listening socket, so no accept error ever comes.
func (g *socketGuard) run(ctx context.Context) {
	ticker := time.NewTicker(g.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.check()
		}
	}
}

// check re-examines the path and repairs what it may. It reports whether it
// installed a new listener, which is what Accept needs to know to retry.
//
// Whatever the path holds, the authority record is rewritten on every check —
// including while SOMEBODY ELSE'S file is at the path. The record says which
// socket this daemon bound and that the daemon is alive; freshness is how a
// reader tells a live holder from a stale record whose pid the kernel has since
// reused (daemonSocketPublicationFreshness), so a daemon that stopped refreshing
// while a replacement sat at its path would go quiet to `plumb debug daemons`
// after ~90 s and the replaced socket would stop being reported at all. The
// record is NOT a claim that the path is healthy: the comparison of the record's
// identity with the file at the path is what reports a replacement, and that is
// what this daemon publishes here.
func (g *socketGuard) check() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return false
	}
	defer g.refreshPublication()
	info, err := os.Lstat(g.path)
	switch {
	case err == nil && g.marker.namesBoundSocket(info):
		g.clearForeign()
		return false
	case err == nil && g.marker.id.known():
		g.reportForeign(info)
		return false
	case err == nil:
		// The identity of this daemon's own socket could not be read when it was
		// bound, so the path cannot be compared with anything. Never remove it,
		// and do not claim a replacement this daemon cannot prove.
		slog.Warn("daemon: cannot tell whether the socket path is still this daemon's (its identity could not be read when it was bound)", "path", g.path)
		return false
	case !errors.Is(err, fs.ErrNotExist):
		slog.Warn("daemon: cannot inspect the socket path", "path", g.path, "err", err)
		return false
	}

	// The path is gone. Only the authority holder may re-bind it: the authority
	// is the account's right to be its one daemon, so a daemon that holds it knows
	// no competitor is binding there, and a daemon that does not must not assume
	// the path is its to take.
	if !g.holdsAuthority() {
		slog.Warn("daemon: the socket path is gone and this daemon does not hold the account authority; not re-binding it", "path", g.path)
		return false
	}
	ln, err := g.bind(g.path)
	if err != nil {
		slog.Error("daemon: the socket path is gone and re-binding it failed", "path", g.path, "err", err)
		return false
	}
	old := g.ln
	g.ln = ln
	g.marker = markRuntimeFile(g.path)
	g.foreign = false
	slog.Warn("daemon: re-bound the socket after its path disappeared (deleted or replaced runtime directory)", "path", g.path)
	if old != nil {
		// The superseded listener has no path any more — its file went with the
		// runtime directory — so closing it cannot disturb the new one, and its
		// unlink-on-close is off in any case.
		_ = old.Close()
	}
	return true
}

// reportForeign says once that the path now names a file this daemon did not
// create. It is never removed: the daemon keeps serving the connections it
// already has, and a client that dials this path reaches whoever put their file
// there. Repeating the error on every tick would fill the log with a condition
// that has not changed; the transition is the event.
func (g *socketGuard) reportForeign(info os.FileInfo) {
	if g.foreign {
		return
	}
	g.foreign = true
	slog.Error("daemon: the socket path no longer names this daemon's socket — it is NOT being removed, and clients dialling this path will not reach this daemon",
		"path", g.path, "mode", info.Mode().String(),
		"this_daemon_inode", g.marker.id.ino, "path_inode", identityOf(info).ino,
		"this_daemon_ctime", g.marker.id.ctime, "path_ctime", identityOf(info).ctime)
}

// clearForeign records that the path names this daemon's own socket again, once.
func (g *socketGuard) clearForeign() {
	if !g.foreign {
		return
	}
	g.foreign = false
	slog.Warn("daemon: the socket path names this daemon's socket again", "path", g.path)
}

// refreshPublication re-writes the authority record through the held fd. Every
// check does it, whatever the path holds, so the record is never older than one
// hygiene interval while its holder lives — which is what lets a reader (doctor,
// `plumb debug daemons`) tell a live holder from a stale record whose pid the
// kernel has since recycled (see daemonSocketPublicationFreshness) and what keeps
// a replaced socket reportable for as long as the daemon is up. It rewrites the
// file the lock is on and never replaces it.
func (g *socketGuard) refreshPublication() {
	if g.publish != nil {
		g.publish(g.path, g.marker.id)
	}
}

// republish rewrites the authority record now, for a change it would otherwise
// only carry at the next check (the legacy bridge's bind, re-bind or first
// client, PLAN-549).
func (g *socketGuard) republish() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.closed {
		g.refreshPublication()
	}
}

// close stops the listener. It deliberately does not remove the socket file —
// that is removeSocketIfOurs' job, and net's unlink-on-close is switched off at
// bind time so the two can never disagree about which file is being removed.
func (g *socketGuard) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = true
	if g.ln != nil {
		_ = g.ln.Close()
	}
}

// removeSocketIfOurs removes the socket this daemon currently serves, and only
// while the path still names it. It runs after close, so the removal is the last
// word on the path either way.
func (g *socketGuard) removeSocketIfOurs(what string) {
	g.mu.Lock()
	marker := g.marker
	g.mu.Unlock()
	marker.removeIfOurs(what)
}

// daemonHoldsAuthority is the single predicate the socket hygiene asks: does this
// daemon hold the ACCOUNT authority — the account's right to have exactly this
// daemon, and therefore the right to re-bind the account's socket paths?
//
// It is a predicate rather than a known lock path so the hygiene does not care
// which lock is the authority: PLAN-527's account lock answers yes here, and a
// pre-527 daemon whose authority was its runtime lifetime lock would answer the
// same question the same way. Every daemon that got as far as serving holds both
// locks — daemonLaunchLocks returns both or an error — so this is true for any
// daemon that could be running.
func daemonHoldsAuthority(locks daemonLocks) bool { return locks.Authority != nil }
