//go:build darwin

package fswatch

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/fswatcher/fswatcher"

	"github.com/plumbkit/plumb/internal/paths"
)

// maxHoldFactor bounds how long the debouncer may hold one path, as a multiple
// of the cooldown. A trailing debounce alone starves a path that is rewritten
// faster than the cooldown (a log, a generator in a loop): every change pushes
// the deadline out again and nothing is ever delivered.
const maxHoldFactor = 5

// startBackend runs one FSEvents stream over the root. fswatcher/fswatcher
// reaches CoreServices through purego, so this holds in CGO_ENABLED=0 builds;
// the stream reports paths and opens no file under the root.
func (w *Watcher) startBackend(opts Options) error {
	src, err := fswatcher.NewWatcher()
	if err != nil {
		return fmt.Errorf("fswatch: %w", err)
	}
	if err := src.AddRecursive(w.root, fswatcher.All); err != nil {
		_ = src.Close()
		return fmt.Errorf("fswatch: %w", err)
	}
	// FSEvents reports resolved paths (/private/var/... for /var/...), and the
	// library resolves symlinks in every event path besides; callers relate
	// events to the root they passed, so map the resolved prefix back.
	p := &fseventsPump{
		w:     w,
		canon: paths.Canonical(w.root),
		deb:   newDebouncer(opts.Cooldown),
	}
	// The library blocks its FSEvents dispatch queue while its channels are
	// full, so they are drained here without pause; whatever plumb's consumer
	// does, this goroutine never waits on it.
	w.wg.Go(func() { p.run(src.Events, src.Errors) })
	w.stop = func() { _ = src.Close() }
	return nil
}

// fseventsPump moves events from the library to the Watcher: rebased onto the
// caller's root, filtered, debounced, and delivered without blocking.
type fseventsPump struct {
	w     *Watcher
	canon string // the root with symlinks resolved
	deb   *debouncer
}

// run drains evs and errs until both are closed (the library closes them when
// it is closed) or the Watcher is closed. Every value on errs means events may
// be missing; the library sends one for FSEvents' MustScanSubDirs, which is how
// the kernel and fseventsd report dropped events, and treating any error alike
// keeps that from hinging on its message text.
func (p *fseventsPump) run(evs <-chan fswatcher.Event, errs <-chan error) {
	var tick <-chan time.Time
	if p.deb.cooldown > 0 {
		t := time.NewTicker(flushInterval(p.deb.cooldown))
		defer t.Stop()
		tick = t.C
	}
	for evs != nil || errs != nil {
		select {
		case <-p.w.done:
			return
		case ev, ok := <-evs:
			if !ok {
				evs = nil
				continue
			}
			p.accept(ev, time.Now())
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			slog.Debug("fswatch: FSEvents reported possible loss", "root", p.w.root, "err", err)
			p.w.signalLost()
		case now := <-tick:
			p.deb.flush(now, p.w.deliver)
		}
	}
}

// accept takes one library event through rebasing, exclusion and debouncing.
func (p *fseventsPump) accept(ev fswatcher.Event, now time.Time) {
	op := opFromLibrary(ev.Op)
	if op == 0 {
		return
	}
	path := p.rebase(ev.Name)
	if p.w.excluded(path) {
		return
	}
	p.emit(path, op, now)
	p.w.expandIfDir(path, op, func(child string) { p.emit(child, Create, now) })
}

// emit hands one change to the debouncer, or straight to the consumer when
// there is no cooldown.
func (p *fseventsPump) emit(path string, op Op, now time.Time) {
	if p.deb.cooldown == 0 {
		p.w.deliver(Event{Path: path, Op: op})
		return
	}
	p.deb.add(path, op, now)
}

// rebase rewrites a path under the resolved root onto the caller's spelling of
// the root. A path outside it (a symlink the library resolved to its target
// elsewhere) is returned unchanged; callers drop it when it is not under root.
func (p *fseventsPump) rebase(path string) string {
	if p.canon == p.w.root {
		return path
	}
	if path == p.canon {
		return p.w.root
	}
	if rest, ok := strings.CutPrefix(path, p.canon+string(filepath.Separator)); ok {
		return filepath.Join(p.w.root, rest)
	}
	return path
}

// opFromLibrary maps the library's bits onto Op explicitly, so a reordering of
// its constants cannot silently change what plumb sees.
func opFromLibrary(in fswatcher.Op) Op {
	var op Op
	if in.Has(fswatcher.Create) {
		op |= Create
	}
	if in.Has(fswatcher.Write) {
		op |= Write
	}
	if in.Has(fswatcher.Remove) {
		op |= Remove
	}
	if in.Has(fswatcher.Rename) {
		op |= Rename
	}
	if in.Has(fswatcher.Chmod) {
		op |= Chmod
	}
	return op
}

// flushInterval is how often held paths are checked: a quarter of the
// cooldown, so a quiet path is delivered at most 25% late, but no busier than
// every 10ms.
func flushInterval(cooldown time.Duration) time.Duration {
	return max(cooldown/4, 10*time.Millisecond)
}

// debouncer holds changes per path until the path has been quiet for the
// cooldown, or has been held for maxHoldFactor cooldowns, then emits the union
// of everything seen. Not safe for concurrent use: the pump goroutine owns it.
type debouncer struct {
	cooldown time.Duration
	pending  map[string]*heldEvent
}

type heldEvent struct {
	op          Op
	first, last time.Time
}

func newDebouncer(cooldown time.Duration) *debouncer {
	return &debouncer{cooldown: cooldown, pending: make(map[string]*heldEvent)}
}

// add records one change to path at now.
func (d *debouncer) add(path string, op Op, now time.Time) {
	if h, ok := d.pending[path]; ok {
		h.op |= op
		h.last = now
		return
	}
	d.pending[path] = &heldEvent{op: op, first: now, last: now}
}

// flush emits and forgets every path that is due at now.
func (d *debouncer) flush(now time.Time, emit func(Event)) {
	maxHold := maxHoldFactor * d.cooldown
	for path, h := range d.pending {
		if now.Sub(h.last) >= d.cooldown || now.Sub(h.first) >= maxHold {
			emit(Event{Path: path, Op: h.op})
			delete(d.pending, path)
		}
	}
}
