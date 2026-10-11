package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
)

// task_env_override_test.go is #634's execution half: run_task's per-call
// env/unset reaching the child, and the report saying what they did. The refusal
// half — which NAMES may be set or removed — is config.ValidateTaskEnvOverrides,
// wired into the tool through WriteDeps.TaskEnvRefusal (see Tasks.Execute), so
// these tests carry the real validator for the direct cases and a stub where the
// point is the wiring itself.

// newCallEnvTool builds a run_task whose resolver hands back one stored command
// with the given [tasks.<lang>] env and argv, with the real per-call env rule
// wired exactly as the cli seam wires it. The per-call overrides are applied by
// the tool itself, so a fake resolver is enough to exercise them.
func newCallEnvTool(root string, env, argv []string) *Tasks {
	return NewTasks(WriteDeps{
		WorkspaceFn:    func(context.Context) string { return root },
		TaskEnvRefusal: config.ValidateTaskEnvOverrides,
	}, func(_ context.Context, req TaskRequest) (TaskCommand, error) {
		return TaskCommand{
			Slot: req.Slot, Provenance: "project", Language: "go", Root: root,
			Env: env, Steps: [][]string{argv},
		}, nil
	})
}

// TestRunTask_EnvOverrideAppliesAndIsReported: a per-call entry reaches the
// child, the report lists the environment that was actually applied, and an entry
// that REPLACED a configured one says so — the replaced value is the one the
// project mirrors CI with, and it is invisible in the effective list.
func TestRunTask_EnvOverrideAppliesAndIsReported(t *testing.T) {
	root := t.TempDir()
	tool := newCallEnvTool(root,
		[]string{"GOTMPDIR={workspace}/.testcache"},
		[]string{"sh", "-c", `printf 'saw:%s|%s' "$GOTMPDIR" "$PLUMB_MODE"`})

	out, err := tool.Execute(context.Background(),
		json.RawMessage(`{"slot":"test","env":{"GOTMPDIR":"/tmp/agent-tmp","PLUMB_MODE":"experiment"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "saw:/tmp/agent-tmp|experiment") {
		t.Errorf("the child did not see the per-call env; got:\n%s", out)
	}
	if !strings.Contains(out, "env: GOTMPDIR=/tmp/agent-tmp PLUMB_MODE=experiment") {
		t.Errorf("the report must list the environment that was applied; got:\n%s", out)
	}
	if !strings.Contains(out, "note: env GOTMPDIR from this call replaces the [tasks.go] env entry for it") {
		t.Errorf("replacing a configured entry must be reported; got:\n%s", out)
	}
	if !strings.Contains(out, "→ ok") {
		t.Errorf("the command should have run to completion; got:\n%s", out)
	}
}

// TestRunTask_UnsetRemovesAConfiguredAndAnInheritedVariable is the case #634 was
// filed for: `unset: ["GOTMPDIR"]` runs the stored command under the system
// temporary directory instead of the checkout-local one the project configured,
// without a native shell. It must beat an INHERITED entry too, which is the half
// only the executor can see.
func TestRunTask_UnsetRemovesAConfiguredAndAnInheritedVariable(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PLUMB_INHERITED", "from-the-daemon")
	tool := newCallEnvTool(root,
		[]string{"GOTMPDIR={workspace}/.testcache"},
		[]string{"sh", "-c", `printf '%s|%s' "${GOTMPDIR-UNSET}" "${PLUMB_INHERITED-UNSET}"`})

	out, err := tool.Execute(context.Background(),
		json.RawMessage(`{"slot":"test","unset":["PLUMB_INHERITED","GOTMPDIR"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "UNSET|UNSET") {
		t.Errorf("unset must remove a configured entry AND an inherited one; got:\n%s", out)
	}
	if strings.Contains(out, "env: GOTMPDIR=") {
		t.Errorf("a removed variable must not be reported as applied; got:\n%s", out)
	}
	if !strings.Contains(out, "unset: GOTMPDIR PLUMB_INHERITED") {
		t.Errorf("the report must name what was removed, sorted; got:\n%s", out)
	}
	if !strings.Contains(out, "note: env GOTMPDIR is removed for this call, so the [tasks.go] env entry for it does not apply") {
		t.Errorf("removing a configured entry must be reported; got:\n%s", out)
	}
}

// TestRunTask_OverrideExpandsTheConfigPlaceholders: a per-call value goes through
// the same expansion as a configured one, so {workspace} means the same directory
// in both and a GOTMPDIR inside the workspace is created for it too (#537's rule,
// which a fresh worktree has no other way to satisfy).
func TestRunTask_OverrideExpandsTheConfigPlaceholders(t *testing.T) {
	root := t.TempDir()
	want := filepath.Join(root, ".testcache", "per-call")
	tool := newCallEnvTool(root, []string{"KEEP=1"},
		[]string{"sh", "-c", `test -d "$GOTMPDIR" && printf 'saw:%s' "$GOTMPDIR"`})

	out, err := tool.Execute(context.Background(),
		json.RawMessage(`{"slot":"test","env":{"GOTMPDIR":"{workspace}/.testcache/per-call"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "saw:"+want) {
		t.Errorf("a per-call {workspace} must expand to the resolver's root (%s); got:\n%s", want, out)
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Errorf("a per-call GOTMPDIR inside the workspace must be created like a configured one: %v", err)
	}
}

// TestRunTask_OverrideCredentialsAreWithheld: the per-call values are reported,
// so they go through the same withholding as a configured entry — the NAME is
// always shown (which variables were set is the point) and a value whose name
// says it is a credential is not.
func TestRunTask_OverrideCredentialsAreWithheld(t *testing.T) {
	root := t.TempDir()
	tool := newCallEnvTool(root, nil, []string{"true"})

	out, err := tool.Execute(context.Background(),
		json.RawMessage(`{"slot":"test","env":{"GITHUB_TOKEN":"opaque-value-123","MODE":"experiment"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "env: GITHUB_TOKEN=[REDACTED] MODE=experiment") {
		t.Errorf("the applied env must be named with the credential withheld; got:\n%s", out)
	}
	if strings.Contains(out, "opaque-value-123") {
		t.Errorf("a per-call credential value leaked into the report; got:\n%s", out)
	}
}

// TestRunTask_RefusesAnEnvChangeThatSteersExecution is the rule the tool applies
// before it resolves anything, through the wired validator: a name that decides
// what the stored command runs may be neither set nor removed. The taxonomy is
// config's (TestValidateTaskEnvOverrides covers it there); this pins that
// run_task consults it, and that the refusal reaches the caller.
func TestRunTask_RefusesAnEnvChangeThatSteersExecution(t *testing.T) {
	root := t.TempDir()
	tool := newCallEnvTool(root, nil, []string{"true"})
	resolverCalls := 0
	counting := NewTasks(WriteDeps{
		WorkspaceFn:    func(context.Context) string { return root },
		TaskEnvRefusal: config.ValidateTaskEnvOverrides,
	}, func(_ context.Context, req TaskRequest) (TaskCommand, error) {
		resolverCalls++
		return TaskCommand{Slot: req.Slot, Steps: [][]string{{"true"}}}, nil
	})

	for _, tc := range []struct {
		name    string
		args    string
		wantErr string
	}{
		{"setting the program the command runs", `{"slot":"test","env":{"PATH":"/tmp/bin"}}`, "changes which program"},
		{"setting go's own flags", `{"slot":"test","env":{"GOFLAGS":"-toolexec=/tmp/x"}}`, "changes which program"},
		{"setting a loader variable", `{"slot":"test","env":{"LD_PRELOAD":"/tmp/x.so"}}`, "dynamic-loader"},
		{"setting a name make reads", `{"slot":"test","env":{"MAKEFLAGS":"-f /tmp/evil.mk"}}`, "changes which program"},
		{"removing a configured steering name", `{"slot":"test","unset":["GOFLAGS"]}`, "changes what the stored command would run"},
		{"removing the module tree choice", `{"slot":"test","unset":["GOWORK"]}`, "changes what the stored command would run"},
		{"a name that is not portable", `{"slot":"test","env":{"bad name":"x"}}`, "not a valid environment variable name"},
		{"a value with a typo placeholder", `{"slot":"test","env":{"GOTMPDIR":"{workpsace}/x"}}`, "unknown placeholder"},
		{"the same name both ways", `{"slot":"test","env":{"GOTMPDIR":"/tmp"},"unset":["GOTMPDIR"]}`, "both env and unset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tool.Execute(context.Background(), json.RawMessage(tc.args)); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("= %v, want a refusal containing %q", err, tc.wantErr)
			}
			if _, err := counting.Execute(context.Background(), json.RawMessage(tc.args)); err == nil {
				t.Error("must be refused")
			}
		})
	}
	if resolverCalls != 0 {
		t.Errorf("a refused override reached the resolver %d time(s) — the check must run before resolution", resolverCalls)
	}

	// The remedy has to point at the only place that CAN set such a name.
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"slot":"test","env":{"PATH":"/tmp/bin"}}`))
	for _, want := range []string{"[tasks.<lang>] env", "plumb trust"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name %s: %v", want, err)
		}
	}
}

// TestRunTask_RefusesOverridesWithNoValidatorWired is the fail-closed half of the
// seam (#634 review): the rule lives in config, internal/tools does not import
// config, so the cli seam supplies it. A session that never did REFUSES the
// overrides rather than applying them unchecked — otherwise a second NewTasks
// caller with its own resolver would silently skip the rule by omission.
func TestRunTask_RefusesOverridesWithNoValidatorWired(t *testing.T) {
	resolved := false
	tool := NewTasks(WriteDeps{}, func(_ context.Context, req TaskRequest) (TaskCommand, error) {
		resolved = true
		return TaskCommand{Slot: req.Slot, Steps: [][]string{{"true"}}}, nil
	})
	for _, args := range []string{
		`{"slot":"test","env":{"MODE":"experiment"}}`,
		`{"slot":"test","unset":["GOTMPDIR"]}`,
	} {
		_, err := tool.Execute(context.Background(), json.RawMessage(args))
		if err == nil || !strings.Contains(err.Error(), "no env validator is wired") {
			t.Errorf("%s = %v, want a fail-closed refusal", args, err)
		}
	}
	if resolved {
		t.Error("an unvalidated override reached the resolver")
	}
	// And a call with no overrides is unaffected by the missing validator.
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"slot":"test"}`)); err != nil {
		t.Errorf("a call with no env changes must still run: %v", err)
	}
}

// TestRunTask_RefusesANullEnvValue: JSON null decodes into a Go string as "", so
// {"GOTMPDIR": null} would otherwise run the command with GOTMPDIR EMPTY — while
// the caller plainly meant "remove it". The refusal names unset, which is how a
// variable is actually removed (#634 review).
func TestRunTask_RefusesANullEnvValue(t *testing.T) {
	resolved := false
	tool := NewTasks(WriteDeps{TaskEnvRefusal: config.ValidateTaskEnvOverrides},
		func(_ context.Context, req TaskRequest) (TaskCommand, error) {
			resolved = true
			return TaskCommand{Slot: req.Slot, Steps: [][]string{{"true"}}}, nil
		})
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"slot":"test","env":{"GOTMPDIR":null}}`))
	if err == nil || !strings.Contains(err.Error(), "is null") || !strings.Contains(err.Error(), "unset") {
		t.Errorf("= %v, want a refusal naming the null value and pointing at unset", err)
	}
	if resolved {
		t.Error("a null env value reached the resolver")
	}
	// An EMPTY STRING is a value, not a removal, and stays legal.
	if err := config.ValidateTaskEnvOverrides(map[string]string{"CI": ""}, nil); err != nil {
		t.Errorf("an empty value must stay allowed: %v", err)
	}
}

// TestRunTask_EnvAndUnsetReachTheResolver pins the argument plumbing: the tool's
// env and unset arrive on the TaskRequest, which is the shape the validator is
// handed and the only channel a resolver could read them from. The two structs
// are converted wholesale (TaskRequest(a)), so this is what would break if that
// conversion were replaced by hand-copied fields.
func TestRunTask_EnvAndUnsetReachTheResolver(t *testing.T) {
	var got TaskRequest
	tool := NewTasks(WriteDeps{TaskEnvRefusal: func(map[string]string, []string) error { return nil }},
		func(_ context.Context, req TaskRequest) (TaskCommand, error) {
			got = req
			return TaskCommand{Slot: req.Slot, Steps: [][]string{{"true"}}}, nil
		})
	if _, err := tool.Execute(context.Background(),
		json.RawMessage(`{"slot":"test","env":{"MODE":"experiment"},"unset":["GOTMPDIR"]}`)); err != nil {
		t.Fatal(err)
	}
	if got.Env["MODE"] != "experiment" {
		t.Errorf("resolver saw env %v, want MODE=experiment", got.Env)
	}
	if len(got.Unset) != 1 || got.Unset[0] != "GOTMPDIR" {
		t.Errorf("resolver saw unset %v, want [GOTMPDIR]", got.Unset)
	}
}
