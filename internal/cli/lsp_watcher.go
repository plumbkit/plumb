package cli

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/plumbkit/plumb/internal/fswatch"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

const lspWatchCooldown = 200 * time.Millisecond

// lspWatchExcludeRegexFor builds the OS watcher's source exclusion for one
// workspace: the directories lspWatchShouldSkipPath skips, plus every
// dot-prefixed directory. It is anchored at the workspace
// (fswatch.ExcludeDirsRegex): the unanchored pattern it replaces matched a
// dot-prefixed ANCESTOR of the workspace too, so a checkout under ~/.config, or
// a test under a dot-prefixed cache, never delivered a single event.
// lspWatchShouldSkipPath stays the authoritative, workspace-relative filter.
func lspWatchExcludeRegexFor(workspace string) string {
	return fswatch.ExcludeDirsRegex(workspace, "vendor", "node_modules", "testdata", "dist", "build", "target", "out", "__pycache__")
}

// lspFSWatcher forwards file changes under one workspace to its language
// server as workspace/didChangeWatchedFiles.
//
// Concurrency: the OS watcher runs from construction; Start launches the
// consumer; Stop signals it, closes the OS watcher and joins. Idempotent.
type lspFSWatcher struct {
	workspace string
	client    *clientProxy

	// events and lost are the OS watcher's channels, and closeOS stops it.
	events  <-chan fswatch.Event
	lost    <-chan struct{}
	closeOS func()

	done     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

func newLSPFSWatcher(workspace string, client *clientProxy) (*lspFSWatcher, error) {
	w, err := fswatch.New(workspace, fswatch.Options{
		Cooldown:     lspWatchCooldown,
		ExcludeRegex: lspWatchExcludeRegexFor(workspace),
	})
	if err != nil {
		return nil, err
	}
	return &lspFSWatcher{
		workspace: workspace,
		client:    client,
		events:    w.Events(),
		lost:      w.Lost(),
		closeOS:   w.Close,
		done:      make(chan struct{}),
	}, nil
}

func (fw *lspFSWatcher) Start() {
	fw.wg.Go(fw.consume)
	slog.Info("lsp: file watcher started", "workspace", fw.workspace)
}

func (fw *lspFSWatcher) Stop() {
	fw.stopOnce.Do(func() {
		close(fw.done)
		if fw.closeOS != nil {
			fw.closeOS()
		}
	})
	fw.wg.Wait()
}

func (fw *lspFSWatcher) consume() {
	for {
		select {
		case <-fw.done:
			return
		case ev, ok := <-fw.events:
			if !ok {
				return
			}
			fw.handle(ev)
		case _, ok := <-fw.lost:
			if !ok {
				return
			}
			slog.Warn("lsp: file watcher lost events; language-server snapshot may need a daemon restart", "workspace", fw.workspace)
		}
	}
}

func (fw *lspFSWatcher) handle(ev fswatch.Event) {
	rel, err := filepath.Rel(fw.workspace, ev.Path)
	if err != nil || lspWatchShouldSkipPath(rel) {
		return
	}
	if c := fw.client.get(); c != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := c.DidChangeWatchedFiles(ctx, protocol.DidChangeWatchedFilesParams{
			Changes: []protocol.FileEvent{{
				URI:  protocol.FileURI(ev.Path),
				Type: lspFileChangeType(ev.Op),
			}},
		}); err != nil {
			slog.Warn("lsp: file watcher notification failed", "path", ev.Path, "err", err)
		}
	}
}

func lspWatchShouldSkipPath(rel string) bool {
	if rel == "" || rel == "." || strings.HasPrefix(rel, "..") {
		return true
	}
	for part := range strings.SplitSeq(rel, string(filepath.Separator)) {
		if part == "" {
			continue
		}
		switch part {
		case ".git", ".plumb", "vendor", "node_modules", "testdata", "dist", "build", "target", "out", "__pycache__":
			return true
		}
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}

// lspFileChangeType maps a possibly coalesced Op onto one LSP change type: a
// removal wins, then a creation; anything else is a change.
func lspFileChangeType(op fswatch.Op) protocol.FileChangeType {
	if op.Has(fswatch.Remove) {
		return protocol.FileDeleted
	}
	if op.Has(fswatch.Create) {
		return protocol.FileCreated
	}
	return protocol.FileChanged
}
