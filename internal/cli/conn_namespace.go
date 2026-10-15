package cli

import "github.com/plumbkit/plumb/internal/session"

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
	if err != nil {
		s.log().Info("daemon: connection refused by its namespace", "err", err)
		return err
	}
	if id != "" {
		s.mutate(func(v *sessionView) { v.namespace = id })
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
