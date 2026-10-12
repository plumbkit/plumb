// Package separateenv tells a test harness whether it runs somewhere a plumb
// daemon of its own may be started.
//
// A developer machine runs exactly one plumb daemon, the one its sessions use.
// Test suites that start, kill or restart daemons (cmd/smoke, cmd/clientsmoke)
// therefore run only in a separate environment: a CI runner, a container or a
// disposable VM. Their TestMain calls RequireForTestMain, which refuses on
// anything else before the suite creates a directory, builds a binary or
// starts a process.
//
// This guards against running such a suite on a developer machine by accident
// (an agent, a habit, a stale recipe); it is not a security boundary, since
// anyone can fake the evidence. It accepts exactly three kinds:
//   - GITHUB_ACTIONS=true, on any OS: a hosted CI runner;
//   - Linux with a readable regular-file container marker, /.dockerenv
//     (Docker) or /run/.containerenv (Podman);
//   - Linux with PLUMB_SEPARATE_ENV=vm: an explicit declaration that this is a
//     disposable VM.
//
// Nothing else counts: no CI=true, no override, no case folding, and a marker
// that cannot be read counts as absent. testdata/vectors.json is byte-identical
// to the vectors of the same rule in the dsh-plumb-identity e2e.
package separateenv

import (
	"fmt"
	"os"
	"runtime"
	"strings"
)

// The kinds of separate environment Detect reports.
const (
	GitHubActions = "github-actions"
	Container     = "container"
	VM            = "vm"
)

// Probe is what Detect reads, injectable for tests.
type Probe struct {
	Getenv   func(string) string
	GOOS     string
	Readable func(path string) bool
}

// ContainerMarkers are the files a container runtime creates at the root.
func ContainerMarkers() []string {
	return []string{"/.dockerenv", "/run/.containerenv"}
}

// Detect returns the kind of separate environment p describes, or "" when it
// may be a developer machine.
func Detect(p Probe) string {
	if p.Getenv("GITHUB_ACTIONS") == "true" {
		return GitHubActions
	}
	if p.GOOS != "linux" {
		return ""
	}
	for _, marker := range ContainerMarkers() {
		if p.Readable(marker) {
			return Container
		}
	}
	if p.Getenv("PLUMB_SEPARATE_ENV") == "vm" {
		return VM
	}
	return ""
}

// Current is Detect for this process.
func Current() string {
	return Detect(Probe{Getenv: os.Getenv, GOOS: runtime.GOOS, Readable: ReadableFile})
}

// ReadableFile reports whether path is a regular file this process can open
// for reading. Anything else, including an error, is false.
func ReadableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	f, err := os.Open(path) //nolint:gosec // a fixed marker path, opened only to prove it is readable
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// Refusal is the message a suite named what prints when Current is "".
func Refusal(what string) string {
	return strings.Join([]string{
		what + ": refusing to run here: it starts plumb daemons of its own, and a developer",
		"machine must run exactly one plumb daemon (the one its sessions use).",
		"Run it in a separate environment instead: `make verify-full-container`",
		"(Docker), or CI. Accepted evidence: GITHUB_ACTIONS=true; Linux with a readable",
		"/.dockerenv or /run/.containerenv; Linux with PLUMB_SEPARATE_ENV=vm (a disposable VM).",
	}, "\n")
}

// RequireForTestMain exits the test binary with status 2 and Refusal(what)
// unless this process runs in a separate environment. Call it first in
// TestMain, before anything is created or started.
func RequireForTestMain(what string) {
	if Current() != "" {
		return
	}
	fmt.Fprintln(os.Stderr, Refusal(what))
	os.Exit(2)
}
