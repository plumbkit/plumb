package cli

import (
	"os"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/mcp"
)

// serverWriteTimeout is the per-connection response-write deadline. A blocked
// socket write would otherwise hold the connection's write mutex forever and
// wedge every later reply. PLUMB_WRITE_TIMEOUT
// accepts a Go duration; "0"/"off"/"disable" disables the deadline. An unset or
// unparseable value uses mcp's built-in default.
func serverWriteTimeout() time.Duration {
	v := strings.TrimSpace(os.Getenv("PLUMB_WRITE_TIMEOUT"))
	if v == "" {
		return mcp.DefaultWriteTimeout
	}
	switch strings.ToLower(v) {
	case "0", "off", "disable", "disabled", "none":
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return mcp.DefaultWriteTimeout
	}
	return d
}

// serverToolExecTimeout bounds a single Execute call for tools that opt into it
// (the filesystem read/list tools). Without a cap a stat/open/readdir on a slow
// or unresponsive mount runs unbounded until the MCP client abandons the call at
// its own multi-minute timeout. PLUMB_TOOL_EXEC_TIMEOUT accepts a Go duration;
// "0"/"off"/"disable" disables the bound. An unset or unparseable value uses
// mcp's built-in default.
func serverToolExecTimeout() time.Duration {
	v := strings.TrimSpace(os.Getenv("PLUMB_TOOL_EXEC_TIMEOUT"))
	if v == "" {
		return mcp.DefaultToolExecTimeout
	}
	switch strings.ToLower(v) {
	case "0", "off", "disable", "disabled", "none":
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return mcp.DefaultToolExecTimeout
	}
	return d
}
