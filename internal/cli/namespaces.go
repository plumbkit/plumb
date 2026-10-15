package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
	"github.com/plumbkit/plumb/internal/daemons"
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/sessionstate"
)

// namespaceRegistry is the daemon's side of PLAN-520: private slices of the one
// daemon's per-connection state, each owned by whoever holds its token, so a
// local end-to-end run is isolated without a daemon of its own (the owner's
// decision E2).
//
// Lifetime. A namespace lives for its TTL, renewed by every namespaced call and
// by `plumb namespace renew`. Release or expiry REVOKES it at once — its token
// is refused from that moment, with no fallback to the default namespace —
// then its live connections get namespaceDrain to finish, are cancelled, and
// its rows are purged. An owner whose liveness hint (pid plus start time) stops
// matching a live process expires the namespace after namespaceOwnerGrace,
// which covers a runner killed before its exit trap could release.
//
// State lives in session_state.db, so namespaces and their members survive a
// daemon restart; restore purges what was revoked or expired meanwhile.
//
// Concurrency: all methods are safe for concurrent use. mu guards the
// in-memory maps only; the store serialises its own writes.
type namespaceRegistry struct {
	store *sessionstate.Store
	conns *connRegistry

	now        func() time.Time
	ownerStart func(pid int) (time.Time, bool)
	drain      time.Duration
	ownerGrace time.Duration

	mu sync.Mutex
	// renewedAt throttles renewal writes: a namespaced call renews in memory and
	// persists at most once per namespaceRenewEvery (or quarter TTL). ttls caches
	// each namespace's TTL, so the throttle is decided without reading the store
	// on every call.
	renewedAt map[string]time.Time
	ttls      map[string]time.Duration
	// draining maps a revoked namespace to the moment its connections are
	// cancelled. cancelledAt records when that first happened: the purge never
	// runs in the same tick as the first cancel, and runs once no connection is
	// left or namespaceForcePurge after that first cancel, whichever is first.
	draining    map[string]time.Time
	cancelledAt map[string]time.Time
	// purgeExtra removes what lives outside session_state.db (session files are
	// handled here; collab and stats rows join in PLAN-520's second slice).
	purgeExtra []func(id string, sessionIDs []string)
}

const (
	namespaceDefaultTTL = 2 * time.Hour
	namespaceMaxTTL     = 7 * 24 * time.Hour
	namespaceDrain      = 30 * time.Second
	namespaceOwnerGrace = 5 * time.Minute
	namespaceRenewEvery = time.Minute
	namespaceTick       = 30 * time.Second
	// namespaceForcePurge bounds how long a purge waits for cancelled
	// connections to finish tearing down before it runs anyway.
	namespaceForcePurge = 10 * time.Second
)

func newNamespaceRegistry(store *sessionstate.Store, conns *connRegistry) *namespaceRegistry {
	return &namespaceRegistry{
		store:       store,
		conns:       conns,
		now:         time.Now,
		ownerStart:  daemons.ProcessStart,
		drain:       namespaceDrain,
		ownerGrace:  namespaceOwnerGrace,
		renewedAt:   make(map[string]time.Time),
		ttls:        make(map[string]time.Duration),
		draining:    make(map[string]time.Time),
		cancelledAt: make(map[string]time.Time),
	}
}

// errNoNamespaceStore is returned when the daemon has no session-state store,
// which namespaces live in.
var errNoNamespaceStore = errors.New("namespaces are unavailable: the daemon has no session-state store")

// create makes a namespace. ownerPID is the liveness hint (0 for none); a pid
// that names no live process is refused, since the namespace would expire at
// once.
func (r *namespaceRegistry) create(ttl time.Duration, ownerPID int) (token string, ns sessionstate.Namespace, err error) {
	if r == nil || r.store == nil {
		return "", ns, errNoNamespaceStore
	}
	switch {
	case ttl == 0:
		ttl = namespaceDefaultTTL
	case ttl < time.Minute || ttl > namespaceMaxTTL:
		return "", ns, fmt.Errorf("namespace TTL %s is out of range (1m to %s)", ttl, namespaceMaxTTL)
	}
	var ownerStart int64
	if ownerPID > 0 {
		start, ok := r.ownerStart(ownerPID)
		if !ok {
			return "", ns, fmt.Errorf("owner process %d is not running", ownerPID)
		}
		ownerStart = start.UnixMilli()
	}
	token, id, err := sessionstate.NewNamespaceToken()
	if err != nil {
		return "", ns, err
	}
	now := r.now()
	ns = sessionstate.Namespace{
		ID: id, TokenHash: sessionstate.HashNamespaceToken(token), TTL: ttl,
		CreatedAt: now, ExpiresAt: now.Add(ttl), OwnerPID: ownerPID, OwnerStart: ownerStart,
		State: sessionstate.NamespaceActive,
	}
	if err := r.store.CreateNamespace(ns); err != nil {
		return "", ns, err
	}
	slog.Info("namespace: created", "namespace", id, "ttl", ttl, "owner_pid", ownerPID)
	return token, ns, nil
}

// resolve returns the active namespace a token belongs to, or an error that
// says why it is refused. It never answers with the default namespace.
func (r *namespaceRegistry) resolve(token string) (sessionstate.Namespace, error) {
	if r == nil || r.store == nil {
		return sessionstate.Namespace{}, errNoNamespaceStore
	}
	ns, err := r.store.NamespaceByToken(token)
	if errors.Is(err, sessionstate.ErrNamespaceUnknown) {
		return ns, errors.New("namespace token refused: no such namespace (it was released, expired and purged, or never created); a namespaced connection never falls back to the default namespace")
	}
	if err != nil {
		return ns, err
	}
	if ns.State != sessionstate.NamespaceActive {
		return ns, fmt.Errorf("namespace %s is revoked; its token is no longer accepted", ns.ID)
	}
	if !r.now().Before(ns.ExpiresAt) {
		r.revoke(ns.ID, "expired")
		return ns, fmt.Errorf("namespace %s expired; its token is no longer accepted", ns.ID)
	}
	return ns, nil
}

// gate is the connection's namespace gate (mcp.Server.OnNamespace): it binds
// the connection to the token's namespace and records the proxy session as a
// member, or refuses. A connection without a token is refused only when its
// proxy session already belongs to a namespace, so a namespaced proxy cannot
// shed its token and land in the default namespace.
func (r *namespaceRegistry) gate(token, proxySessionID string) (string, error) {
	if token == "" {
		if r == nil || r.store == nil || proxySessionID == "" {
			return "", nil
		}
		member, err := r.store.NamespaceOf(proxySessionID)
		if err != nil {
			return "", err
		}
		if member != "" {
			return "", fmt.Errorf("this proxy session belongs to namespace %s; reconnect with its token", member)
		}
		return "", nil
	}
	if proxySessionID == "" {
		return "", errors.New("a namespaced connection needs a proxy session ID; connect through `plumb serve --namespace`")
	}
	ns, err := r.resolve(token)
	if err != nil {
		return "", err
	}
	member, err := r.store.NamespaceOf(proxySessionID)
	if err != nil {
		return "", err
	}
	if member == "" {
		// A proxy session the default namespace already knows cannot be taken
		// into a namespace: its rows would be purged with it.
		if _, known, err := r.store.LoadIdentity(proxySessionID); err != nil || known {
			return "", errors.New("this proxy session already belongs to the default namespace; start a new serve with --namespace")
		}
	}
	if err := r.store.AddNamespaceMember(ns.ID, proxySessionID); err != nil {
		return "", err
	}
	r.touch(ns.ID)
	return ns.ID, nil
}

// touch renews a namespace on activity: in memory every call, in the store at
// most once per namespaceRenewEvery.
func (r *namespaceRegistry) touch(id string) {
	if r == nil || r.store == nil || id == "" {
		return
	}
	now := r.now()
	r.mu.Lock()
	ttl, cached := r.ttls[id]
	r.mu.Unlock()
	if !cached {
		ns, err := r.store.NamespaceByID(id)
		if err != nil || ns.State != sessionstate.NamespaceActive {
			return
		}
		ttl = ns.TTL
		r.mu.Lock()
		r.ttls[id] = ttl
		r.mu.Unlock()
	}
	// Persist at most once per namespaceRenewEvery — or per quarter TTL for a
	// short one, so a namespace in continuous use never reaches its expiry
	// between two persisted renewals. The write itself only moves an ACTIVE
	// namespace's expiry, so a revoked one is never revived.
	every := min(namespaceRenewEvery, ttl/4)
	r.mu.Lock()
	if now.Sub(r.renewedAt[id]) < every {
		r.mu.Unlock()
		return
	}
	r.renewedAt[id] = now
	r.mu.Unlock()
	if err := r.store.SetNamespaceExpiry(id, now.Add(ttl)); err != nil {
		slog.Warn("namespace: renew", "namespace", id, "err", err)
	}
}

// renew extends the namespace a token belongs to by its TTL.
func (r *namespaceRegistry) renew(token string) (sessionstate.Namespace, error) {
	ns, err := r.resolve(token)
	if err != nil {
		return ns, err
	}
	ns.ExpiresAt = r.now().Add(ns.TTL)
	return ns, r.store.SetNamespaceExpiry(ns.ID, ns.ExpiresAt)
}

// release revokes the namespace a token belongs to and starts its drain. A
// namespace already revoked is released again harmlessly.
func (r *namespaceRegistry) release(token string) (sessionstate.Namespace, error) {
	if r == nil || r.store == nil {
		return sessionstate.Namespace{}, errNoNamespaceStore
	}
	ns, err := r.store.NamespaceByToken(token)
	if err != nil {
		return ns, err
	}
	r.revoke(ns.ID, "released")
	return r.store.NamespaceByID(ns.ID)
}

// revoke marks a namespace revoked — its token is refused from now on — and
// schedules the cancellation of its connections after the drain.
func (r *namespaceRegistry) revoke(id, why string) {
	if err := r.store.RevokeNamespace(id); err != nil {
		slog.Warn("namespace: revoke", "namespace", id, "err", err)
		return
	}
	r.mu.Lock()
	if _, ok := r.draining[id]; !ok {
		r.draining[id] = r.now().Add(r.drain)
	}
	r.mu.Unlock()
	slog.Info("namespace: revoked", "namespace", id, "why", why, "drain", r.drain)
}

// list returns every namespace the store holds.
func (r *namespaceRegistry) list() ([]sessionstate.Namespace, error) {
	if r == nil || r.store == nil {
		return nil, errNoNamespaceStore
	}
	return r.store.ListNamespaces()
}

// restore runs once at daemon start: what was revoked, or expired while the
// daemon was down, has no live connection left, so it is purged at once.
func (r *namespaceRegistry) restore() {
	all, err := r.list()
	if err != nil {
		return
	}
	now := r.now()
	for _, ns := range all {
		if ns.State != sessionstate.NamespaceActive || !now.Before(ns.ExpiresAt) {
			r.purge(ns.ID)
		}
	}
}

// tick is the janitor: expire what is past its time or has lost its owner,
// cancel the connections of namespaces whose drain is over, and purge those
// whose connections are gone.
func (r *namespaceRegistry) tick() {
	all, err := r.list()
	if err != nil {
		return
	}
	now := r.now()
	for _, ns := range all {
		if ns.State == sessionstate.NamespaceActive {
			r.checkActive(ns, now)
		}
	}
	r.mu.Lock()
	var due []string
	for id, at := range r.draining {
		if !now.Before(at) {
			due = append(due, id)
		}
	}
	r.mu.Unlock()
	for _, id := range due {
		r.drainDue(id, now)
	}
}

// drainDue handles a namespace whose drain is over: cancel its connections and
// purge it on a LATER tick, once none is left (or namespaceForcePurge after the
// first cancel), so a connection still tearing down cannot write rows after
// the purge that would then count as the default namespace's.
func (r *namespaceRegistry) drainDue(id string, now time.Time) {
	live := r.conns.cancelNamespace(id)
	r.mu.Lock()
	first, cancelled := r.cancelledAt[id]
	if !cancelled {
		r.cancelledAt[id] = now
	}
	r.mu.Unlock()
	if !cancelled {
		return // cancelled just now: purge on a later tick
	}
	if live > 0 && now.Before(first.Add(namespaceForcePurge)) {
		return
	}
	r.purge(id)
}

// ownerStartTolerance absorbs a start time read slightly differently later:
// Linux derives it from btime, which a clock adjustment can nudge. A recycled
// pid starts far later than that.
const ownerStartTolerance = 2 * time.Second

func ownerStartMatches(gotMs, wantMs int64) bool {
	d := time.Duration(gotMs-wantMs) * time.Millisecond
	return d > -ownerStartTolerance && d < ownerStartTolerance
}

// checkActive expires one active namespace when its time is up or its owner
// has been gone for the grace.
func (r *namespaceRegistry) checkActive(ns sessionstate.Namespace, now time.Time) {
	if !now.Before(ns.ExpiresAt) {
		r.revoke(ns.ID, "expired")
		return
	}
	if ns.OwnerPID <= 0 {
		return
	}
	start, ok := r.ownerStart(ns.OwnerPID)
	if ok && ownerStartMatches(start.UnixMilli(), ns.OwnerStart) {
		if !ns.OwnerMismatchAt.IsZero() {
			_ = r.store.SetOwnerMismatch(ns.ID, time.Time{})
		}
		return
	}
	if ns.OwnerMismatchAt.IsZero() {
		_ = r.store.SetOwnerMismatch(ns.ID, now)
		return
	}
	if now.Sub(ns.OwnerMismatchAt) >= r.ownerGrace {
		r.revoke(ns.ID, "owner gone")
	}
}

// purge removes every trace of a namespace: its rows in session_state.db, the
// session files of its members, and whatever purgeExtra covers.
func (r *namespaceRegistry) purge(id string) {
	sessionIDs, err := r.store.NamespaceSessionIDs(id)
	if err != nil {
		slog.Warn("namespace: purge (session IDs)", "namespace", id, "err", err)
		return
	}
	r.mu.Lock()
	extra := r.purgeExtra
	r.mu.Unlock()
	for _, fn := range extra {
		fn(id, sessionIDs)
	}
	if _, err := r.store.PurgeNamespace(id); err != nil {
		slog.Warn("namespace: purge", "namespace", id, "err", err)
		return
	}
	for _, sid := range sessionIDs {
		session.Remove(sid)
	}
	// Also every session row stamped with the namespace, including one whose
	// identity record was never written (a connection refused or cut short).
	if all, err := session.List(); err == nil {
		for _, info := range session.InNamespace(all, id) {
			session.Remove(info.ID)
		}
	}
	r.mu.Lock()
	delete(r.draining, id)
	delete(r.cancelledAt, id)
	delete(r.renewedAt, id)
	delete(r.ttls, id)
	r.mu.Unlock()
	slog.Info("namespace: purged", "namespace", id, "sessions", len(sessionIDs))
}

// startNamespaces gives the daemon's connection registry its namespace registry
// and starts the janitor. With no session-state store there is none, and every
// namespace token is refused rather than silently ignored.
func startNamespaces(ctx context.Context, store *sessionstate.Store, conns *connRegistry, stats *statsStore, collabs *collabPool) {
	if store == nil {
		return
	}
	r := newNamespaceRegistry(store, conns)
	r.purgeExtra = append(r.purgeExtra,
		func(id string, _ []string) { purgeNamespaceMail(ctx, store, collabs, id) },
		func(id string, _ []string) { stats.PurgeNamespace(id) })
	conns.namespaces = r
	go r.run(ctx)
}

// purgeNamespaceMail deletes a namespace's mail and intents from every collab
// store it wrote to: the roots its members recorded, and the daemon-level store.
// A root whose collab.db is gone has nothing left to delete.
func purgeNamespaceMail(ctx context.Context, store *sessionstate.Store, collabs *collabPool, id string) {
	if collabs == nil {
		return
	}
	roots, err := store.NamespaceRoots(id)
	if err != nil {
		slog.Warn("namespace: purge (roots)", "namespace", id, "err", err)
	}
	stores := []*collab.Store{collabs.getGlobal()}
	for _, root := range roots {
		stores = append(stores, collabs.get(root))
	}
	for _, st := range stores {
		if st == nil {
			continue
		}
		if _, err := st.PurgeNamespace(ctx, id); err != nil {
			slog.Warn("namespace: purge (mail)", "namespace", id, "workspace", st.Workspace(), "err", err)
		}
	}
}

// run drives the janitor until ctx ends.
func (r *namespaceRegistry) run(ctx context.Context) {
	if r == nil || r.store == nil {
		return
	}
	r.restore()
	t := time.NewTicker(namespaceTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.tick()
		}
	}
}
