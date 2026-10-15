package collab

import (
	"context"
	"errors"
	"fmt"
)

// namespace.go is how one collab.db serves several namespaces.
//
// A namespace is a private slice of the shared daemon: a set of sessions that
// can see each other's intents and mail and nobody else's. The daemon keeps ONE
// handle per collab.db file whatever the number of namespaces — a second handle
// to the same WAL file would be a second writer to contend with, and the
// per-store caches (the prepared probe, the ownership cache) are worth sharing —
// so the namespace cannot be the store. It is a property of the ROW instead:
// every row records the namespace it was written in (the v5 `namespace` column),
// and every view of the store binds its own namespace to every statement it
// issues.
//
// The default namespace is "". A file written before namespaces existed backfills
// to it, so the default view reads exactly what it always did, and a namespaced
// view starts from an empty mailbox rather than inheriting the default one's.
//
// The namespace is always a BOUND argument and never part of the statement text.
// That keeps the prepared-statement cache (probeStmt) one entry per statement
// shape rather than one per namespace, and it means no namespace name — which an
// operator chooses — can reach the SQL as anything but data.

// InNamespace returns a view of the same store that reads and writes only
// namespace ns's rows. It shares the database handle and every cache, so making
// one is cheap and holds no resource of its own; closing it is a no-op, and the
// store it came from stays open for every other view.
//
// The view is derived from the shared core, not from the receiver's namespace: a
// view of a view is a view of the original store in the new namespace, never an
// intersection. A nil store yields a nil view, whose methods then answer as a nil
// Store always has.
func (s *Store) InNamespace(ns string) *Store {
	if s == nil || s.storeCore == nil {
		return nil
	}
	return &Store{storeCore: s.storeCore, ns: ns, view: true}
}

// Namespace reports the namespace this view is scoped to; "" is the default.
func (s *Store) Namespace() string {
	if s == nil {
		return ""
	}
	return s.ns
}

// PurgeNamespace deletes every row of namespace ns, intents and notes alike,
// delivered or not, and returns how many rows went. It is how a namespace's
// state is wiped when the namespace is retired.
//
// It refuses the default namespace. "" is every pre-namespace row and every
// session that never chose one, so a call that reached here with an empty name by
// mistake — an unset variable, a lookup that found nothing — would otherwise
// delete the whole default mailbox; and there is no legitimate caller that wants
// that, since the default namespace is not retired. ns is checked before the
// store, so the refusal does not depend on what the store is.
//
// With Prune, this is one of the two statements that cross namespaces on purpose.
// It acts on ns whichever view it is called through — it is a store-level
// operation named by its argument, not a read of the receiver's own slice.
func (s *Store) PurgeNamespace(ctx context.Context, ns string) (int64, error) {
	if ns == "" {
		return 0, errors.New("collab: refusing to purge the default namespace")
	}
	if s == nil || s.storeCore == nil || s.db == nil {
		return 0, nil
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM collab_rows WHERE namespace = ?`, ns)
	if err != nil {
		return 0, fmt.Errorf("collab: purge namespace: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("collab: purge namespace: rows affected: %w", err)
	}
	return n, nil
}
