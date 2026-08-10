package cli

import (
	"fmt"
	"runtime/debug"
	"strconv"
)

// Version is set at build time via -ldflags "-X github.com/plumbkit/plumb/internal/cli.Version=<tag>".
// Falls back to "dev" when built without the flag (e.g. go run, go test).
var Version = "dev"

// Build-time provenance stamps, injected alongside Version via
// -ldflags "-X github.com/plumbkit/plumb/internal/cli.<name>=<value>".
//
// They exist because debug.ReadBuildInfo() cannot reliably answer "which public
// source commit is this binary?" for plumb. The private plumb-ops superproject
// mounts this repository as its ./plumb submodule through go.work, so a build
// launched from the ops root resolves the embedded VCS settings against the
// OUTER module: vcs.revision then describes plumb-ops (or is absent entirely)
// and vcs.modified reads true even for a spotlessly clean public tree. The
// Makefile always runs with its working directory inside this repository, so it
// can stamp the correct SHA explicitly — which is the whole point of these vars.
var (
	// Revision is the full public-source commit SHA this binary was built from.
	// "" means unstamped.
	Revision = ""
	// RevisionDirty records whether that source tree had uncommitted changes,
	// as "true" or "false" — ldflags can only inject strings. "" (or any value
	// that does not parse as a bool) means unknown, and is reported as unknown
	// rather than quietly assumed clean.
	RevisionDirty = ""
	// BuildChannel is "release" (GoReleaser), "dev" (the Makefile), or ""
	// (unknown — e.g. a bare `go build` or `go run`).
	BuildChannel = ""
)

// init upgrades the "dev" fallback with the module version Go embeds in the
// binary, so `go install github.com/plumbkit/plumb/cmd/plumb@v0.14.0` reports
// v0.14.0 rather than "dev". The ldflags stamp, when present, always wins; a
// workspace/source build has version "(devel)", which stays "dev".
func init() {
	if Version != "dev" {
		return
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			Version = v
		}
	}
}

// BuildProvenance is the resolved answer to "which source commit is this binary
// built from, and was that tree clean?".
//
// The *Known flags keep "unknown" distinguishable from "clean". A consumer must
// never read a zero Dirty on an unstamped build as evidence of a clean tree,
// and Revision is only meaningful when RevisionKnown is set.
type BuildProvenance struct {
	Revision      string
	RevisionKnown bool
	Dirty         bool
	DirtyKnown    bool
	Channel       string
}

// resolveProvenance applies the fixed resolution order. It is pure — it takes
// the three ldflags stamps and the already-flattened build-info settings — so
// every branch is testable without mutating package-level state:
//
//  1. the ldflags stamps, when present, always win;
//  2. otherwise debug.ReadBuildInfo()'s vcs.revision / vcs.modified settings;
//  3. otherwise unknown, reported honestly as such — never guessed, and never
//     blank-passed-off-as-clean.
//
// Dirtiness is always resolved from the SAME source as the revision. With an
// explicit revision stamp, the embedded vcs.modified describes a different
// (outer) module and would be a lie, so it is not consulted; an ldflags build
// that stamps the revision but not the dirty flag reports dirtiness as unknown.
func resolveProvenance(revStamp, dirtyStamp, channelStamp string, settings map[string]string) BuildProvenance {
	p := BuildProvenance{Channel: channelStamp}
	switch {
	case revStamp != "":
		p.Revision, p.RevisionKnown = revStamp, true
		p.Dirty, p.DirtyKnown = parseBoolStamp(dirtyStamp)
	case settings["vcs.revision"] != "":
		p.Revision, p.RevisionKnown = settings["vcs.revision"], true
		p.Dirty, p.DirtyKnown = parseBoolStamp(settings["vcs.modified"])
	}
	return p
}

// parseBoolStamp reads a "true"/"false" stamp from ldflags or from build info.
// Anything else — including the empty string — is unknown, never silently clean.
func parseBoolStamp(s string) (value, known bool) {
	b, err := strconv.ParseBool(s)
	if err != nil {
		return false, false
	}
	return b, true
}

// buildSettings flattens debug.ReadBuildInfo()'s settings into a lookup map.
// Returns nil when no build info is embedded (which resolveProvenance treats as
// "no VCS settings").
func buildSettings() map[string]string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return nil
	}
	m := make(map[string]string, len(info.Settings))
	for _, s := range info.Settings {
		m[s.Key] = s.Value
	}
	return m
}

// Provenance resolves this binary's build provenance from the ldflags stamps and
// the embedded build info. Exported because the daemon reports the same facts
// through daemon_info.
func Provenance() BuildProvenance {
	return resolveProvenance(Revision, RevisionDirty, BuildChannel, buildSettings())
}

// goRuntimeVersion reports the Go toolchain version the binary was built with,
// or "unknown" when no build info is embedded.
func goRuntimeVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.GoVersion != "" {
		return info.GoVersion
	}
	return "unknown"
}

// shortRevision abbreviates a commit SHA to 12 characters — git's own modern
// default abbreviation length: short enough to read, long enough to stay
// unambiguous in a repository this size. Shorter inputs pass through unchanged.
func shortRevision(rev string) string {
	const abbrev = 12
	if len(rev) <= abbrev {
		return rev
	}
	return rev[:abbrev]
}

// versionLine renders `plumb version`'s human output, newline included.
//
// With no revision stamped the line is byte-identical to what plumb has always
// printed — "plumb <version> (<go>)" — so anything scraping it keeps working.
// A known revision extends that SAME line rather than adding a second one:
// `make install` echoes `plumb version | tail -1`, and a trailing revision line
// would have replaced the version in that message.
func versionLine(p BuildProvenance, goVersion string) string {
	if !p.RevisionKnown {
		return fmt.Sprintf("plumb %s (%s)\n", Version, goVersion)
	}
	rev := shortRevision(p.Revision)
	if p.DirtyKnown && p.Dirty {
		rev += "-dirty"
	}
	return fmt.Sprintf("plumb %s (%s, rev %s)\n", Version, goVersion, rev)
}
