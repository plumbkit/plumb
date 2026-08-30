//go:build clients || clients_conformance

package clientsmoke

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

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
// Raw MCP and real-client adapters feed tool results into the same state machine;
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

type conformanceAction struct {
	stage     conformanceStage
	arguments map[string]any
	complete  bool
}

type conformanceScenario struct {
	step      int
	tmpHome   string
	workspace string
	readPath  string
	editPath  string
}

func newConformanceScenario(tmpHome, fixture string) *conformanceScenario {
	return &conformanceScenario{
		tmpHome:   tmpHome,
		workspace: fixture,
		readPath:  filepath.Join(fixture, "read.txt"),
		editPath:  filepath.Join(fixture, "edit.txt"),
	}
}

func (s *conformanceScenario) next(body string) (conformanceAction, error) {
	var action conformanceAction
	switch s.step {
	case 0:
		// session_start is the sole workspace-pin authority since serve started
		// unattached (f4de91ab): cwd is not intent, so a bare session_start has
		// nothing to pin from and refuses. Real clients name the workspace —
		// Codex via its config, Claude via the argument — so the scenario does
		// too, naming the fixture explicitly.
		action = conformanceAction{
			stage: deterministicConformanceScenario[stageSessionStart],
			arguments: map[string]any{
				"workspace": s.workspace,
				"purpose":   "client-conformance",
			},
		}

	case 1:
		if !strings.Contains(body, "Workspace:") {
			return action, errors.New("invocation: session_start result was not returned")
		}
		if !strings.Contains(body, "Tool profile: full") {
			return action, errors.New("discovery: session_start did not report the expected full profile")
		}
		action = conformanceAction{
			stage: deterministicConformanceScenario[stageReadSuccess],
			arguments: map[string]any{
				"file_path": s.readPath,
			},
		}

	case 2:
		if !strings.Contains(body, "clientsmoke-read-ok") {
			return action, errors.New("invocation: successful path-bearing read result was not returned")
		}
		action = conformanceAction{
			stage: deterministicConformanceScenario[stageEditRefusal],
			arguments: map[string]any{
				"file_path": s.editPath,
				"edits": []map[string]string{{
					"old_string": "before",
					"new_string": "after",
				}},
			},
		}

	case 3:
		stage := deterministicConformanceScenario[stageEditRefusal]
		if !stage.expectRefusal || !strings.Contains(body, "has not been read") {
			return action, errors.New("recovery: unread edit did not return the expected strict-mode refusal")
		}
		action = conformanceAction{
			stage: deterministicConformanceScenario[stageReadRemediation],
			arguments: map[string]any{
				"file_path": s.editPath,
			},
		}

	case 4:
		mtime := extractHeaderToken(body, "mtime=")
		if mtime == "" {
			return action, errors.New("recovery: remediation read did not return an mtime token")
		}
		action = conformanceAction{
			stage: deterministicConformanceScenario[stageEditRemediated],
			arguments: map[string]any{
				"file_path":      s.editPath,
				"expected_mtime": mtime,
				"edits": []map[string]string{{
					"old_string": "before",
					"new_string": "after",
				}},
			},
		}

	case 5:
		if !strings.Contains(body, "applied 1 edit") {
			return action, errors.New("recovery: edit did not succeed after the advertised read remediation")
		}
		action = conformanceAction{
			stage: deterministicConformanceScenario[stageReadBeforeReconnect],
			arguments: map[string]any{
				"file_path": s.editPath,
			},
		}

	case 6:
		mtime := extractHeaderToken(body, "mtime=")
		if mtime == "" || !strings.Contains(body, "after") {
			return action, errors.New("reconnect: pre-restart read did not return the edited content and mtime")
		}
		stage := deterministicConformanceScenario[stageEditAfterReconnect]
		if !stage.restartsDaemon {
			return action, errors.New("reconnect: shared scenario lost its daemon-restart contract")
		}
		stopDaemon(s.tmpHome)
		action = conformanceAction{
			stage: stage,
			arguments: map[string]any{
				"file_path":      s.editPath,
				"expected_mtime": mtime,
				"edits": []map[string]string{{
					"old_string": "after",
					"new_string": "after-reconnect",
				}},
			},
		}

	case 7:
		if !strings.Contains(body, "applied 1 edit") {
			return action, errors.New("reconnect: edit did not succeed from replayed pre-restart read state")
		}
		action.complete = true

	default:
		return action, fmt.Errorf("scenario received unexpected step %d", s.step+1)
	}
	s.step++
	return action, nil
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
