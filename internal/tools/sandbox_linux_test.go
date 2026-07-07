//go:build linux

package tools

import (
	"strings"
	"testing"
)

func TestBuildBwrapArgv(t *testing.T) {
	got := buildBwrapArgv("/usr/bin/bwrap", []string{"go", "build"},
		SandboxOpts{WorkspaceRoot: "/ws", AllowWrites: true, DenyNetwork: true})
	joined := strings.Join(got, " ")
	for _, want := range []string{"--ro-bind / /", "--tmpfs /tmp", "--bind /ws /ws", "--unshare-net"} {
		if !strings.Contains(joined, want) {
			t.Errorf("bwrap argv missing %q:\n%v", want, got)
		}
	}
	// plumb's own runtime dir is re-bound read-only, after the writable cache bind.
	if p := linuxPlumbRuntimeDir(); p != "" {
		if !strings.Contains(joined, "--ro-bind-try "+p+" "+p) {
			t.Errorf("bwrap argv does not re-bind plumb runtime dir read-only:\n%v", got)
		}
	}
	// The original argv is the tail, after the -- separator.
	if n := len(got); n < 2 || got[n-2] != "go" || got[n-1] != "build" {
		t.Errorf("original argv not preserved at tail: %v", got)
	}
}

func TestBuildBwrapArgv_NoWritesNoNet(t *testing.T) {
	got := buildBwrapArgv("/usr/bin/bwrap", []string{"echo", "hi"}, SandboxOpts{WorkspaceRoot: "/ws"})
	joined := strings.Join(got, " ")
	if strings.Contains(joined, "--bind /ws /ws") {
		t.Error("workspace bound read-write despite allow_writes=false")
	}
	if strings.Contains(joined, "--unshare-net") {
		t.Error("network unshared despite deny_network=false")
	}
}
