//go:build darwin

package fswatch

// fsevents_darwin.go is plumb's binding to CoreServices' FSEventStream API,
// called through purego so it needs no cgo: release builds (CGO_ENABLED=0) and
// local cgo builds run exactly this code. It is deliberately thin: one stream
// per watched root, paths and flags exactly as FSEvents reports them, nothing
// resolved or filtered here (backend_darwin.go does that).
//
// It replaces github.com/fswatcher/fswatcher, whose darwin backend this follows
// for the CoreFoundation, CoreServices and libdispatch calls (MIT licence,
// Copyright (c) 2026 Yasuhiro Matsumoto). Two of that library's choices
// broke plumb, and are the reason this file exists (PLAN-488):
//
//   - It resolves symlinks in every event path. Creating alias.go -> real.go
//     then arrives as a change to real.go, and the topology index, which keeps
//     an in-workspace symlink under its own name, never learns of the alias.
//     Here the path is FSEvents' own: the link's.
//   - It creates its stream with kFSEventStreamCreateFlagWatchRoot and drops the
//     stream the first time the root is renamed, so a workspace directory that is
//     moved away and recreated is never watched again and nothing says so. A
//     stream without that flag watches a PATH: it keeps reporting for whatever
//     directory lives there.

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// FSEvents event flags (FSEvents.h, kFSEventStreamEventFlag*).
const (
	fseMustScanSubDirs   = 0x00000001
	fseUserDropped       = 0x00000002
	fseKernelDropped     = 0x00000004
	fseRootChanged       = 0x00000020
	fseMount             = 0x00000040
	fseUnmount           = 0x00000080
	fseItemCreated       = 0x00000100
	fseItemRemoved       = 0x00000200
	fseItemInodeMetaMod  = 0x00000400
	fseItemRenamed       = 0x00000800
	fseItemModified      = 0x00001000
	fseItemFinderInfoMod = 0x00002000
	fseItemChangeOwner   = 0x00004000
	fseItemXattrMod      = 0x00008000
	fseItemIsFile        = 0x00010000
	fseItemIsDir         = 0x00020000
	fseItemIsSymlink     = 0x00040000
)

// FSEvents stream creation flags (kFSEventStreamCreateFlag*). WatchRoot (0x04)
// is deliberately absent; see the file comment.
const (
	fseCreateNoDefer    = 0x02
	fseCreateFileEvents = 0x10
)

const (
	fseSinceNow   = ^uint64(0) // kFSEventStreamEventIdSinceNow
	fseLatency    = 0.01       // seconds; plumb debounces itself
	cfStringUTF8  = 0x08000100 // kCFStringEncodingUTF8
	rawBatchQueue = 64         // batches buffered between a callback and its reader
)

// rawEvent is one FSEvents record: the path as reported, and its flags.
type rawEvent struct {
	path  string
	flags uint32
}

// fseStreamContext mirrors FSEventStreamContext. Only info is set; the rest
// stay zero (version 0, and no retain, release or copyDescription callbacks).
type fseStreamContext struct {
	_    int64      // version, a CFIndex
	info uintptr    // void *; the stream's registry id
	_    [3]uintptr // retain, release, copyDescription
}

// fseAPI holds the C entry points, resolved once per process.
//
// Concurrency: written once under load's sync.Once, read-only afterwards.
type fseAPI struct {
	load sync.Once
	err  error

	cfStringCreate   func(alloc uintptr, s string, encoding uint32) uintptr
	cfArrayCreate    func(alloc uintptr, capacity int, callbacks uintptr) uintptr
	cfArrayAppend    func(array, value uintptr)
	cfRelease        func(ref uintptr)
	cfArrayCallbacks uintptr // &kCFTypeArrayCallBacks

	streamCreate     func(alloc, callback uintptr, ctx *fseStreamContext, paths uintptr, since uint64, latency float64, flags uint32) uintptr
	streamSetQueue   func(stream, queue uintptr)
	streamStart      func(stream uintptr) bool
	streamStop       func(stream uintptr)
	streamInvalidate func(stream uintptr)
	streamRelease    func(stream uintptr)

	queueCreate  func(label string, attr uintptr) uintptr
	queueRelease func(queue uintptr)
	queueSyncF   func(queue, ctx, fn uintptr)
}

// fse is the process's one binding. A package variable because the C entry
// points and the callbacks below are process-wide by nature: purego never frees
// a callback, so they are made once rather than per stream.
var fse fseAPI

// fseStreams routes a callback to its stream by the id FSEvents hands back as
// the context's info pointer. A Go pointer cannot be given to C to keep, so an
// id stands in for it.
var fseStreams = struct {
	sync.Mutex
	next uintptr
	byID map[uintptr]*fseStream
}{byID: make(map[uintptr]*fseStream)}

// fseCallback is the FSEventStreamCallback every stream shares. It runs on the
// stream's dispatch queue, copies the batch out of C memory, and hands it to
// the stream's reader, waiting only until the stream is closed.
var fseCallback = purego.NewCallback(func(_, info, n uintptr, paths, flags, _ unsafe.Pointer) {
	fseStreams.Lock()
	s := fseStreams.byID[info]
	fseStreams.Unlock()
	if s == nil {
		return
	}
	batch := make([]rawEvent, n)
	for i := range batch {
		// eventPaths is a char ** because the stream is created without
		// kFSEventStreamCreateFlagUseCFTypes; eventFlags is a uint32 array.
		cstr := *(*unsafe.Pointer)(unsafe.Add(paths, uintptr(i)*unsafe.Sizeof(uintptr(0)))) //nolint:gosec // G103: reading FSEvents' callback arrays, bounded by its count
		batch[i] = rawEvent{
			path:  goString(cstr),
			flags: *(*uint32)(unsafe.Add(flags, uintptr(i)*4)), //nolint:gosec // G103: as above
		}
	}
	select {
	case s.batches <- batch:
	case <-s.closed:
	}
})

// fseBarrier is a no-op run with dispatch_sync_f: once it returns, no callback
// is running on that queue.
var fseBarrier = purego.NewCallback(func(uintptr) {})

// goString copies a NUL-terminated C string.
func goString(p unsafe.Pointer) string {
	if p == nil {
		return ""
	}
	n := 0
	for *(*byte)(unsafe.Add(p, n)) != 0 { //nolint:gosec // G103: walking a C string to its terminator
		n++
	}
	return string(unsafe.Slice((*byte)(p), n)) //nolint:gosec // G103: copying it into Go memory
}

// loadFSEvents resolves the C entry points once.
func loadFSEvents() error {
	fse.load.Do(func() {
		lib := func(path string) uintptr {
			if fse.err != nil {
				return 0
			}
			h, err := purego.Dlopen(path, purego.RTLD_LAZY|purego.RTLD_GLOBAL)
			if err != nil {
				fse.err = fmt.Errorf("fswatch: load %s: %w", path, err)
			}
			return h
		}
		cf := lib("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation")
		cs := lib("/System/Library/Frameworks/CoreServices.framework/CoreServices")
		sys := lib("/usr/lib/libSystem.B.dylib")
		if fse.err != nil {
			return
		}
		callbacks, err := purego.Dlsym(cf, "kCFTypeArrayCallBacks")
		if err != nil {
			fse.err = fmt.Errorf("fswatch: kCFTypeArrayCallBacks: %w", err)
			return
		}
		fse.cfArrayCallbacks = callbacks
		purego.RegisterLibFunc(&fse.cfStringCreate, cf, "CFStringCreateWithCString")
		purego.RegisterLibFunc(&fse.cfArrayCreate, cf, "CFArrayCreateMutable")
		purego.RegisterLibFunc(&fse.cfArrayAppend, cf, "CFArrayAppendValue")
		purego.RegisterLibFunc(&fse.cfRelease, cf, "CFRelease")
		purego.RegisterLibFunc(&fse.streamCreate, cs, "FSEventStreamCreate")
		purego.RegisterLibFunc(&fse.streamSetQueue, cs, "FSEventStreamSetDispatchQueue")
		purego.RegisterLibFunc(&fse.streamStart, cs, "FSEventStreamStart")
		purego.RegisterLibFunc(&fse.streamStop, cs, "FSEventStreamStop")
		purego.RegisterLibFunc(&fse.streamInvalidate, cs, "FSEventStreamInvalidate")
		purego.RegisterLibFunc(&fse.streamRelease, cs, "FSEventStreamRelease")
		purego.RegisterLibFunc(&fse.queueCreate, sys, "dispatch_queue_create")
		purego.RegisterLibFunc(&fse.queueRelease, sys, "dispatch_release")
		purego.RegisterLibFunc(&fse.queueSyncF, sys, "dispatch_sync_f")
	})
	return fse.err
}

// fseStream is one running FSEventStream over one path.
//
// Concurrency: batches is written by the FSEvents callback (on the stream's
// dispatch queue) and read by one reader goroutine. close may be called from any
// goroutine, more than once; when it returns no callback for this stream is
// running or will run again.
type fseStream struct {
	batches   chan []rawEvent
	closed    chan struct{}
	id        uintptr
	stream    uintptr
	queue     uintptr
	closeOnce sync.Once
}

// openFSEventStream starts a stream reporting file-level events for path and
// everything beneath it, from now on.
func openFSEventStream(path string) (*fseStream, error) {
	if err := loadFSEvents(); err != nil {
		return nil, err
	}
	s := &fseStream{batches: make(chan []rawEvent, rawBatchQueue), closed: make(chan struct{})}
	fseStreams.Lock()
	fseStreams.next++
	s.id = fseStreams.next
	fseStreams.byID[s.id] = s
	fseStreams.Unlock()
	unregister := func() {
		fseStreams.Lock()
		delete(fseStreams.byID, s.id)
		fseStreams.Unlock()
	}

	cfPath := fse.cfStringCreate(0, path, cfStringUTF8)
	if cfPath == 0 {
		unregister()
		return nil, fmt.Errorf("fswatch: CFStringCreateWithCString(%q) failed", path)
	}
	paths := fse.cfArrayCreate(0, 1, fse.cfArrayCallbacks)
	fse.cfArrayAppend(paths, cfPath)
	ctx := &fseStreamContext{info: s.id}
	s.stream = fse.streamCreate(0, fseCallback, ctx, paths, fseSinceNow, fseLatency, fseCreateFileEvents|fseCreateNoDefer)
	runtime.KeepAlive(ctx) // FSEventStreamCreate copies the context
	fse.cfRelease(paths)   // the stream retains what it needs
	fse.cfRelease(cfPath)
	if s.stream == 0 {
		unregister()
		return nil, errors.New("fswatch: FSEventStreamCreate failed")
	}
	s.queue = fse.queueCreate("dev.plumb.fswatch\x00", 0)
	fse.streamSetQueue(s.stream, s.queue)
	if !fse.streamStart(s.stream) {
		fse.streamInvalidate(s.stream)
		fse.streamRelease(s.stream)
		fse.queueRelease(s.queue)
		unregister()
		return nil, errors.New("fswatch: FSEventStreamStart failed")
	}
	return s, nil
}

// close stops the stream and waits until no callback for it is running.
func (s *fseStream) close() {
	s.closeOnce.Do(func() {
		close(s.closed) // a callback blocked handing over a batch gives up
		fse.streamStop(s.stream)
		fse.streamInvalidate(s.stream)
		// The queue is serial: once this no-op has run, any callback that was
		// already executing has returned, and an invalidated stream starts no
		// more.
		fse.queueSyncF(s.queue, 0, fseBarrier)
		fse.streamRelease(s.stream)
		fse.queueRelease(s.queue)
		fseStreams.Lock()
		delete(fseStreams.byID, s.id)
		fseStreams.Unlock()
	})
}

// openStreamCount reports how many streams are registered, for tests that
// check nothing leaks.
func openStreamCount() int {
	fseStreams.Lock()
	defer fseStreams.Unlock()
	return len(fseStreams.byID)
}
