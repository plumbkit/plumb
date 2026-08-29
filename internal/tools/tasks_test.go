package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runTask(t *testing.T, tool *Tasks, args string) (string, error) {
	t.Helper()
	return tool.Execute(context.Background(), json.RawMessage(args))
}

// TestRunTask_ValidateSlot covers the two rejections that are now different
// things. A MALFORMED name is refused by the tool as input hygiene; a
// well-formed name with no command is refused by the resolver, which can say
// what IS configured. Before project-defined slots existed both were one
// closed-set check, and that check is what made the vocabulary Go-shaped.
func TestRunTask_ValidateSlot(t *testing.T) {
	tool := NewTasks(WriteDeps{}, func(string, string) (TaskCommand, error) { return TaskCommand{}, nil })

	for _, bad := range []string{"Deploy", "9deploy", "de ploy", "deploy!", ""} {
		args, _ := json.Marshal(map[string]any{"slot": bad})
		if _, err := runTask(t, tool, string(args)); err == nil {
			t.Errorf("slot %q is malformed and must be refused", bad)
		}
	}

	// Well formed but unconfigured: still an error, from the resolver.
	if _, err := runTask(t, tool, `{"slot":"deploy"}`); err == nil {
		t.Error("expected an error for a slot with no command")
	}
}

// TestRunTask_ProjectDefinedSlotReachesResolver is the point of the change: a
// slot the project named, not one of the built-in five, must reach the resolver
// rather than being refused by a closed set in this package.
func TestRunTask_ProjectDefinedSlotReachesResolver(t *testing.T) {
	var got string
	tool := NewTasks(WriteDeps{}, func(slot, _ string) (TaskCommand, error) {
		got = slot
		return TaskCommand{Slot: slot, Provenance: "project", Steps: [][]string{{"true"}}}, nil
	})
	if _, err := runTask(t, tool, `{"slot":"check"}`); err != nil {
		t.Fatalf("a project-defined slot must run: %v", err)
	}
	if got != "check" {
		t.Errorf("resolver saw slot %q, want \"check\"", got)
	}
}

// TestRunTask_SchemaHasNoSlotEnum pins the trade this change makes. An MCP
// client enforces an enum on its side, so a static enum and a project-defined
// vocabulary cannot both exist; the resolver's "configured slots" message is
// what replaces the client-side constraint.
func TestRunTask_SchemaHasNoSlotEnum(t *testing.T) {
	var schema struct {
		Properties struct {
			Slot map[string]any `json:"slot"`
		} `json:"properties"`
	}
	if err := json.Unmarshal((&Tasks{}).InputSchema(), &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if _, ok := schema.Properties.Slot["enum"]; ok {
		t.Error("slot must not declare an enum — it would re-close the vocabulary client-side")
	}
	if !strings.Contains(schema.Properties.Slot["description"].(string), "[tasks.<lang>]") {
		t.Error("with no enum, the description must say where the vocabulary comes from")
	}
}

func TestRunTask_TargetMustBeShellSafe(t *testing.T) {
	tool := NewTasks(WriteDeps{}, func(string, string) (TaskCommand, error) { return TaskCommand{}, nil })
	if _, err := runTask(t, tool, `{"slot":"test","target":"foo; rm -rf /"}`); err == nil {
		t.Error("expected a target with shell metacharacters to be refused")
	}
}

func TestRunTask_NoResolver(t *testing.T) {
	tool := NewTasks(WriteDeps{}, nil)
	if _, err := runTask(t, tool, `{"slot":"build"}`); err == nil {
		t.Error("expected an error when no resolver is wired")
	}
}

func TestRunTask_ResolverErrorPropagates(t *testing.T) {
	tool := NewTasks(WriteDeps{}, func(slot, _ string) (TaskCommand, error) {
		return TaskCommand{}, errors.New("untrusted: run `plumb trust`")
	})
	_, err := runTask(t, tool, `{"slot":"build"}`)
	if err == nil || !strings.Contains(err.Error(), "plumb trust") {
		t.Errorf("expected the resolver's trust error, got %v", err)
	}
}

// TestRunTask_RunsInTheResolvedWorkingDir is PLAN-325's acceptance criterion,
// asserted where it is decided.
//
// The holder-repository shape: the workspace root holds no module (only a
// go.work), the module sits in a subdirectory, and running from the root fails
// instantly while the tree compiles perfectly. Every Go task command in such a
// repository — plumb-ops itself — exited 1 in 0.02s, and mutation_test was
// unusable there because its compile gate could never pass.
//
// It asserts the cwd by having the command PRINT it, rather than by inspecting
// the TaskCommand: the question is where the process ran, and only the process
// can answer that. WorkspaceFn deliberately returns the root, so a regression
// that ignores WorkingDir has somewhere wrong to fall back to and the assertion
// can tell the two apart.
func TestRunTask_RunsInTheResolvedWorkingDir(t *testing.T) {
	root := t.TempDir()
	module := filepath.Join(root, "module")
	if err := os.MkdirAll(module, 0o755); err != nil {
		t.Fatal(err)
	}

	tool := NewTasks(WriteDeps{WorkspaceFn: func(context.Context) string { return root }},
		func(slot, _ string) (TaskCommand, error) {
			return TaskCommand{
				Slot:       slot,
				Provenance: "project",
				Steps:      [][]string{{"/bin/pwd"}},
				WorkingDir: module,
			}, nil
		})
	out, err := runTask(t, tool, `{"slot":"build"}`)
	if err != nil {
		t.Fatalf("run_task: %v", err)
	}

	got := reportedCwd(t, out)
	assertSameDir(t, got, module, "the command did not run in the resolved working_dir")
	if sameDir(got, root) {
		t.Errorf("the command ran in the workspace ROOT (%s), the holder directory that cannot build; output:\n%s", root, out)
	}
}

// reportedCwd pulls the directory /bin/pwd printed out of a run_task report.
func reportedCwd(t *testing.T, out string) string {
	t.Helper()
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "/") && !strings.HasPrefix(line, "$") {
			return line
		}
	}
	t.Fatalf("no directory line in the run_task report:\n%s", out)
	return ""
}

// assertSameDir compares directories by IDENTITY (the package's sameDir, which
// is os.SameFile-based), never by spelling.
//
// A string comparison here is a macOS-only time bomb: t.TempDir() hands back a
// path under /var, which is a symlink to /private/var, so the path the test
// holds and the path the child process reports are two spellings of one
// directory — green on Linux CI, red locally, for a reason that has nothing to
// do with the behaviour under test.
func assertSameDir(t *testing.T, got, want, msg string) {
	t.Helper()
	if !sameDir(got, want) {
		t.Errorf("%s: ran in %s, want %s", msg, got, want)
	}
}

// TestRunTask_FallsBackToTheWorkspaceRoot is the other direction: with no
// working_dir configured, behaviour is exactly what it was before the field
// existed. Without this, the test above would pass against a runner that had
// simply stopped consulting the workspace at all.
func TestRunTask_FallsBackToTheWorkspaceRoot(t *testing.T) {
	root := t.TempDir()
	tool := NewTasks(WriteDeps{WorkspaceFn: func(context.Context) string { return root }},
		func(slot, _ string) (TaskCommand, error) {
			return TaskCommand{Slot: slot, Provenance: "default", Steps: [][]string{{"/bin/pwd"}}}, nil
		})
	out, err := runTask(t, tool, `{"slot":"build"}`)
	if err != nil {
		t.Fatalf("run_task: %v", err)
	}
	assertSameDir(t, reportedCwd(t, out), root,
		"with no working_dir the command must still run in the workspace root")
}

func TestRunTask_RunsStepsAndStopsOnFailure(t *testing.T) {
	tool := NewTasks(WriteDeps{}, func(slot, _ string) (TaskCommand, error) {
		return TaskCommand{
			Slot:       "verify",
			Provenance: "default",
			Steps:      [][]string{{"echo", "building"}, {"false"}, {"echo", "should-not-run"}},
		}, nil
	})
	out, err := runTask(t, tool, `{"slot":"verify"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "building") || !strings.Contains(out, "stopped") {
		t.Errorf("expected the first step to run and the failing step to stop the chain:\n%s", out)
	}
	if strings.Contains(out, "should-not-run") {
		t.Error("a step after the failed one should not run")
	}
}

func TestRunTask_AllStepsOK(t *testing.T) {
	tool := NewTasks(WriteDeps{}, func(slot, _ string) (TaskCommand, error) {
		return TaskCommand{Slot: "build", Provenance: "global", Steps: [][]string{{"true"}}}, nil
	})
	out, err := runTask(t, tool, `{"slot":"build"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "→ ok") {
		t.Errorf("expected an ok result:\n%s", out)
	}
}

// TestRunTask_NoCommandRefusalCarriesItsRemedy is the rejection fixture for the
// larger of run_task's two message families, asserted at the MCP surface the
// agent actually sees rather than on noCommandError alone.
//
// It exists because the remedy is the whole point of the message: 15 of 41
// run_task failures in 90 days of telemetry were the bare sentence this
// replaced ("no test command configured for this workspace"), which named no
// config file, no language and no alternative slot, so the caller's only move
// was to abandon run_task for raw shell.
//
// Every assertion names something the CALLER never supplied — the language, the
// other configured slots, the absolute config path — so none of them can be
// satisfied by an echo of the request.
func TestRunTask_NoCommandRefusalCarriesItsRemedy(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), ".plumb", "config.toml")
	tool := NewTasks(WriteDeps{}, func(slot, _ string) (TaskCommand, error) {
		return TaskCommand{
			Slot: slot, Language: "go",
			Configured: []string{"build", "test"},
			ConfigPath: cfgPath,
		}, nil
	})
	_, err := runTask(t, tool, `{"slot":"lint"}`)
	if err == nil {
		t.Fatal("an unconfigured slot must be refused")
	}
	msg := err.Error()
	for _, want := range []string{"go", "build, test", "[tasks.go] lint", cfgPath, "agent_config"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal must name %q; got: %s", want, msg)
		}
	}

	// The fallback direction: a resolver that could not name a file must still
	// point at one, or the remedy is unactionable. Asserting only the presence of
	// cfgPath above would be satisfied by a message that hardcoded it.
	bare := NewTasks(WriteDeps{}, func(slot, _ string) (TaskCommand, error) {
		return TaskCommand{Slot: slot, Language: "go"}, nil
	})
	_, err = runTask(t, bare, `{"slot":"lint"}`)
	if err == nil {
		t.Fatal("an unconfigured slot must be refused")
	}
	if !strings.Contains(err.Error(), ".plumb/config.toml") {
		t.Errorf("with no resolved path the refusal must still name the config file; got: %s", err)
	}
	if strings.Contains(err.Error(), cfgPath) {
		t.Errorf("the fallback message leaked the other fixture's path: %s", err)
	}
}

// TestRunTask_TargetRefusalReachesTheCallerIntact pins that the resolver's
// enriched {target} refusal — the largest non-policy failure family — is passed
// through to the agent rather than flattened into the tool's own wording.
//
// The tool cannot build this message itself (it does not import config and
// cannot see the stored command or the file it came from), so the only thing
// that can go wrong here is the tool swallowing or rewriting it. The fixture
// spells the resolver's message out in full and requires it verbatim.
func TestRunTask_TargetRefusalReachesTheCallerIntact(t *testing.T) {
	const resolved = `run_task test: a target was given but the stored test command for go has no ` +
		`{target} placeholder. Stored command: "go test -count=1 ./..." (from /tmp/ws/.plumb/config.toml). ` +
		`To scope this slot, restore the placeholder plumb ships for it ("go test {target:./...}") ` +
		`under [tasks.go] test`
	tool := NewTasks(WriteDeps{}, func(_, target string) (TaskCommand, error) {
		if target == "" {
			return TaskCommand{Slot: "test", Steps: [][]string{{"true"}}}, nil
		}
		return TaskCommand{}, errors.New(resolved)
	})
	_, err := runTask(t, tool, `{"slot":"test","target":"./internal/cli"}`)
	if err == nil {
		t.Fatal("the resolver refused this call; run_task must surface that")
	}
	if err.Error() != resolved {
		t.Errorf("run_task rewrote the resolver's remedy.\n got: %s\nwant: %s", err, resolved)
	}

	// The other direction, in the same build: the same tool must still RUN when
	// no target is given, so the assertion above cannot be satisfied by a tool
	// that refuses everything.
	if _, err := runTask(t, tool, `{"slot":"test"}`); err != nil {
		t.Errorf("an unscoped call must still run: %v", err)
	}
}

// TestRunTask_NotesReachTheResponse pins the delivery of the resolver's notes.
//
// The notes exist because two things used to happen silently: a composite slot
// accepted a target, discarded it, ran the WHOLE suite and reported success; and
// a stored command was rewritten to make a target land. The tool cannot build
// either message (it does not import config), so the only thing that can go
// wrong here is the tool dropping it — which is exactly what a silent failure
// looks like from the caller's side.
func TestRunTask_NotesReachTheResponse(t *testing.T) {
	const note = `the target "./internal/cli" was NOT applied: verify is a composite`
	tool := NewTasks(WriteDeps{}, func(slot, target string) (TaskCommand, error) {
		cmd := TaskCommand{Slot: slot, Provenance: "default", Steps: [][]string{{"true"}}}
		if target != "" {
			cmd.Notes = []string{note}
		}
		return cmd, nil
	})

	out, err := runTask(t, tool, `{"slot":"verify","target":"./internal/cli"}`)
	if err != nil {
		t.Fatalf("a composite slot must not be refused a target: %v", err)
	}
	if !strings.Contains(out, note) {
		t.Errorf("run_task swallowed the resolver's note.\n got: %s\nwant it to contain: %s", out, note)
	}
	// It must be legible as a note rather than mistaken for command output.
	if !strings.Contains(out, "note: ") {
		t.Errorf("the note is not labelled as one: %s", out)
	}

	// Other direction, same build: a call the resolver had nothing to say about
	// carries no note, so the assertion above cannot be met by a constant string.
	out, err = runTask(t, tool, `{"slot":"verify"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "note: ") {
		t.Errorf("a call with nothing to disclose must carry no note: %s", out)
	}
}

// TestRunTask_NoCommandRemedyClosesTheTrustLoop covers the clause that keeps the
// remedy from delivering the caller into the NEXT refusal family. Both remedies
// it offers write the project config, and a project-supplied task command does
// not run until `plumb trust` — the second-largest policy refusal family in the
// telemetry — so the message has to say so, and has to name the alternative that
// needs no trust.
func TestRunTask_NoCommandRemedyClosesTheTrustLoop(t *testing.T) {
	tool := NewTasks(WriteDeps{}, func(slot, _ string) (TaskCommand, error) {
		return TaskCommand{Slot: slot, Language: "go", Configured: []string{"build"}}, nil
	})
	_, err := runTask(t, tool, `{"slot":"lint"}`)
	if err == nil {
		t.Fatal("an unconfigured slot must be refused")
	}
	for _, want := range []string{"plumb trust", "global config"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the remedy must mention %q or it ends in the next refusal; got: %s", want, err)
		}
	}
}
