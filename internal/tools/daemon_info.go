package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// daemonInfo returns session and daemon metadata to the calling agent.
type daemonInfo struct {
	sessID        string
	name          func() string
	daemonVersion string
	startedAt     time.Time
}

// NewDaemonInfo creates a tool that exposes session and daemon metadata.
// sessID and sessName identify the current MCP connection; daemonVersion and
// startedAt describe the daemon process itself.
func NewDaemonInfo(sessID, sessName, daemonVersion string, startedAt time.Time) *daemonInfo {
	return NewDaemonInfoFunc(sessID, func() string { return sessName }, daemonVersion, startedAt)
}

// NewDaemonInfoFunc creates a daemon_info tool whose session name can change
// during the session.
func NewDaemonInfoFunc(sessID string, name func() string, daemonVersion string, startedAt time.Time) *daemonInfo {
	return &daemonInfo{
		sessID:        sessID,
		name:          name,
		daemonVersion: daemonVersion,
		startedAt:     startedAt,
	}
}

func (t *daemonInfo) Name() string { return "daemon_info" }

func (t *daemonInfo) Description() string {
	return "Returns metadata about the current MCP session and daemon process: " +
		"session name (e.g. swift-falcon), session ID, daemon version, start timestamp, and uptime. " +
		"Use this to identify which session you are operating in or to verify the daemon state."
}

func (t *daemonInfo) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
}

func (t *daemonInfo) Execute(_ context.Context, _ json.RawMessage) (string, error) {
	up := time.Since(t.startedAt)
	h := int(up.Hours())
	m := int(up.Minutes()) % 60
	s := int(up.Seconds()) % 60
	var upStr string
	switch {
	case h > 0:
		upStr = fmt.Sprintf("%dh %dm", h, m)
	case m > 0:
		upStr = fmt.Sprintf("%dm %ds", m, s)
	default:
		upStr = fmt.Sprintf("%ds", s)
	}
	return fmt.Sprintf(
		"session name:   %s\nsession id:     %s\ndaemon version: %s\nstarted at:     %s\nuptime:         %s",
		t.name(),
		t.sessID,
		t.daemonVersion,
		t.startedAt.Format(time.RFC3339),
		upStr,
	), nil
}
