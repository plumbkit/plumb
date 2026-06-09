package cli

// Version is set at build time via -ldflags "-X github.com/plumbkit/plumb/internal/cli.Version=<tag>".
// Falls back to "dev" when built without the flag (e.g. go run, go test).
var Version = "dev"
