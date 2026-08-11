package base_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp/adapters/base"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
)

// writeTrackedDoc materialises a document under t.TempDir and returns its URI.
func writeTrackedDoc(t *testing.T, name, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return paths.PathToURI(path)
}

// openedDocs returns the text-document item of every didOpen the tracker sent,
// in order.
func openedDocs(t *testing.T, conn *stubCaller) []protocol.TextDocumentItem {
	t.Helper()
	var out []protocol.TextDocumentItem
	for _, r := range conn.sent() {
		if r.method != protocol.MethodDidOpen {
			continue
		}
		params, ok := r.params.(protocol.DidOpenTextDocumentParams)
		if !ok {
			t.Fatalf("didOpen params = %#v, want protocol.DidOpenTextDocumentParams", r.params)
		}
		out = append(out, params.TextDocument)
	}
	return out
}

// closedURIs returns the URI of every didClose the tracker sent, in order.
func closedURIs(t *testing.T, conn *stubCaller) []string {
	t.Helper()
	var out []string
	for _, r := range conn.sent() {
		if r.method != protocol.MethodDidClose {
			continue
		}
		params, ok := r.params.(protocol.DidCloseTextDocumentParams)
		if !ok {
			t.Fatalf("didClose params = %#v, want protocol.DidCloseTextDocumentParams", r.params)
		}
		out = append(out, params.TextDocument.URI)
	}
	return out
}

// TestOpenTracker_EnsureSendsDidOpenOnceWithTheLanguageID pins the wire shape of
// the lazy open: the document's on-disk text, version 1, and the tracker's own
// languageId — and exactly one didOpen however many times a query asks for it.
func TestOpenTracker_EnsureSendsDidOpenOnceWithTheLanguageID(t *testing.T) {
	conn := &stubCaller{}
	tracker := base.NewOpenTracker(base.New(conn, "stubls"), "swift")
	uri := writeTrackedDoc(t, "Greeter.swift", "struct Greeter {}\n")

	for range 3 {
		if err := tracker.Ensure(context.Background(), uri); err != nil {
			t.Fatalf("Ensure = %v, want nil error", err)
		}
	}

	opened := openedDocs(t, conn)
	if len(opened) != 1 {
		t.Fatalf("sent %d didOpen notifications, want exactly 1 — the tracker must cache", len(opened))
	}
	want := protocol.TextDocumentItem{URI: uri, LanguageID: "swift", Version: 1, Text: "struct Greeter {}\n"}
	if opened[0] != want {
		t.Fatalf("didOpen document = %#v, want %#v", opened[0], want)
	}
}

// TestOpenTracker_EnsureUsesItsOwnLanguageID proves the languageId is
// per-tracker state, not a package constant: the adapters sharing this
// type each send their own.
func TestOpenTracker_EnsureUsesItsOwnLanguageID(t *testing.T) {
	for _, languageID := range []string{"swift", "zig", "html"} {
		t.Run(languageID, func(t *testing.T) {
			conn := &stubCaller{}
			tracker := base.NewOpenTracker(base.New(conn, "stubls"), languageID)
			uri := writeTrackedDoc(t, "doc."+languageID, "x\n")

			if err := tracker.Ensure(context.Background(), uri); err != nil {
				t.Fatalf("Ensure = %v, want nil error", err)
			}
			if opened := openedDocs(t, conn); len(opened) != 1 || opened[0].LanguageID != languageID {
				t.Fatalf("didOpen languageId = %#v, want %q", opened, languageID)
			}
		})
	}
}

// TestOpenTracker_EnsureLabelsAnUnreadableDocument pins "<server> open <uri>",
// the label the lazy-open adapters surface when the file is not there.
// conformance.RunLazyOpenErrorContract pins the same string from the adapter
// side; this pins it where it is produced.
func TestOpenTracker_EnsureLabelsAnUnreadableDocument(t *testing.T) {
	conn := &stubCaller{}
	tracker := base.NewOpenTracker(base.New(conn, "stubls"), "swift")
	missing := paths.PathToURI(filepath.Join(t.TempDir(), "absent.swift"))

	err := tracker.Ensure(context.Background(), missing)
	if err == nil {
		t.Fatal("Ensure on a document that is not on disk = nil error, want the open failure")
	}
	// The cause is the operating system's own open error, whose wording is
	// platform-specific: the prefix pins the label, errors.Is pins the %w.
	if want := "stubls open " + missing + ": "; !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("error = %q, want prefix %q", err, want)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("error %q does not unwrap to fs.ErrNotExist — the %%w wrapping was lost", err)
	}
	if opened := openedDocs(t, conn); len(opened) != 0 {
		t.Fatalf("sent %#v, want nothing — an unreadable document must not reach the transport", opened)
	}
}

// TestOpenTracker_EnsureRetriesAfterAFailedDidOpen pins the tracking boundary: a
// document is recorded open only once the notification is away, so a transport
// failure does not leave the adapter believing a document is open that the
// server never saw.
func TestOpenTracker_EnsureRetriesAfterAFailedDidOpen(t *testing.T) {
	conn := &stubCaller{notifyErr: errTransport}
	tracker := base.NewOpenTracker(base.New(conn, "stubls"), "zig")
	uri := writeTrackedDoc(t, "main.zig", "pub fn main() void {}\n")
	ctx := context.Background()

	err := tracker.Ensure(ctx, uri)
	if want := "stubls didOpen: " + errTransport.Error(); err == nil || err.Error() != want {
		t.Fatalf("Ensure = %v, want %q", err, want)
	}
	if !errors.Is(err, errTransport) {
		t.Fatalf("error %q does not unwrap to the cause", err)
	}

	conn.mu.Lock()
	conn.notifyErr = nil
	conn.mu.Unlock()

	if err := tracker.Ensure(ctx, uri); err != nil {
		t.Fatalf("Ensure after the transport recovered = %v, want nil error", err)
	}
	if opened := openedDocs(t, conn); len(opened) != 2 {
		t.Fatalf("sent %d didOpen notifications, want 2 — the failed open must be retried", len(opened))
	}
}

// TestOpenTracker_RefreshClosesTrackedDocumentsSoTheNextEnsureReopens covers the
// whole staleness cycle: an open document that changes on disk is closed and
// forgotten, and the next query opens it again with the new content.
func TestOpenTracker_RefreshClosesTrackedDocumentsSoTheNextEnsureReopens(t *testing.T) {
	conn := &stubCaller{}
	tracker := base.NewOpenTracker(base.New(conn, "stubls"), "html")
	ctx := context.Background()
	uri := writeTrackedDoc(t, "index.html", "<p>one</p>\n")

	if err := tracker.Ensure(ctx, uri); err != nil {
		t.Fatalf("Ensure = %v, want nil error", err)
	}
	if err := os.WriteFile(paths.URIToPath(uri), []byte("<p>two</p>\n"), 0o600); err != nil {
		t.Fatalf("rewriting the document: %v", err)
	}
	tracker.Refresh(ctx, []protocol.FileEvent{{URI: uri, Type: protocol.FileChanged}})
	if err := tracker.Ensure(ctx, uri); err != nil {
		t.Fatalf("Ensure after Refresh = %v, want nil error", err)
	}

	if got := closedURIs(t, conn); len(got) != 1 || got[0] != uri {
		t.Fatalf("didClose URIs = %#v, want exactly [%s]", got, uri)
	}
	opened := openedDocs(t, conn)
	if len(opened) != 2 {
		t.Fatalf("sent %d didOpen notifications, want 2 — Refresh must force a reopen", len(opened))
	}
	if opened[1].Text != "<p>two</p>\n" {
		t.Fatalf("reopened text = %q, want the fresh on-disk content", opened[1].Text)
	}
}

// TestOpenTracker_RefreshIgnoresUntrackedDocuments proves Refresh is scoped to
// what the tracker itself opened: plumb hands it every watched-file event, and
// closing a document the server never opened would be a spurious notification.
func TestOpenTracker_RefreshIgnoresUntrackedDocuments(t *testing.T) {
	conn := &stubCaller{}
	tracker := base.NewOpenTracker(base.New(conn, "stubls"), "swift")
	ctx := context.Background()
	tracked := writeTrackedDoc(t, "Greeter.swift", "struct Greeter {}\n")

	if err := tracker.Ensure(ctx, tracked); err != nil {
		t.Fatalf("Ensure = %v, want nil error", err)
	}
	tracker.Refresh(ctx, []protocol.FileEvent{
		{URI: "file:///elsewhere/Other.swift", Type: protocol.FileChanged},
		{URI: tracked, Type: protocol.FileChanged},
	})
	// A second Refresh over the same events must stay silent: the document was
	// dropped from the map by the first.
	tracker.Refresh(ctx, []protocol.FileEvent{{URI: tracked, Type: protocol.FileChanged}})

	if got := closedURIs(t, conn); len(got) != 1 || got[0] != tracked {
		t.Fatalf("didClose URIs = %#v, want exactly [%s]", got, tracked)
	}
}

// TestOpenTracker_ConcurrentEnsureAndRefresh is a race-detector target: the
// adapters call Ensure from concurrent queries while watched-file events arrive
// on another goroutine.
func TestOpenTracker_ConcurrentEnsureAndRefresh(t *testing.T) {
	conn := &stubCaller{}
	tracker := base.NewOpenTracker(base.New(conn, "stubls"), "swift")
	ctx := context.Background()
	uri := writeTrackedDoc(t, "Greeter.swift", "struct Greeter {}\n")

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := tracker.Ensure(ctx, uri); err != nil {
				t.Errorf("Ensure = %v, want nil error", err)
			}
		}()
		go func() {
			defer wg.Done()
			tracker.Refresh(ctx, []protocol.FileEvent{{URI: uri, Type: protocol.FileChanged}})
		}()
	}
	wg.Wait()
}
