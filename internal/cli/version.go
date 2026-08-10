package cli

import (
	"encoding/json"
	"fmt"
	"runtime"
	"runtime/debug"
	"strconv"
	"sync"

	"github.com/spf13/cobra"
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
	// "" means unstamped; a value that is not a plausible SHA is treated as
	// unstamped too (see looksLikeRevision).
	Revision = ""
	// RevisionDirty records whether that source tree had uncommitted changes,
	// as "true" or "false" — ldflags can only inject strings. "" (or any value
	// that does not parse as a bool) means unknown, and is reported as unknown
	// rather than quietly assumed clean. Both stampers emit nothing at all when
	// they cannot measure the tree state, which lands here as unknown.
	RevisionDirty = ""
	// BuildChannel is "release" (a GoReleaser release build), "dev" (the
	// Makefile, or a GoReleaser --snapshot dry run), or "" (unknown — e.g. a
	// bare `go build` or `go run`).
	BuildChannel = ""
)

func init() {
	applyModuleVersionFallback()
	versionCmd.Flags().BoolVar(&versionJSON, "json", false,
		"print machine-readable JSON (version, source revision, runtime)")
}

// applyModuleVersionFallback upgrades the "dev" fallback with the module version
// Go embeds in the binary, so `go install github.com/plumbkit/plumb/cmd/plumb@v0.14.0`
// reports v0.14.0 rather than "dev". The ldflags stamp, when present, always
// wins; a workspace/source build has version "(devel)", which stays "dev".
func applyModuleVersionFallback() {
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
//  1. the ldflags stamps, when present and plausible, always win;
//  2. otherwise debug.ReadBuildInfo()'s vcs.revision / vcs.modified settings;
//  3. otherwise unknown, reported honestly as such — never guessed, and never
//     blank-passed-off-as-clean.
//
// Dirtiness is always resolved from the SAME source as the revision. With an
// explicit revision stamp, the embedded vcs.modified describes a different
// (outer) module and would be a lie, so it is not consulted; a build that
// stamps the revision but not the dirty flag reports dirtiness as unknown.
func resolveProvenance(revStamp, dirtyStamp, channelStamp string, settings map[string]string) BuildProvenance {
	p := BuildProvenance{Channel: channelStamp}
	switch {
	case looksLikeRevision(revStamp):
		p.Revision, p.RevisionKnown = revStamp, true
		p.Dirty, p.DirtyKnown = parseBoolStamp(dirtyStamp)
	case looksLikeRevision(settings["vcs.revision"]):
		p.Revision, p.RevisionKnown = settings["vcs.revision"], true
		p.Dirty, p.DirtyKnown = parseBoolStamp(settings["vcs.modified"])
	}
	return p
}

// looksLikeRevision reports whether s is a plausible git commit SHA: seven or
// more hex digits and nothing else.
//
// A stamp can be present but meaningless. GoReleaser renders {{ .FullCommit }}
// as the literal string "none" when it cannot resolve git information —
// reproducible with a --snapshot build in a repository that has no remote
// configured — and reporting revision "none" as revision_known would be exactly
// the unknown-presented-as-known this mechanism exists to prevent. Git object
// names are hex in both the SHA-1 and SHA-256 formats, so rejecting non-hex
// rejects placeholders without rejecting any real revision. An implausible
// stamp falls through to the next source rather than short-circuiting to
// unknown: a failed stamp is not a stamp.
func looksLikeRevision(s string) bool {
	if len(s) < 7 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
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
//
// Memoised: every input is fixed at link time, so the answer cannot change over
// the process lifetime, and rebuilding the settings map on each connection's
// tool registration is pure waste.
var Provenance = sync.OnceValue(func() BuildProvenance {
	return resolveProvenance(Revision, RevisionDirty, BuildChannel, buildSettings())
})

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
//
// The three dirty states get three renderings: a bare SHA means the tree was
// measured and clean, "-dirty" means measured and dirty, and "-dirty?" means the
// build could not measure it. Collapsing the last two into a bare SHA would make
// this the only surface where dirty is readable without dirty_known — the JSON
// payload and daemon_info both keep them apart.
func versionLine(p BuildProvenance, goVersion string) string {
	if !p.RevisionKnown {
		return fmt.Sprintf("plumb %s (%s)\n", Version, goVersion)
	}
	rev := shortRevision(p.Revision)
	switch {
	case !p.DirtyKnown:
		rev += "-dirty?"
	case p.Dirty:
		rev += "-dirty"
	}
	return fmt.Sprintf("plumb %s (%s, rev %s)\n", Version, goVersion, rev)
}

// versionJSON backs `plumb version --json`. The flag is named "json" so the
// shared suppressLogo rule withholds the banner for this invocation — a logo on
// stdout ahead of the payload is a parse error on line 1 for every consumer.
var versionJSON bool

// versionReport is the --json payload. Its key set is pinned by
// TestVersionJSONKeys, so renaming a key is a deliberate act rather than a
// refactor side effect.
//
// revision_known and dirty_known exist so a consumer can tell "clean" from "we
// have no idea": an unstamped build reports dirty=false with dirty_known=false,
// and reading the first without the second is exactly the bug they prevent.
type versionReport struct {
	Version       string `json:"version"`
	Revision      string `json:"revision"`
	RevisionKnown bool   `json:"revision_known"`
	Dirty         bool   `json:"dirty"`
	DirtyKnown    bool   `json:"dirty_known"`
	GoVersion     string `json:"go_version"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	BuildChannel  string `json:"build_channel"`
}

// newVersionReport assembles the payload from the resolved provenance plus the
// runtime facts a bug report needs.
func newVersionReport(p BuildProvenance, goVersion string) versionReport {
	return versionReport{
		Version:       Version,
		Revision:      p.Revision,
		RevisionKnown: p.RevisionKnown,
		Dirty:         p.Dirty,
		DirtyKnown:    p.DirtyKnown,
		GoVersion:     goVersion,
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		BuildChannel:  p.Channel,
	}
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	RunE: func(_ *cobra.Command, _ []string) error {
		p := Provenance()
		goVersion := goRuntimeVersion()
		if versionJSON {
			out, err := json.MarshalIndent(newVersionReport(p, goVersion), "", "  ")
			if err != nil {
				return fmt.Errorf("encoding version report: %w", err)
			}
			fmt.Println(string(out))
			return nil
		}
		fmt.Print(versionLine(p, goVersion))
		return nil
	},
}
