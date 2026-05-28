package cli

import (
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sgtdi/fswatcher"

	"github.com/golimpio/plumb/internal/lsp/protocol"
)

const (
	lspWatchCooldown = 200 * time.Millisecond
	// Directory-name regex only; no credentials are embedded here.
	lspWatchExcludeRegex = `(^|/)(\.[^/]+|vendor|node_modules|testdata|dist|build|__pycache__)(/|$)` //nolint:gosec
)

type lspFSWatcher struct {
	workspace string
	client    *clientProxy
	w         fswatcher.Watcher

	cancel   context.CancelFunc
	done     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

func newLSPFSWatcher(workspace string, client *clientProxy) (*lspFSWatcher, error) {
	w, err := fswatcher.New(
		fswatcher.WithSeverity(fswatcher.SeverityNone),
		fswatcher.WithCooldown(lspWatchCooldown),
		fswatcher.WithExcRegex(lspWatchExcludeRegex),
		fswatcher.WithPath(workspace, fswatcher.WithDepth(fswatcher.WatchNested)),
	)
	if err != nil {
		return nil, err
	}
	return &lspFSWatcher{workspace: workspace, client: client, w: w, done: make(chan struct{})}, nil
}

func (fw *lspFSWatcher) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	fw.cancel = cancel
	fw.wg.Go(func() { _ = fw.w.Watch(ctx) })
	fw.wg.Go(fw.consume)
	slog.Info("lsp: file watcher started", "workspace", fw.workspace)
}

func (fw *lspFSWatcher) Stop() {
	fw.stopOnce.Do(func() {
		close(fw.done)
		if fw.cancel != nil {
			fw.cancel()
		}
		fw.w.Close()
	})
	fw.wg.Wait()
}

func (fw *lspFSWatcher) consume() {
	events := fw.w.Events()
	dropped := fw.w.Dropped()
	for {
		select {
		case <-fw.done:
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			fw.handle(ev)
		case _, ok := <-dropped:
			if !ok {
				return
			}
			slog.Warn("lsp: file watcher dropped events; language-server snapshot may need a daemon restart", "workspace", fw.workspace)
		}
	}
}

func (fw *lspFSWatcher) handle(ev fswatcher.WatchEvent) {
	if lspWatchHasOverflow(ev) {
		slog.Warn("lsp: file watcher overflow; language-server snapshot may need a daemon restart", "workspace", fw.workspace)
		return
	}
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
				Type: lspFileChangeType(ev),
			}},
		}); err != nil {
			slog.Warn("lsp: file watcher notification failed", "path", ev.Path, "err", err)
		}
	}
}

func lspWatchHasOverflow(ev fswatcher.WatchEvent) bool {
	return slices.Contains(ev.Types, fswatcher.EventOverflow)
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
		case ".git", ".plumb", "vendor", "node_modules", "testdata", "dist", "build", "__pycache__":
			return true
		}
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}

func lspFileChangeType(ev fswatcher.WatchEvent) protocol.FileChangeType {
	if slices.Contains(ev.Types, fswatcher.EventRemove) {
		return protocol.FileDeleted
	}
	if slices.Contains(ev.Types, fswatcher.EventCreate) {
		return protocol.FileCreated
	}
	return protocol.FileChanged
}
