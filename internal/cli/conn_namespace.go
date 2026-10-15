package cli

import (
	"fmt"

	"github.com/plumbkit/plumb/internal/collab"
	"github.com/plumbkit/plumb/internal/session"
)

// This file is a connection's side of PLAN-520's namespaces: the gate that binds
// it at initialize, the stamp that marks its session row, and the renewal every
// call carries. The registry itself is namespaces.go.

// namespaces returns the daemon's namespace registry, or nil outside a daemon.
func (s *connSession) namespaces() *namespaceRegistry {
	if s.registry == nil {
		return nil
	}
	return s.registry.namespaces
}

// onNamespace is the connection's namespace gate (mcp.Server.OnNamespace). It
// runs before every other initialize hook, so identity restore, the workspace
// pin and every later tool call already see the namespace. A refusal error
// refuses the whole connection.
func (s *connSession) onNamespace(token, proxySessionID string) error {
	id, err := s.namespaces().gate(token, proxySessionID)
	if err == nil {
		// A connection keeps the namespace it first joined: a re-sent initialize
		// cannot move it into another, or out to the default one.
		if cur := s.view().namespace; cur != "" && cur != id {
			err = fmt.Errorf("this connection belongs to namespace %s", cur)
		}
	}
	if err != nil {
		s.log().Info("daemon: connection refused by its namespace", "err", err)
		if s.view().namespace == "" {
			// Retire the row registered at accept, so a refused connection is
			// not listed in the default namespace for as long as it lingers.
			session.Unregister(s.sessionID())
		}
		return err
	}
	if id != "" {
		s.mutate(func(v *sessionView) { v.namespace = id })
		s.stampNamespace() // at once; InitializeMeta stamps again after any adoption
		s.log().Info("daemon: connection joined namespace", "namespace", id)
	}
	return nil
}

// stampNamespace records the connection's namespace on its session row. Run
// once the initialize hooks are done, because one of them may adopt a different
// session ID (onSessionID) whose row must carry it.
func (s *connSession) stampNamespace() {
	ns := s.view().namespace
	if ns == "" {
		return
	}
	session.Patch(s.sessionID(), func(info *session.Info) { info.Namespace = ns })
}

// touchNamespace renews the connection's namespace on activity.
func (s *connSession) touchNamespace() {
	if ns := s.view().namespace; ns != "" {
		s.namespaces().touch(ns)
	}
}

// namespaceID is the connection's namespace ("" for the default), for the tools
// that scope what they list by it.
func (s *connSession) namespaceID() string { return s.view().namespace }

// nsCollab narrows a pooled collab store to the connection's namespace: its
// mail and intents are invisible outside it (PLAN-520). The pool's store is
// shared; the view only scopes the rows. nil stays nil.
func (s *connSession) nsCollab(st *collab.Store) *collab.Store {
	if st == nil {
		return nil
	}
	return st.InNamespace(s.view().namespace)
}

// nsCollabResult is nsCollab for a reader that also reports an error.
func (s *connSession) nsCollabResult(st *collab.Store, err error) (*collab.Store, error) {
	return s.nsCollab(st), err
}

// nsGlobalReader is the daemon-level store's reader, narrowed to the namespace.
func (s *connSession) nsGlobalReader() (*collab.Store, error) {
	return s.nsCollabResult(s.collabPool.getGlobalResult())
}

// nsCollabWrite is nsCollab on a write path: a namespaced connection also
// records the root it is about to write under, so the namespace's purge can
// reach the rows it leaves there.
func (s *connSession) nsCollabWrite(st *collab.Store, root string) *collab.Store {
	if ns := s.view().namespace; ns != "" && st != nil {
		if reg := s.namespaces(); reg != nil && reg.store != nil {
			if err := reg.store.AddNamespaceRoot(ns, root); err != nil {
				s.log().Warn("namespace: could not record a root for the purge", "namespace", ns, "root", root, "err", err)
			}
		}
	}
	return s.nsCollab(st)
}
