# Provenance

`fsevents_darwin.go` is plumb's own FSEvents binding. Its CoreFoundation,
CoreServices and libdispatch calls through purego follow the darwin backend of
[github.com/fswatcher/fswatcher](https://github.com/fswatcher/fswatcher)
(`watcher_darwin.go`, v0.1.0), MIT licensed, Copyright (c) 2026 Yasuhiro
Matsumoto; the licence text is in `LICENSE-fswatcher`.

plumb does not depend on that module. The binding differs where plumb needed it
to (PLAN-488): event paths are passed through exactly as FSEvents reports them,
never resolved through symlinks, and a stream is never dropped when the root
changes (that library removed its stream on the first root rename, so a root
renamed away and recreated was never watched again; FSEvents itself keeps
watching the path). Closing a stream also waits, with `dispatch_sync_f` on its
serial queue, until no callback for it is running.
