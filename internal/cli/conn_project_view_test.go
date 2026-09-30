package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/sessionstate"
	"github.com/plumbkit/plumb/internal/tools"
)

// Issue #522. On a shared connection an agent that pinned its own shard to
// project B, while the connection is pinned to project A, resolved run_task's
// [tasks.<lang>] block — commands, working_dir, env — from A's config and ran it
// against B's root. A's `working_dir = "plumb"` sent the agent's build into
// <B>/plumb, which does not exist. PLAN-440 item 4 moved the working directory's
// ROOT to the agent; the config it is joined with stayed the connection's.

// connProjectA is the connection's project: a trusted config whose task block
// is right for A and wrong anywhere else.
const connProjectA = `
[tasks.go]
working_dir = "sub"
build = "go build -v ./..."
test = "go test -count=1 {target:./...}"

[[command]]
name = "gen"
exec = ["go", "generate", "./..."]
`

// agentProjectSession pins a connection to a trusted project A, then pins a
// subagent's own shard to agentRoot. It returns the session, A, and the
// subagent's ctx.
func agentProjectSession(t *testing.T, agentRoot func(connRoot string) string) (*connSession, string, context.Context) {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	a := t.TempDir()
	mustGitDir(t, a)
	writeExecProject(t, a, connProjectA)
	if err := os.MkdirAll(filepath.Join(a, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	grantExecTrust(t, a)

	s := execTrustSession(t, a)
	s.mutate(func(v *sessionView) { v.acquiredLanguage = "go" })
	s.recordLogicalAgentCall("coordinator")
	s.recordLogicalAgentCall("subagent")
	ctx := mcp.WithLogicalAgent(context.Background(), "subagent")
	root := agentRoot(a)
	if _, err := s.repinAgent(ctx, root, "go", sessionstate.PinSourceSessionStart, true); err != nil {
		t.Fatalf("pinning the subagent to %s: %v", root, err)
	}
	if got := s.workspaceFor(ctx); filepath.Clean(got) != filepath.Clean(root) {
		t.Fatalf("precondition: agent root = %q, want %q", got, root)
	}
	return s, a, ctx
}

// separateProject is an agentRoot that is a project of its own, outside A.
func separateProject(t *testing.T, body string) func(string) string {
	t.Helper()
	return func(string) string {
		b := t.TempDir()
		mustGitDir(t, b)
		if body != "" {
			writeExecProject(t, b, body)
		}
		return b
	}
}

func defaultGoSteps(t *testing.T, slot string) [][]string {
	t.Helper()
	steps, err := buildTaskSteps(config.Defaults().Tasks["go"], "go", slot, "")
	if err != nil || len(steps) == 0 {
		t.Fatalf("default go %s steps: %v %v", slot, steps, err)
	}
	return steps
}

// The reported case: B has no project config, so its build is the shipped
// default, run at B's root — not A's command in <B>/sub.
func TestRunTask_AgentPinnedElsewhereResolvesItsOwnProjectConfig(t *testing.T) {
	var b string
	s, _, ctx := agentProjectSession(t, func(a string) string {
		b = separateProject(t, "")(a)
		return b
	})

	got, err := s.taskResolver(ctx, tools.TaskRequest{Slot: "build"})
	if err != nil {
		t.Fatalf("resolving build for the subagent: %v", err)
	}
	if filepath.Clean(got.WorkingDir) != filepath.Clean(b) {
		t.Errorf("run_task would run in %q; the agent is pinned to %q and its project sets no working_dir "+
			"(the connection project's working_dir leaked across)", got.WorkingDir, b)
	}
	if want := defaultGoSteps(t, "build"); !slices.EqualFunc(got.Steps, want, slices.Equal) {
		t.Errorf("run_task would run %v, want the agent project's (default) %v — the connection project's command leaked across", got.Steps, want)
	}
	if want := config.ProjectConfigPath(b); got.ConfigPath != want {
		t.Errorf("ConfigPath = %q, want the agent project's %q", got.ConfigPath, want)
	}
}

// B's own project config is trust-gated against B: A's grant does not bless it,
// and once B is trusted it is B's command that runs.
func TestRunTask_AgentPinnedElsewhereUsesItsOwnProjectsCommandAndTrust(t *testing.T) {
	var b string
	s, _, ctx := agentProjectSession(t, func(a string) string {
		b = separateProject(t, "[tasks.go]\nbuild = \"go vet ./...\"\n")(a)
		return b
	})

	if _, err := s.taskResolver(ctx, tools.TaskRequest{Slot: "build"}); err == nil || !strings.Contains(err.Error(), "not trusted") {
		t.Fatalf("an untrusted agent project must be refused whatever the connection's trust, got %v", err)
	}
	grantExecTrust(t, b)
	got, err := s.taskResolver(ctx, tools.TaskRequest{Slot: "build"})
	if err != nil {
		t.Fatalf("resolving build once B is trusted: %v", err)
	}
	want := [][]string{{"go", "vet", "./..."}}
	if !slices.EqualFunc(got.Steps, want, slices.Equal) || filepath.Clean(got.WorkingDir) != filepath.Clean(b) {
		t.Errorf("resolved %v in %q, want B's %v in %q", got.Steps, got.WorkingDir, want, b)
	}
}

// run_command reads [[command]] and its exec trust from the same place: A's
// trusted allow-list must not run in B, and B's own entries are gated on B.
func TestRunCommand_AgentPinnedElsewhereReadsItsOwnAllowList(t *testing.T) {
	var b string
	s, _, ctx := agentProjectSession(t, func(a string) string {
		b = separateProject(t, "")(a)
		return b
	})
	if got, err := s.commandResolver(ctx, "gen", ""); err == nil {
		t.Fatalf("the connection project's [[command]] resolved for an agent in another project: %+v", got)
	}

	writeExecProject(t, b, "[[command]]\nname = \"gen\"\nexec = [\"go\", \"generate\", \"./b/...\"]\n")
	if _, err := s.commandResolver(ctx, "gen", ""); err == nil || !strings.Contains(err.Error(), "not trusted") {
		t.Fatalf("B's untrusted [[command]] must be refused on B's trust, not A's; got %v", err)
	}
	grantExecTrust(t, b)
	got, err := s.commandResolver(ctx, "gen", "")
	if err != nil {
		t.Fatalf("resolving B's trusted command: %v", err)
	}
	if want := []string{"go", "generate", "./b/..."}; !slices.Equal(got.Argv, want) {
		t.Errorf("argv = %v, want B's %v", got.Argv, want)
	}
}

// A global [[command]] is the user's own and needs no grant anywhere. The
// connection project's [[command]] entries make ITS allow-list project-supplied;
// that provenance must not follow the agent into B, where it would put the
// user's global command behind B's trust (or mislabel it "project").
func TestRunCommand_AgentPinnedElsewhereRunsGlobalCommandsAsGlobal(t *testing.T) {
	s, _, ctx := agentProjectSession(t, separateProject(t, ""))
	base := config.Defaults()
	base.Commands = []config.CommandConfig{{Name: "fmt", Exec: []string{"gofmt", "-l", "."}}}
	s.store = config.NewStore(base)

	got, err := s.commandResolver(ctx, "fmt", "")
	if err != nil {
		t.Fatalf("a global [[command]] was refused for the agent in B: %v", err)
	}
	if got.Provenance != "global" {
		t.Errorf("provenance = %q, want \"global\" — the connection project's provenance leaked across", got.Provenance)
	}
}

// topology_affected spells targets relative to the test command's working_dir,
// and session_start reports the task surface: both must describe the agent's
// project, or they hand it targets and slots for the wrong one.
func TestTaskSurface_AgentPinnedElsewhereDescribesItsOwnProject(t *testing.T) {
	s, _, ctx := agentProjectSession(t, separateProject(t, ""))
	if got := s.testScope(ctx); got.WorkingDir != "" {
		t.Errorf("testScope for the agent carries the connection project's working_dir %q", got.WorkingDir)
	}
	if got := s.taskState(ctx); slices.Contains(got.Commands, "gen") {
		t.Errorf("session_start would list the connection project's [[command]] for the agent: %v", got.Commands)
	}
}

// Positive control: a single-agent connection and an agent pinned to the
// connection's own project both still resolve A's config exactly as before.
func TestRunTask_ConnectionProjectConfigUnchangedForItsOwnCallers(t *testing.T) {
	s, a, agentCtx := agentProjectSession(t, func(a string) string { return a })
	for name, ctx := range map[string]context.Context{
		"unattributed call": context.Background(),
		"agent on A":        agentCtx,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := s.taskResolver(ctx, tools.TaskRequest{Slot: "build"})
			if err != nil {
				t.Fatalf("resolving build: %v", err)
			}
			want := [][]string{{"go", "build", "-v", "./..."}}
			if !slices.EqualFunc(got.Steps, want, slices.Equal) || filepath.Clean(got.WorkingDir) != filepath.Join(a, "sub") {
				t.Errorf("resolved %v in %q, want A's %v in %q", got.Steps, got.WorkingDir, want, filepath.Join(a, "sub"))
			}
			if _, err := s.commandResolver(ctx, "gen", ""); err != nil {
				t.Errorf("A's trusted [[command]] must still resolve: %v", err)
			}
			if got := s.testScope(ctx); got.WorkingDir != "sub" {
				t.Errorf("testScope.WorkingDir = %q, want A's \"sub\"", got.WorkingDir)
			}
		})
	}
}

// A working_dir resolves to a directory that does not exist: the resolver must
// say which setting produced it, so the exec-time refusal can name it (#522).
func TestRunTask_WorkingDirSourceNamesTheSetting(t *testing.T) {
	var b string
	s, _, ctx := agentProjectSession(t, func(a string) string {
		b = separateProject(t, "[tasks.go]\nworking_dir = \"plumb\"\n")(a)
		return b
	})
	grantExecTrust(t, b)
	got, err := s.taskResolver(ctx, tools.TaskRequest{Slot: "build"})
	if err != nil {
		t.Fatalf("resolving build: %v", err)
	}
	want := `[tasks.go] working_dir = "plumb" in ` + config.ProjectConfigPath(b)
	if got.WorkingDirSource != want {
		t.Errorf("WorkingDirSource = %q, want %q", got.WorkingDirSource, want)
	}
}
