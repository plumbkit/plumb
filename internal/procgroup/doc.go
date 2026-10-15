// Package procgroup puts a child process in its own process group and signals
// that group as a unit, so cancelling a child takes its descendants with it.
//
// Killing only the direct child leaves its own children running: a language
// server's telemetry helper, a build tool's compiler, a shell's backgrounded
// job. They are reparented to init and outlive the daemon that started them.
// A child that leads its own process group can be signalled together with
// every descendant that stayed in the group, by addressing the negative group
// id.
//
// The signalling half is Unix-only. On other platforms Set is a no-op and
// Terminate and Kill reach the direct child alone (the stdlib default), which
// keeps callers compiling; plumb itself does not support those platforms.
//
// A descendant that starts its own group (sourcekit-lsp's background
// swift-build does) is out of reach of a group signal. That is a limit of the
// mechanism, not of this package.
//
// Concurrency: every function is stateless and safe for concurrent use.
package procgroup
