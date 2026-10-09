//go:build linux || windows

package fswatch

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	"github.com/sgtdi/fswatcher"
)

// startBackend runs sgtdi/fswatcher, which uses inotify on Linux and
// ReadDirectoryChangesW on Windows; neither opens the files it watches. sgtdi
// debounces and applies the exclusion itself, exactly as plumb configured it
// before this package existed.
func (w *Watcher) startBackend(opts Options) error {
	cfg := []fswatcher.WatcherOpt{
		fswatcher.WithSeverity(fswatcher.SeverityNone), // plumb does its own logging
		fswatcher.WithCooldown(opts.Cooldown),
		fswatcher.WithPath(w.root, fswatcher.WithDepth(fswatcher.WatchNested)),
	}
	if opts.ExcludeRegex != "" {
		cfg = append(cfg, fswatcher.WithExcRegex(opts.ExcludeRegex))
	}
	src, err := fswatcher.New(cfg...)
	if err != nil {
		return fmt.Errorf("fswatch: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.wg.Go(func() {
		if err := src.Watch(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("fswatch: watcher stopped", "root", w.root, "err", err)
			w.signalLost()
		}
	})
	w.wg.Go(func() { pumpSgtdi(w, src.Events(), src.Dropped()) })
	w.stop = func() {
		cancel()
		src.Close()
	}
	return nil
}

// pumpSgtdi forwards sgtdi's events until the Watcher is closed. An overflow
// marker, or anything on sgtdi's Dropped channel (its own buffer filled), means
// events were lost. A directory that appears is expanded (expandIfDir): inotify
// never reports files created in it before sgtdi has registered its watch.
func pumpSgtdi(w *Watcher, evs, dropped <-chan fswatcher.WatchEvent) {
	for {
		select {
		case <-w.done:
			return
		case ev, ok := <-evs:
			if !ok {
				return
			}
			if slices.Contains(ev.Types, fswatcher.EventOverflow) {
				w.signalLost()
				continue
			}
			if op := opFromTypes(ev.Types); op != 0 {
				w.deliver(Event{Path: ev.Path, Op: op})
				w.expandIfDir(ev.Path, op, func(child string) { w.deliver(Event{Path: child, Op: Create}) })
			}
		case _, ok := <-dropped:
			if !ok {
				return
			}
			w.signalLost()
		}
	}
}

// opFromTypes maps sgtdi's event types onto Op. EventUnknown still reports a
// change: something happened to the path, so the consumer must look.
func opFromTypes(types []fswatcher.EventType) Op {
	var op Op
	for _, t := range types {
		switch t {
		case fswatcher.EventCreate:
			op |= Create
		case fswatcher.EventMod:
			op |= Write
		case fswatcher.EventRemove:
			op |= Remove
		case fswatcher.EventRename:
			op |= Rename
		case fswatcher.EventChmod:
			op |= Chmod
		case fswatcher.EventUnknown:
			op |= Write
		}
	}
	return op
}
