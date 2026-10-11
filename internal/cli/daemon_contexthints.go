package cli

// daemon_contexthints.go — the daemon's wiring for advisory context hints
// (context_hint.go): the ledger's lifecycle, the root resolver, and the off
// switch.

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/plumbkit/plumb/internal/contexthints"
	"github.com/plumbkit/plumb/internal/tools"
)

// contextHintsEnv overrides [context] hints in either direction: on|1|true
// turns hints on, off|0|false turns them off, anything else leaves the config
// in charge. Off costs nothing: the hook does not call the daemon, and a daemon
// that is asked anyway answers noop without opening or writing the ledger.
const contextHintsEnv = "PLUMB_CONTEXT_HINTS"

// hintEnvSwitch reads the override: set reports whether it says anything, on
// what it says.
func hintEnvSwitch(getenv func(string) string) (set, on bool) {
	switch strings.ToLower(strings.TrimSpace(getenv(contextHintsEnv))) {
	case "on", "1", "true":
		return true, true
	case "off", "0", "false":
		return true, false
	}
	return false, false
}

// contextHintsEnabled is the daemon's decision: its own environment's override,
// else the live config.
func contextHintsEnabled(configOn func() bool) bool {
	if set, on := hintEnvSwitch(os.Getenv); set {
		return on
	}
	return configOn != nil && configOn()
}

// openContextHintLedger opens the daemon's hint ledger; a var so tests can see
// whether, and when, it is opened.
var openContextHintLedger = contexthints.Open

// startContextHints returns the control-socket handler and a closer. The ledger
// is opened lazily, by the first request that finds hints on: a user who never
// turns hints on gets no context_hints.db at all. A ledger that cannot be opened
// leaves hints unavailable until the daemon restarts (the hook says nothing):
// the allowance lives in the ledger, so there is no unmetered fallback.
//
// hinter is the Slice A collector; nil until it lands, which answers every
// seeded request with noop(no-collector).
//
// configOn reads the live global [context] hints switch; the daemon's own
// PLUMB_CONTEXT_HINTS is honoured beside it.
func startContextHints(ctx context.Context, registry *connRegistry, hinter tools.ContextHinter, configOn func() bool) (func(context.Context, contextHintRequest) contextHintReply, func()) {
	enabled := func() bool { return contextHintsEnabled(configOn) }
	var (
		mu     sync.Mutex
		svc    *contextHintService
		ledger *contexthints.Store
		failed bool
	)
	service := func() *contextHintService {
		mu.Lock()
		defer mu.Unlock()
		if svc != nil || failed {
			return svc
		}
		l, err := openContextHintLedger()
		if err != nil {
			failed = true
			slog.Warn("daemon: context-hint ledger unavailable; hooks will receive no hints", "err", err)
			return nil
		}
		ledger = l
		svc = newContextHintService(hinter, l, registry.hintRoot, enabled)
		go pruneContextHints(ctx, l, reaperInterval)
		return svc
	}
	handler := func(ctx context.Context, req contextHintRequest) contextHintReply {
		if req.Off || !req.On && !enabled() {
			return contextHintReply{Outcome: contexthints.OutcomeNoop}
		}
		s := service()
		if s == nil {
			return contextHintReply{Outcome: contexthints.OutcomeError}
		}
		return s.serve(ctx, req)
	}
	closeLedger := func() {
		mu.Lock()
		defer mu.Unlock()
		if ledger != nil {
			ledger.Close()
		}
	}
	return handler, closeLedger
}

// pruneContextHints ages the ledger out on its own tick, best effort.
func pruneContextHints(ctx context.Context, ledger *contexthints.Store, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := ledger.Prune(time.Now()); err != nil {
			slog.Debug("daemon: context-hint ledger prune failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// hintRoot resolves a hook caller to the root it works in, by the same rule the
// mailbox probe uses to find a recipient: exactly one live connection must know
// the caller. Zero or several means no root — a hint is never routed by a guess.
func (r *connRegistry) hintRoot(external string) (hintRoot, bool) {
	r.mu.Lock()
	var resolvers []func(string) (string, bool, bool)
	for _, h := range r.conns {
		if h.hintRoot != nil {
			resolvers = append(resolvers, h.hintRoot)
		}
	}
	r.mu.Unlock()
	var found hintRoot
	matches := 0
	for _, resolve := range resolvers {
		if root, inherited, ok := resolve(external); ok && root != "" {
			found = hintRoot{Path: root, Inherited: inherited}
			matches++
		}
	}
	return found, matches == 1
}

// hookHintRoot is one connection's answer to hintRoot. An identity this
// connection already knows (its owner, or a registered logical-agent shard)
// resolves to the root its mail and calls already use. A subagent that has made
// no call yet — the SubagentStart case — resolves only when its conversation is
// live here, and then to the root its FIRST call will be seeded with
// (firstCallRoot, the same seeding shardOf runs), reported as inherited.
func (s *connSession) hookHintRoot(external string) (root string, inherited, ok bool) {
	if inbox, known := s.hookInbox(external); known {
		return inbox.Root, false, inbox.Root != ""
	}
	linkage := linkageIDOf(external)
	if linkage == external || linkage == "" {
		return "", false, false
	}
	if _, live := s.hookInbox(linkage); !live {
		return "", false, false
	}
	root, inherited = s.firstCallRoot(external)
	return root, inherited, root != ""
}
