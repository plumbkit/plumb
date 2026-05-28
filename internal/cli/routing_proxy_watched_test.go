package cli

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/golimpio/plumb/internal/lsp/protocol"
)

// watchedFilesRecordingClient embeds stubClient (routing_proxy_test.go) and
// shadows DidChangeWatchedFiles so the routing tests can assert which batches
// landed where.
type watchedFilesRecordingClient struct {
	*stubClient
	mu      sync.Mutex
	batches []protocol.DidChangeWatchedFilesParams
}

func (w *watchedFilesRecordingClient) DidChangeWatchedFiles(_ context.Context, params protocol.DidChangeWatchedFilesParams) error {
	w.mu.Lock()
	w.batches = append(w.batches, params)
	w.mu.Unlock()
	return nil
}

func (w *watchedFilesRecordingClient) totalEvents() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	var n int
	for _, b := range w.batches {
		n += len(b.Changes)
	}
	return n
}

// TestRoutingProxy_DidChangeWatchedFiles_SkipsLanguageNone is the 0.8.17
// regression guard: a watched-file event under a workspace detected as
// LanguageNone (a bare .git/ repo, no LSP language) must be dropped silently
// rather than routed to the primary, which would log the
// `acquiring none ... language "none" not configured` warning the user reported.
func TestRoutingProxy_DidChangeWatchedFiles_SkipsLanguageNone(t *testing.T) {
	base := freshTempDir(t)
	rootA := filepath.Join(base, "projA")
	rootNone := filepath.Join(base, "scratch")
	mustMkdir(t, rootA)
	mustWrite(t, filepath.Join(rootA, "go.mod"), "module a\n")
	mustMkdir(t, rootNone)
	mustMkdir(t, filepath.Join(rootNone, ".git")) // resolves as LanguageNone

	pool := newTestPool()
	clientA := &watchedFilesRecordingClient{stubClient: &stubClient{id: "A"}}
	installEntry(pool, rootA, clientA)

	rp := newRoutingProxy(pool)
	rp.setPrimary(rootA, pool.entries[rootA].proxy)

	err := rp.DidChangeWatchedFiles(context.Background(), protocol.DidChangeWatchedFilesParams{
		Changes: []protocol.FileEvent{
			{URI: "file://" + filepath.Join(rootA, "main.go"), Type: protocol.FileChanged},
			{URI: "file://" + filepath.Join(rootNone, "vulnerability-suppressions-local.xml"), Type: protocol.FileChanged},
			{URI: "file://" + filepath.Join(rootA, "other.go"), Type: protocol.FileChanged},
		},
	})
	if err != nil {
		t.Fatalf("DidChangeWatchedFiles: %v", err)
	}
	if got := clientA.totalEvents(); got != 2 {
		t.Errorf("clientA: got %d events, want 2 (LanguageNone event must be skipped)", got)
	}
	// The LanguageNone event must NOT have leaked into clientA's batches.
	for _, b := range clientA.batches {
		for _, ev := range b.Changes {
			if filepath.Dir(ev.URI[len("file://"):]) == rootNone {
				t.Errorf("LanguageNone path %q leaked into rootA's stream", ev.URI)
			}
		}
	}
}

// TestRoutingProxy_DidChangeWatchedFiles_GroupsByWorkspace verifies events
// span multiple LSP-attached workspaces are sent to the right language server
// each — never cross-routed.
func TestRoutingProxy_DidChangeWatchedFiles_GroupsByWorkspace(t *testing.T) {
	rootA, rootB := setupTwoProjects(t)

	pool := newTestPool()
	clientA := &watchedFilesRecordingClient{stubClient: &stubClient{id: "A"}}
	clientB := &watchedFilesRecordingClient{stubClient: &stubClient{id: "B"}}
	installEntry(pool, rootA, clientA)
	installEntry(pool, rootB, clientB)

	rp := newRoutingProxy(pool)
	rp.setPrimary(rootA, pool.entries[rootA].proxy)

	err := rp.DidChangeWatchedFiles(context.Background(), protocol.DidChangeWatchedFilesParams{
		Changes: []protocol.FileEvent{
			{URI: "file://" + filepath.Join(rootA, "a.go"), Type: protocol.FileChanged},
			{URI: "file://" + filepath.Join(rootB, "b.go"), Type: protocol.FileCreated},
			{URI: "file://" + filepath.Join(rootA, "a2.go"), Type: protocol.FileDeleted},
		},
	})
	if err != nil {
		t.Fatalf("DidChangeWatchedFiles: %v", err)
	}
	if got := clientA.totalEvents(); got != 2 {
		t.Errorf("clientA: got %d events, want 2 (a.go + a2.go)", got)
	}
	if got := clientB.totalEvents(); got != 1 {
		t.Errorf("clientB: got %d events, want 1 (b.go)", got)
	}
}

// TestRoutingProxy_DidChangeWatchedFiles_EmptyBatchIsNoOp verifies an empty
// changes list short-circuits without calling any client.
func TestRoutingProxy_DidChangeWatchedFiles_EmptyBatchIsNoOp(t *testing.T) {
	rootA, _ := setupTwoProjects(t)

	pool := newTestPool()
	clientA := &watchedFilesRecordingClient{stubClient: &stubClient{id: "A"}}
	installEntry(pool, rootA, clientA)

	rp := newRoutingProxy(pool)
	rp.setPrimary(rootA, pool.entries[rootA].proxy)

	if err := rp.DidChangeWatchedFiles(context.Background(), protocol.DidChangeWatchedFilesParams{}); err != nil {
		t.Fatalf("DidChangeWatchedFiles: %v", err)
	}
	if got := clientA.totalEvents(); got != 0 {
		t.Errorf("clientA: got %d events, want 0", got)
	}
}
