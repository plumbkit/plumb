package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// TestProcessIsPlumbDaemon pins the identity rule itself, on every platform, over
// argv SHAPES rather than over processes. That is deliberate: a real
// `plumb daemon` cannot be started outside a container (PLAN-518 §2 — exactly one
// runs on the owner's host and it is not a test's to start), and the cases that
// matter are the false positives, because a false positive is the one that ends
// in a signal.
func TestProcessIsPlumbDaemon(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want bool
	}{
		{"the installed daemon", []string{"/opt/homebrew/bin/plumb", "daemon"}, true},
		{"a relative invocation", []string{"plumb", "daemon"}, true},
		{"flags after daemon", []string{"/usr/local/bin/plumb", "daemon", "--log-level", "debug"}, true},
		{"an editor on the source", []string{"/usr/bin/vim", "plumb", "daemon.go"}, false},
		{"a shell running the command", []string{"/bin/sh", "-c", "plumb daemon"}, false},
		{"plumb serving, not daemoning", []string{"/opt/homebrew/bin/plumb", "serve"}, false},
		{"a different binary named like ours", []string{"/usr/bin/daemon", "plumb"}, false},
		{"no arguments at all", []string{"/opt/homebrew/bin/plumb"}, false},
		{"empty argv", nil, false},
		{"an empty argv element", []string{"", "daemon"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := processIsPlumbDaemon(tc.argv); got != tc.want {
				t.Fatalf("processIsPlumbDaemon(%q) = %v, want %v", tc.argv, got, tc.want)
			}
		})
	}
}

// TestProcessArgv_ReadsThisProcess is the PLAN-553 regression, in the shape a
// host can carry: the argv reader must work with no external command at all.
//
// The defect it pins was not that the identity rule was wrong but that the rule
// could not RUN where `ps` is absent (debian:bookworm-slim and minimal images):
// every pid then answered "not a plumb daemon", so `plumb restart` stopped
// nothing, and the legacy migration silently became a no-op — fail-closed, never
// two daemons, and the old daemon still serving. On Linux this reader is
// /proc-based; on darwin it is `ps`, which every such host has. Either way it must
// answer for a process that certainly exists: this one.
func TestProcessArgv_ReadsThisProcess(t *testing.T) {
	argv, ok := processArgv(os.Getpid())
	if !ok {
		t.Fatal("processArgv could not read this process: every identity check here would answer false, which is the PLAN-553 defect")
	}
	if len(argv) == 0 || argv[0] == "" {
		t.Fatalf("processArgv returned %q", argv)
	}
	if filepath.Base(argv[0]) == "" {
		t.Fatalf("argv[0] = %q has no basename", argv[0])
	}
	// The negative control in the same shape: this test binary is alive, is this
	// user's, and is not a daemon — so the reader answering is not the same as the
	// reader saying yes.
	if processIsPlumbDaemon(argv) {
		t.Fatalf("this test binary reads as a plumb daemon: %q", argv)
	}
	if isPlumbDaemonProcess(os.Getpid()) {
		t.Fatalf("isPlumbDaemonProcess(this process) = true for %q", argv)
	}
}
