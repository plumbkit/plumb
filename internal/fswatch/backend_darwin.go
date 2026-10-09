//go:build darwin

package fswatch

import (
	"path/filepath"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/paths"
)

// maxHoldFactor bounds how long the debouncer may hold one path, as a multiple
// of the cooldown. A trailing debounce alone starves a path that is rewritten
// faster than the cooldown (a log, a generator in a loop): every change pushes
// the deadline out again and nothing is ever delivered.
const maxHoldFactor = 5

// fseLossFlags mark a record that stands for changes FSEvents could not report
// one by one: a subtree to rescan because events were dropped (by the kernel,
// by fseventsd, or for this client), a volume mounted or unmounted inside the
// tree, or the root path changing (only reported with WatchRoot, which the
// stream does not use, but never ignored if it appears).
const fseLossFlags = fseMustScanSubDirs | fseUserDropped | fseKernelDropped | fseRootChanged | fseMount | fseUnmount

// startBackend runs one FSEvents stream over the root (fsevents_darwin.go). It
// reports paths and opens nothing under the root, in every build.
func (w *Watcher) startBackend(opts Options) error {
	s, err := openFSEventStream(w.root)
	if err != nil {
		return err
	}
	canon := paths.Canonical(w.root)
	p := &fseventsPump{
		w:     w,
		canon: canon,
		fold:  paths.FoldsCase(canon),
		deb:   newDebouncer(opts.Cooldown),
	}
	// The callback waits on this goroutine to take each batch, so it reads
	// without pause; whatever plumb's consumer does, it never waits on it.
	w.wg.Go(func() { p.run(s.batches) })
	w.stop = s.close
	return nil
}

// fseventsPump moves FSEvents records to the Watcher: rebased onto the caller's
// root, filtered, debounced, and delivered without blocking.
type fseventsPump struct {
	w     *Watcher
	canon string // the root with symlinks resolved, as FSEvents reports it
	fold  bool   // the root's volume is case-insensitive
	deb   *debouncer
}

// run drains batches until the Watcher is closed.
func (p *fseventsPump) run(batches <-chan []rawEvent) {
	var tick <-chan time.Time
	if p.deb.cooldown > 0 {
		t := time.NewTicker(flushInterval(p.deb.cooldown))
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-p.w.done:
			return
		case batch := <-batches:
			now := time.Now()
			for _, ev := range batch {
				p.accept(ev, now)
			}
		case now := <-tick:
			p.deb.flush(now, p.w.deliver)
		}
	}
}

// accept takes one FSEvents record through loss detection, rebasing,
// exclusion, debouncing and directory expansion.
func (p *fseventsPump) accept(ev rawEvent, now time.Time) {
	if ev.flags&fseLossFlags != 0 {
		p.w.signalLost()
	}
	op := opFromFlags(ev.flags)
	if op == 0 {
		return
	}
	path := p.rebase(ev.path)
	if path == p.w.root {
		// The root directory itself was removed, renamed away, recreated, or
		// replaced by another moved into its place. The stream watches the PATH
		// and keeps reporting for whatever directory lives there, but nothing
		// reports the contents of one that arrived whole, nor what left with the
		// old one: only a reconcile can. The root's own metadata changes (a
		// Chmod, a Write to the directory) need nothing.
		if op.Has(Create | Remove | Rename) {
			p.w.signalLost()
		}
		return
	}
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
// the root: FSEvents reports /private/var/... for a root given as /var/....
// On a case-insensitive volume (the APFS and HFS+ default) the prefix is also
// matched case-insensitively, because FSEvents spells the root as it is on disk
// and the caller may not have; on a case-sensitive one a differently cased
// prefix is another directory and is never rewritten. A path outside the root
// is returned unchanged; callers drop it.
func (p *fseventsPump) rebase(path string) string {
	rest, ok := cutRoot(path, p.canon, p.fold)
	if !ok {
		return path
	}
	if rest == "" {
		return p.w.root
	}
	return filepath.Join(p.w.root, rest)
}

// cutRoot reports whether path is root or lies beneath it, and the part after
// root's separator ("" for root itself). fold compares the prefix
// case-insensitively.
func cutRoot(path, root string, fold bool) (string, bool) {
	if len(path) < len(root) {
		return "", false
	}
	if head := path[:len(root)]; head != root && (!fold || !strings.EqualFold(head, root)) {
		return "", false
	}
	rest := path[len(root):]
	if rest == "" {
		return "", true
	}
	if rest[0] != filepath.Separator {
		return "", false // a sibling sharing the root's name as a prefix
	}
	return rest[1:], true
}

// opFromFlags maps FSEvents item flags onto Op. The flags of one record
// accumulate everything that happened to the path within the stream's latency,
// so several bits are common.
func opFromFlags(f uint32) Op {
	var op Op
	if f&fseItemCreated != 0 {
		op |= Create
	}
	if f&fseItemModified != 0 {
		op |= Write
	}
	if f&fseItemRemoved != 0 {
		op |= Remove
	}
	if f&fseItemRenamed != 0 {
		op |= Rename
	}
	if f&(fseItemInodeMetaMod|fseItemFinderInfoMod|fseItemChangeOwner|fseItemXattrMod) != 0 {
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
