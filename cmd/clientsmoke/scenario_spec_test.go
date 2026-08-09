//go:build clients || clients_conformance

package clientsmoke

import "strings"

type conformanceStage struct {
	name           string
	tool           string
	expectRefusal  bool
	restartsDaemon bool
}

const (
	stageDiscovery = iota
	stageSessionStart
	stageReadSuccess
	stageEditRefusal
	stageReadRemediation
	stageEditRemediated
	stageReadBeforeReconnect
	stageEditAfterReconnect
)

// deterministicConformanceScenario is the protocol-independent scenario contract.
// Raw MCP and real-client adapters use the same ordered tool and outcome metadata;
// each adapter is responsible only for its wire format.
var deterministicConformanceScenario = [...]conformanceStage{
	{name: "discovery", tool: "tools/list"},
	{name: "invocation", tool: "session_start"},
	{name: "path_read", tool: "read_file"},
	{name: "known_refusal", tool: "edit_file", expectRefusal: true},
	{name: "advertised_remediation", tool: "read_file"},
	{name: "recovery", tool: "edit_file"},
	{name: "pre_reconnect_read", tool: "read_file"},
	{name: "reconnect_replay", tool: "edit_file", restartsDaemon: true},
}

func conformanceRequiredTools() []string {
	seen := make(map[string]bool)
	var tools []string
	for _, stage := range deterministicConformanceScenario {
		if strings.Contains(stage.tool, "/") || seen[stage.tool] {
			continue
		}
		seen[stage.tool] = true
		tools = append(tools, stage.tool)
	}
	return tools
}

func extractHeaderToken(body, prefix string) string {
	at := strings.LastIndex(body, prefix)
	if at < 0 {
		return ""
	}
	value := body[at+len(prefix):]
	if end := strings.IndexAny(value, " \n\r\t"); end >= 0 {
		value = value[:end]
	}
	return strings.Trim(value, "\\\"")
}
