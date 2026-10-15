package cli

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/plumbkit/plumb/internal/account"
)

// The legacy bridge (PLAN-549): the canonical daemon also answers on the MCP
// socket of every legacy runtime location whose lifetime lock it holds.
//
// The case it exists for is an OLD `plumb serve` proxy — a binary from before the
// account authority (PLAN-527) — whose own daemon is gone: a long-lived IDE or
// terminal session whose client has not restarted. That proxy only knows its
// legacy socket path. Without the bridge it can neither reach the account's
// daemon nor start one (PLAN-530's fence: the canonical daemon holds the legacy
// lifetime lock, so an old launcher cannot win it), and it stays without plumb
// until somebody restarts it.
//
// Holding the legacy lifetime lock is the ownership proof. No other daemon can
// serve a location whose lock this daemon holds, so binding its socket takes
// nothing from anybody — and the bind still refuses a LIVE socket that is not
// ours (bindOwnUnixSocket, PLAN-529): a path somebody serves is reported, never
// displaced. Each bridge socket is owned by a socketGuard exactly like the
// canonical one, so it is recorded and removed by identity, re-bound if its path
// is deleted, and never removed once somebody else's file replaces it.
//
// The MCP socket only — never the legacy CONTROL socket. The control socket is
// the daemon's control plane, and serving it at a legacy path would let an old
// CLI command a new daemon: a version-skew pairing that buys nothing (the CLI
// ships with the daemon) while widening what an out-of-date binary can do to the
// account's one daemon.
//
// It is a bridge with a sunset, not a second front door: the first client through
// each path is logged with the remedy (restart the client, which then uses the
// account socket), the paths and their first use are published in the authority
// record so `plumb debug daemons` shows whether any client still needs them, and
// docs/architecture.md says what retires it.
//
// Concurrency: safe for concurrent use. The socket set is fixed by bind, before
// any accept loop starts; first-use bookkeeping is under mu.
type legacyBridge struct {
	mu      sync.Mutex
	sockets []*bridgeSocket
	// republish rewrites the authority record now, so a bind, a re-bind or a
	// first client is visible to readers at once rather than at the account
	// guard's next tick. It is called without mu held.
	republish func()
	// loops tracks the accept loops, so close can wait for them to stop before
	// the daemon's drain begins.
	loops sync.WaitGroup
}

// bridgeSocket is one legacy MCP socket path this daemon answers on.
type bridgeSocket struct {
	path  string
	guard *socketGuard
	// id is the identity of the socket file the guard currently serves; the guard
	// reports it on every check, so a re-bind is reflected.
	id fileIdentity
	// firstClient is when the first client connected through this path, zero
	// until one does.
	firstClient time.Time
}

// bridgeRecord is a bridge socket as the authority record publishes it: the path,
// the identity of the file this daemon bound there (so a reader can tell OUR
// socket from a foreign legacy daemon's), and the first time an old client used it.
type bridgeRecord struct {
	Path        string    `json:"path"`
	Dev         uint64    `json:"dev,omitempty"`
	Ino         uint64    `json:"ino,omitempty"`
	Ctime       int64     `json:"ctime,omitempty"`
	FirstClient time.Time `json:"first_client,omitzero"`
}

func newLegacyBridge() *legacyBridge { return &legacyBridge{} }

// bind binds the MCP socket of every legacy location this daemon holds the
// lifetime lock of, then publishes them. A path it cannot bind — a live listener
// that is not ours, an unwritable directory, a path too long for a unix socket —
// is reported and skipped: the bridge is a convenience for old clients, and the
// canonical socket is what the daemon exists to serve.
//
// A legacy location is derived from the environment of the client that started
// this daemon, so it is held to the XDG spec's own terms before anything listens
// there (the check the canonical runtime base gets): a directory this uid owns,
// mode 0700. The account's daemon serves its whole MCP surface on a bridge
// socket, and a directory somebody else can write is one where they could swap
// that socket for their own.
func (b *legacyBridge) bind(ctx context.Context, held []heldLegacyLock, canonical string, republish func()) {
	b.republish = republish
	for _, loc := range legacyLocationsOf(held) {
		if loc.Socket == canonical || b.has(loc.Socket) {
			continue
		}
		if err := os.MkdirAll(loc.Dir, 0o700); err != nil || !account.UsableRuntimeBase(loc.Dir, os.Getuid()) {
			slog.Warn("daemon: not answering old clients at a legacy socket path: its directory is not a 0700 directory this user owns",
				"path", loc.Socket, "dir", loc.Dir, "err", err)
			continue
		}
		s := &bridgeSocket{path: loc.Socket}
		g, err := newSocketGuard(loc.Socket,
			func() bool { return true }, // the legacy lifetime lock is held for the daemon's whole life
			func(_ string, id fileIdentity) { b.setID(s, id) })
		if err != nil {
			slog.Warn("daemon: not answering old clients at a legacy socket path", "path", loc.Socket, "err", err)
			continue
		}
		s.guard = g
		b.mu.Lock()
		b.sockets = append(b.sockets, s)
		b.mu.Unlock()
		go g.run(ctx)
		slog.Info("daemon: answering old clients at a legacy socket path (the legacy bridge)", "path", loc.Socket)
	}
	b.publish()
}

func (b *legacyBridge) has(path string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.ContainsFunc(b.sockets, func(s *bridgeSocket) bool { return s.path == path })
}

// setID records the identity a bridge guard reports — at bind and on every
// check — and republishes when it changed (a re-bind), so readers never wait a
// tick to recognise the new socket. It runs under the bridge guard's own lock,
// so the republish is handed to a goroutine rather than taken in line.
func (b *legacyBridge) setID(s *bridgeSocket, id fileIdentity) {
	b.mu.Lock()
	changed := s.id != id
	s.id = id
	bound := s.guard != nil
	b.mu.Unlock()
	if changed && bound {
		go b.publish()
	}
}

// publish asks for the authority record to be rewritten now.
func (b *legacyBridge) publish() {
	if b.republish != nil {
		b.republish()
	}
}

// serve runs an accept loop per bridge socket. Each connection goes to handle on
// wg, the daemon's own connection group, so shutdown drains bridge clients with
// every other. A loop ends when its guard is closed.
func (b *legacyBridge) serve(wg *sync.WaitGroup, handle func(net.Conn)) {
	b.mu.Lock()
	sockets := slices.Clone(b.sockets)
	b.mu.Unlock()
	for _, s := range sockets {
		b.loops.Go(func() {
			for {
				conn, err := s.guard.Accept()
				if err != nil {
					// Only a CLOSED guard ends the loop. A re-bind closes the old
					// listener too, and that Accept error must be followed onto the
					// new listener, not read as shutdown.
					if s.guard.listener() == nil {
						return
					}
					if !errors.Is(err, net.ErrClosed) {
						slog.Error("daemon: accept on a legacy bridge socket", "path", s.path, "err", err)
					}
					continue
				}
				b.noteClient(s)
				wg.Go(func() { handle(conn) })
			}
		})
	}
}

// noteClient records, once per path, that an old client is still using it — the
// signal the bridge's sunset is decided on — and says what retires the need.
func (b *legacyBridge) noteClient(s *bridgeSocket) {
	b.mu.Lock()
	first := s.firstClient.IsZero()
	if first {
		s.firstClient = time.Now()
	}
	b.mu.Unlock()
	if first {
		b.publish()
		slog.Warn("daemon: a client connected through a legacy socket path; it is served, but restart that client so it uses the account socket",
			"path", s.path, "account_socket", daemonSocketPath())
	}
}

// records is the bridge as the authority record publishes it.
func (b *legacyBridge) records() []bridgeRecord {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]bridgeRecord, 0, len(b.sockets))
	for _, s := range b.sockets {
		out = append(out, bridgeRecord{Path: s.path, Dev: s.id.dev, Ino: s.id.ino, Ctime: s.id.ctime, FirstClient: s.firstClient})
	}
	return out
}

// close stops every bridge listener, waits for the accept loops to return — so
// none can add a connection once the daemon's drain has begun — and removes each
// socket file that is still the one this daemon created. Safe to call twice.
func (b *legacyBridge) close() {
	b.mu.Lock()
	sockets := slices.Clone(b.sockets)
	b.mu.Unlock()
	for _, s := range sockets {
		s.guard.close()
	}
	b.loops.Wait()
	for _, s := range sockets {
		s.guard.removeSocketIfOurs("legacy bridge socket")
	}
}

// ourBridgeAt reports whether the socket at path is a legacy bridge socket of the
// account's LIVE daemon: the authority record names path as a bridge, the file
// there is the one that daemon published (device, inode and change time — the
// reader rule of fileIdentity.matches), and the publisher is alive and has
// refreshed the record within the freshness window. It takes no lock and dials nothing, so the read-only probes (the legacy
// probe, doctor, the census) can use it to keep from mistaking the account's own
// daemon, answering old clients, for a daemon an older binary left running.
func ourBridgeAt(path string) bool {
	authority, err := authorityPath()
	if err != nil {
		return false
	}
	m, ok := readAuthority(authority)
	if !ok || m.PID <= 0 || !processAlive(m.PID) || time.Since(m.Published) > daemonSocketPublicationFreshness {
		return false
	}
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	at := identityOf(info)
	return slices.ContainsFunc(m.Bridges, func(r bridgeRecord) bool {
		return r.Path == path && r.Ino != 0 && at.matches(fileIdentity{dev: r.Dev, ino: r.Ino, ctime: r.Ctime})
	})
}
