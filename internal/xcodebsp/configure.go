package xcodebsp

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// State is the observable Xcode build-server lifecycle state.
type State string

const (
	StateDisabled                 State = "disabled"
	StateNotApplicable            State = "not_applicable"
	StateConfigured               State = "configured"
	StateUntrusted                State = "untrusted"
	StateAmbiguous                State = "ambiguous"
	StateConfiguring              State = "configuring"
	StateConfiguredNeedsBuildData State = "configured_needs_build_data"
	StateRestarting               State = "restarting"
	StateWarming                  State = "warming"
	StateSemanticProven           State = "semantic_proven"
	StateFailed                   State = "failed"
)

// Status is a snapshot suitable for logs, doctor, and session guidance.
type Status struct {
	State     State
	Detail    string
	Selection Selection
}

// ExecResult is the bounded result needed from an argv-only command runner.
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	TimedOut bool
}

// Runner executes argv without a shell.
type Runner interface {
	Run(context.Context, string, []string, time.Duration) (ExecResult, error)
}

// ConfigureRequest carries resolved config and the mandatory workspace trust bit.
type ConfigureRequest struct {
	Root    string
	Scheme  string
	Timeout time.Duration
	Enabled bool
	Trusted bool
}

func configurePreflight(req ConfigureRequest, runner Runner) (Inspection, Status, bool) {
	inspection := Inspect(req.Root)
	switch {
	case !inspection.IsBareXcode():
		return inspection, Status{State: StateNotApplicable}, true
	case inspection.BuildServerOK:
		return inspection, Status{State: StateConfigured, Detail: "buildServer.json is valid"}, true
	case inspection.BuildServerErr != nil:
		return inspection, Status{State: StateFailed, Detail: "existing buildServer.json is invalid; refusing to overwrite: " + inspection.BuildServerErr.Error()}, true
	case !req.Enabled:
		return inspection, Status{State: StateDisabled, Detail: "xcode.auto_build_server is off"}, true
	case !req.Trusted:
		return inspection, Status{State: StateUntrusted, Detail: "workspace trust is required before Xcode tooling can run"}, true
	case runner == nil:
		return inspection, Status{State: StateFailed, Detail: "Xcode command runner is unavailable"}, true
	default:
		return inspection, Status{}, false
	}
}

// Configure generates a buildServer.json without building the Xcode project.
func Configure(ctx context.Context, req ConfigureRequest, runner Runner) Status {
	inspection, status, done := configurePreflight(req, runner)
	if done {
		return status
	}

	marker, err := inspection.SelectMarker()
	if err != nil {
		return Status{State: StateAmbiguous, Detail: err.Error()}
	}
	listArgv := []string{"xcodebuild", marker.Flag, marker.Path, "-list", "-json"}
	list, err := runner.Run(ctx, req.Root, listArgv, req.Timeout)
	if err != nil {
		return failedCommand("listing Xcode schemes", list, err)
	}
	if list.ExitCode != 0 || list.TimedOut {
		return failedCommand("listing Xcode schemes", list, nil)
	}
	schemes, err := Schemes(marker, []byte(list.Stdout))
	if err != nil {
		return Status{State: StateFailed, Detail: err.Error()}
	}
	scheme, err := SelectScheme(schemes, req.Scheme)
	if err != nil {
		return Status{State: StateAmbiguous, Detail: err.Error()}
	}
	selection := Selection{Marker: marker, Scheme: scheme}
	configArgv := []string{"xcode-build-server", "config", "-scheme", scheme, marker.Flag, marker.Path}
	generated, err := runner.Run(ctx, req.Root, configArgv, req.Timeout)
	if err != nil {
		status := failedCommand("configuring Xcode build server", generated, err)
		status.Selection = selection
		return status
	}
	if generated.ExitCode != 0 || generated.TimedOut {
		status := failedCommand("configuring Xcode build server", generated, nil)
		status.Selection = selection
		return status
	}
	after := Inspect(req.Root)
	if !after.BuildServerOK {
		detail := "xcode-build-server completed without a valid buildServer.json"
		if after.BuildServerErr != nil {
			detail += ": " + after.BuildServerErr.Error()
		}
		return Status{State: StateFailed, Detail: detail, Selection: selection}
	}
	return Status{
		State:     StateConfiguredNeedsBuildData,
		Detail:    "buildServer.json configured; an existing Xcode build log is required for complete semantic data",
		Selection: selection,
	}
}

// Hint renders lifecycle-aware guidance for session orientation and empty semantic
// results. Configuration, build data, restart, and semantic proof remain distinct.
func (s Status) Hint() string {
	prefix := "\n\nXcode note: "
	switch s.State {
	case StateDisabled:
		return prefix + "automatic build-server setup is disabled. Set [xcode] auto_build_server = true and trust this workspace, or configure xcode-build-server manually. Plumb never builds the project automatically."
	case StateUntrusted:
		return prefix + "automatic build-server setup is enabled but this workspace is not trusted. Review its configuration, then run plumb trust in the workspace before retrying."
	case StateAmbiguous:
		return prefix + s.Detail + ". Choose the intended Xcode marker or set [xcode] scheme explicitly; Plumb will not guess."
	case StateConfiguring:
		return prefix + "build-server configuration is in progress; retry the semantic query shortly."
	case StateConfigured, StateConfiguredNeedsBuildData:
		return prefix + "buildServer.json is valid, but complete semantic data may still require building the selected scheme once in Xcode. Plumb never runs that build automatically."
	case StateRestarting:
		return prefix + "buildServer.json was generated and SourceKit-LSP is restarting; retry shortly."
	case StateWarming:
		return prefix + "SourceKit-LSP restarted with the generated buildServer.json and is warming. If results remain incomplete, build the selected scheme once in Xcode; Plumb never runs that build automatically."
	case StateFailed:
		return prefix + "automatic build-server setup failed: " + s.Detail
	default:
		return ""
	}
}

func failedCommand(action string, result ExecResult, err error) Status {
	detail := action + " failed"
	switch {
	case result.TimedOut:
		detail += " (timed out)"
	case err != nil:
		detail += ": " + err.Error()
	default:
		detail += fmt.Sprintf(" (exit %d)", result.ExitCode)
	}
	if stderr := strings.TrimSpace(result.Stderr); stderr != "" {
		detail += ": " + stderr
	}
	return Status{State: StateFailed, Detail: detail}
}
