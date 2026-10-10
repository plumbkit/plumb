package cli

// daemon_contexthints.go — the daemon's wiring for advisory context hints
// (context_hint.go): the ledger's lifecycle, the root resolver, and the off
// switch.

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/contexthints"
	"github.com/plumbkit/plumb/internal/tools"
)

// contextHintsEnvOff is the user-visible off switch. Any of off/0/false
// silences every hint; the ledger still records the noop, so "off" is
// measurable and never mistaken for an outage.
const contextHintsEnv = "PLUMB_CONTEXT_HINTS"

func contextHintsEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(contextHintsEnv))) {
	case "off", "0", "false":
		return false
	}
	return true
}

// startContextHints opens the ledger and returns the control-socket handler and
// a closer. A ledger that cannot be opened leaves hints unavailable (the hook
// gets an error line and says nothing): the allowance lives in the ledger, so
// there is no unmetered fallback.
//
// hinter is the Slice A collector; nil until it lands, which answers every
// seeded request with noop(no-collector).
func startContextHints(ctx context.Context, registry *connRegistry, hinter tools.ContextHinter) (func(context.Context, contextHintRequest) contextHintReply, func()) {
	ledger, err := contexthints.Open()
	if err != nil {
		slog.Warn("daemon: context-hint ledger unavailable; hooks will receive no hints", "err", err)
		return nil, func() {}
	}
	svc := newContextHintService(hinter, ledger, registry.hintRoot, contextHintsEnabled)
	go pruneContextHints(ctx, ledger, reaperInterval)
	return svc.serve, ledger.Close
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
