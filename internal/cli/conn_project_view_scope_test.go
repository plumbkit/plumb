package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/tools"
)

// The edges of projectViewFor (#522): a config that will not parse, the window
// inside a connection re-pin, the boundary an agent's own pin builds, and
// agent_config writing the project the calling agent is actually in.

// A malformed agent config resolves to the GLOBAL config, as applyProjectConfig
// does, never to the connection's project: A's trusted `gen` must not resolve
// for an agent whose own project ships TOML that does not parse.
func TestProjectView_MalformedAgentConfigNeverBorrowsTheConnections(t *testing.T) {
	s, _, ctx := agentProjectSession(t, separateProject(t, "[[command]\nname=\"gen\"\n"))

	if got, err := s.commandResolver(ctx, "gen", ""); err == nil {
		t.Fatalf("the connection project's trusted `gen` resolved for an agent whose own config is malformed: %+v", got)
	}
	if got, err := s.taskResolver(ctx, tools.TaskRequest{Slot: "build"}); err == nil {
		t.Fatalf("run_task resolved for an agent whose own config is malformed: %+v", got)
	}
}

// A connection re-pin moves acquiredRoot before applyProjectConfig swaps the new
// root's config in. A call in that window must not get the new root paired with
// the OLD project's allow-list and trust. The window is reproduced exactly: the
// view is loaded for A, and acquiredRoot already names B.
func TestProjectView_RepinWindowDoesNotPairNewRootWithOldConfig(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	a := t.TempDir()
	writeExecProject(t, a, connProjectA)
	grantExecTrust(t, a)
	s := execTrustSession(t, a)
	if _, err := s.commandResolver(context.Background(), "gen", ""); err != nil {
		t.Fatalf("precondition: A's trusted gen must resolve on A: %v", err)
	}

	b := t.TempDir()
	s.mutate(func(v *sessionView) { v.acquiredRoot = b })
	if got, err := s.commandResolver(context.Background(), "gen", ""); err == nil {
		t.Fatalf("mid re-pin, A's trusted `gen` resolved against B: %+v", got)
	}
	s.applyProjectConfig(b)
	if _, err := s.commandResolver(context.Background(), "gen", ""); err == nil {
		t.Fatal("after the re-pin settles, B (no [[command]]) must still have no `gen`")
	}
}

// The boundary an agent's pin builds is its own root's. Project [workspace]
// roots are ClassForcedGlobal, so no project config widens any boundary; the
// per-project widening is the roots the USER granted for a root (TUI/CLI), and
// those must follow the agent's root in both directions.
func TestAgentBoundary_GrantedRootsFollowTheAgentsRoot(t *testing.T) {
	scratch := t.TempDir()
	mk := func(name string) string {
		d := filepath.Join(scratch, name)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		return d
	}
	projectRoot, grantedA, grantedB := mk("project-root"), mk("granted-a"), mk("granted-b")
	connCfg := connProjectA + "\n[workspace]\nextra_roots = [\"" + projectRoot + "\"]\n"

	var a string
	s, _, ctx := agentProjectSession(t, func(connRoot string) string {
		a = connRoot
		writeExecProject(t, a, connCfg)
		grantExecTrust(t, a)
		b := t.TempDir()
		mustGitDir(t, b)
		roots := config.NewWorkspaceRootsStore()
		if err := roots.SetExtraRoots(a, []string{grantedA}); err != nil {
			t.Fatal(err)
		}
		if err := roots.SetExtraRoots(b, []string{grantedB}); err != nil {
			t.Fatal(err)
		}
		return b
	})
	s.applyProjectConfig(a) // rebuild the connection's policy with A's grant

	agent := s.policyFor(ctx)
	for _, refused := range []string{grantedA, projectRoot} {
		if _, err := agent.Check(filepath.Join(refused, "f.go"), tools.AccessRead); err == nil {
			t.Errorf("the agent in B may read %s, a root granted to (or configured by) the connection's project", refused)
		}
	}
	if _, err := agent.Check(filepath.Join(grantedB, "f.go"), tools.AccessReadWrite); err != nil {
		t.Errorf("the agent in B is refused the root the user granted B: %v", err)
	}
	if _, err := s.policyFor(context.Background()).Check(filepath.Join(grantedA, "f.go"), tools.AccessReadWrite); err != nil {
		t.Errorf("positive control: the connection on A is refused A's granted root: %v", err)
	}
}

// agent_config writes the calling agent's project. An agent in B that set
// tasks.go.lint used to rewrite A's config, leaving its own run_task unchanged.
func TestAgentConfig_WritesTheCallingAgentsProject(t *testing.T) {
	var b string
	s, a, ctx := agentProjectSession(t, func(connRoot string) string {
		b = separateProject(t, "")(connRoot)
		return b
	})
	before := s.view().tasks["go"].Lint

	if _, err := s.applyAgentConfig(ctx, map[string]any{"tasks.go.lint": "go vet ./..."}); err != nil {
		t.Fatalf("agent_config from the agent in B: %v", err)
	}
	if body := readProjectConfig(t, b); !strings.Contains(body, "go vet ./...") {
		t.Errorf("B's config does not carry the agent's write:\n%s", body)
	}
	if body := readProjectConfig(t, a); strings.Contains(body, "go vet ./...") {
		t.Errorf("the agent's write landed in the connection's project A:\n%s", body)
	}
	if got := s.view().tasks["go"].Lint; got != before {
		t.Errorf("the connection's view changed (%q -> %q) for a write to another project", before, got)
	}
	v, _ := s.projectViewFor(ctx)
	if got := v.tasks["go"].Lint; got != "go vet ./..." {
		t.Errorf("the agent's own view lint = %q, want its write", got)
	}

	// Positive control: the connection's own caller still writes A, live.
	if _, err := s.applyAgentConfig(context.Background(), map[string]any{"tasks.go.lint": "go vet -v ./..."}); err != nil {
		t.Fatalf("agent_config from the connection's caller: %v", err)
	}
	if got := s.view().tasks["go"].Lint; got != "go vet -v ./..." {
		t.Errorf("the connection's write to A is not live in its view: lint = %q", got)
	}
}

func readProjectConfig(t *testing.T, ws string) string {
	t.Helper()
	body, err := os.ReadFile(config.ProjectConfigPath(ws))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(body)
}
