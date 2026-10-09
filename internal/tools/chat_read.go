package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
)

// readStores preserves open errors while retaining any readable store, so a
// partial delivery is never dropped merely because the other store failed.
func (i Inbox) readStores() ([]*collab.Store, error) {
	var stores []*collab.Store
	var failures []error
	read := func(reader func() (*collab.Store, error), legacy func() *collab.Store) {
		var store *collab.Store
		var err error
		if reader != nil {
			store, err = reader()
		} else if legacy != nil {
			store = legacy()
		}
		if store != nil {
			stores = append(stores, store)
		}
		failures = append(failures, err)
	}
	read(i.WorkspaceReader, i.Workspace)
	if i.Policy.CrossProject {
		read(i.GlobalReader, i.Global)
	}
	return stores, errors.Join(failures...)
}

// ClaimResult is the explicit receive path. It preserves errors and any rows
// already claimed, unlike advisory Claim, which logs a failure and carries on.
func (i Inbox) ClaimResult(ctx context.Context) ([]collab.Row, error) {
	if !i.Policy.Mailbox || i.Self == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, chatClaimTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stores, err := i.readStores()
	var failures []error
	failures = append(failures, err)
	var out []collab.Row
	now := time.Now()
	for _, store := range stores {
		remaining := maxDeliveredPerCall - len(out)
		if remaining <= 0 {
			break
		}
		claim := store.ClaimNotes
		if i.Policy.KeepDeliveredNotes {
			claim = store.ClaimNotesKeeping
		}
		rows, claimErr := claim(ctx, i.claimant(), now, remaining)
		out = append(out, rows...)
		failures = append(failures, claimErr)
	}
	failures = append(failures, ctx.Err())
	return out, errors.Join(failures...)
}

// Snapshot observes the same stores and eligibility as delivery without claiming
// or reading bodies. Its digest is opaque metadata and uses constant memory.
func (i Inbox) Snapshot(ctx context.Context) (collab.MailboxSnapshot, error) {
	if !i.Policy.Mailbox || i.Self == "" {
		return collab.MailboxSnapshot{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, chatClaimTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return collab.MailboxSnapshot{}, err
	}
	stores, err := i.readStores()
	if err != nil {
		return collab.MailboxSnapshot{}, err
	}
	hash := sha256.New()
	var result collab.MailboxSnapshot
	now := time.Now()
	for _, store := range stores {
		part, err := store.Snapshot(ctx, i.claimant(), now)
		if err != nil {
			return collab.MailboxSnapshot{}, err
		}
		result.Count += part.Count
		_, _ = fmt.Fprintf(hash, "%t:%s;", store.IsGlobal(), part.Fingerprint)
	}
	result.Fingerprint = hex.EncodeToString(hash.Sum(nil))
	return result, nil
}
