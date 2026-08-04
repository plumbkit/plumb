package base

import (
	"context"
	"os"
	"sync"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
)

// OpenTracker keeps the set of documents an adapter has sent didOpen for, for
// the servers that answer per-document requests only for open documents:
// sourcekit-lsp replies -32001 "No language service for <uri> found", zls
// resolves an unopened file to nothing, and vscode-html-language-server has no
// filesystem access at all. plumb's external-edit model drives servers with
// didChangeWatchedFiles rather than the open-document lifecycle, so those three
// adapters open a document lazily before the first per-document query, keep it
// open, and drop it again when it changes on disk.
//
// # Hold it as a named field, never embed it
//
// An embedded *OpenTracker would promote Ensure and Refresh into the embedding
// adapter's exported surface, and plumb resolves optional adapter capabilities
// structurally (see the package doc). The three lazy-open adapters therefore
// declare it as a named field and call through it, which also keeps each
// ensure-open site visible in the concrete adapter — the set of methods that
// need one differs per server and is deliberately asymmetric.
//
// Concurrency: all methods are safe for concurrent use. OpenTracker holds a
// mutex and is used only through a pointer — never copy it.
type OpenTracker struct {
	base *Adapter

	// languageID is the LSP languageId sent in the didOpen text-document item
	// ("swift", "zig", "html"). Servers key their parser off it.
	languageID string

	mu   sync.Mutex
	open map[string]bool
}

// NewOpenTracker creates a tracker that opens documents on a, tagging each
// didOpen with languageID. Errors it returns carry a's server label.
func NewOpenTracker(a *Adapter, languageID string) *OpenTracker {
	return &OpenTracker{base: a, languageID: languageID, open: make(map[string]bool)}
}

// Ensure makes sure uri's current on-disk content is open on the server,
// reading the file and sending didOpen the first time it sees a URI.
// Already-open documents are left untouched, so repeated queries cost nothing.
//
// A document is recorded open only once the didOpen notification has been
// handed to the transport: a failure on either half (reading the file, sending
// the notification) leaves the URI untracked so the next query retries.
func (t *OpenTracker) Ensure(ctx context.Context, uri string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.open[uri] {
		return nil
	}
	content, err := os.ReadFile(paths.URIToPath(uri))
	if err != nil {
		return Wrap(t.base, "open "+uri, err)
	}
	if err := t.base.DidOpen(ctx, protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{URI: uri, LanguageID: t.languageID, Version: 1, Text: string(content)},
	}); err != nil {
		return err
	}
	t.open[uri] = true
	return nil
}

// Refresh closes any tracked document that appears in changes and forgets it,
// so the next Ensure reopens it with fresh content — didChangeWatchedFiles does
// not update the server's open-document copy.
//
// Callers pass the UNFILTERED change list: the tracker's copy is stale whatever
// the server's watcher globs say, and a document plumb opened is one plumb must
// keep current. Close failures are ignored deliberately — the document is
// dropped from the map either way, and a transport that cannot take a didClose
// cannot take the query that would follow it.
func (t *OpenTracker) Refresh(ctx context.Context, changes []protocol.FileEvent) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, c := range changes {
		if t.open[c.URI] {
			_ = t.base.DidClose(ctx, protocol.DidCloseTextDocumentParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: c.URI},
			})
			delete(t.open, c.URI)
		}
	}
}
