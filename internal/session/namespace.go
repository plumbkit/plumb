package session

import (
	"os"
	"path/filepath"
	"strings"
)

// This file holds the session registry's side of PLAN-520's namespaces: each
// session row carries the namespace it belongs to (Info.Namespace), callers see
// only their own, and a namespace's purge removes its rows outright.

// Remove deletes a session's file outright, for a namespace purge: a purged
// namespace leaves no trace, not even an ended row waiting out its grace.
// Errors are ignored, like Unregister's.
func Remove(id string) {
	dir, err := Dir()
	if err != nil || id == "" || strings.ContainsAny(id, `/\`) {
		return
	}
	_ = withSessionDirLock(dir, func() error {
		err := os.Remove(filepath.Join(dir, id+".json"))
		if os.IsNotExist(err) {
			return nil
		}
		return err
	})
}

// NamespaceVisible reports whether a session is visible to a caller in
// namespace ns: each namespace, the default ("") included, sees only its own.
func NamespaceVisible(info Info, ns string) bool { return info.Namespace == ns }

// InNamespace keeps the sessions of namespace ns ("" is the default namespace).
// Rosters, peer lists and mail addressing see only their own namespace; a human
// view (the TUI, the web UI, `plumb sessions`) shows the default one.
func InNamespace(all []Info, ns string) []Info {
	out := all[:0:0]
	for _, info := range all {
		if NamespaceVisible(info, ns) {
			out = append(out, info)
		}
	}
	return out
}

// InNamespaceOf keeps the sessions in the namespace of the session selfID — the
// caller's own row — or the default namespace when selfID is not among them.
func InNamespaceOf(all []Info, selfID string) []Info {
	return InNamespace(all, NamespaceOf(all, selfID))
}

// NamespaceOf is the namespace on session id's row, or the default namespace
// when id is not among all.
func NamespaceOf(all []Info, id string) string {
	for _, info := range all {
		if info.ID == id {
			return info.Namespace
		}
	}
	return ""
}
