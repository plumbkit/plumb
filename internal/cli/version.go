package cli

import "runtime/debug"

// Version is set at build time via -ldflags "-X github.com/plumbkit/plumb/internal/cli.Version=<tag>".
// Falls back to "dev" when built without the flag (e.g. go run, go test).
var Version = "dev"

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
