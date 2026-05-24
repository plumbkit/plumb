package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
)

// Version is set by the cli package so the MCP tool can report it.
var Version string

// versionTool reports plumb's version and runtime environment.
type versionTool struct{}

func NewVersion() *versionTool { return &versionTool{} }

func (*versionTool) Name() string { return "version" }

func (*versionTool) Description() string {
	return "Return the plumb server version, Go runtime, and OS/arch. Useful for debugging and bug reports."
}

func (*versionTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{},"required":[],"additionalProperties":false}`)
}

func (*versionTool) Execute(_ context.Context, _ json.RawMessage) (string, error) {
	v := Version
	if v == "" {
		v = "unknown"
	}
	return fmt.Sprintf("plumb %s\ngo %s\n%s/%s", v, runtime.Version(), runtime.GOOS, runtime.GOARCH), nil
}
