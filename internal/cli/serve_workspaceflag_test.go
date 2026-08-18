package cli

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
)

// TestResolveWorkspaceHint pins the resolution order for the serve workspace
// attach hint: --workspace flag > PLUMB_WORKSPACE env > serve cwd > no hint,
// with blank values treated as unset and explicit values $VAR-expanded and made
// absolute (the same normalisation resolveAllowDirs applies).
func TestResolveWorkspaceHint(t *testing.T) {
	t.Setenv("PLUMB_WORKSPACE_TESTVAR", "/expanded/ws")
	abs, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name      string
		flag, env string
		cwd, want string
	}{
		{"flag beats env and cwd", "/flag/ws", "/env/ws", "/cwd/ws", "/flag/ws"},
		{"env beats cwd", "", "/env/ws", "/cwd/ws", "/env/ws"},
		{"cwd when neither is set", "", "", "/cwd/ws", "/cwd/ws"},
		{"nothing set means no hint", "", "", "", ""},
		{"blank flag falls through to env", "   ", "/env/ws", "/cwd/ws", "/env/ws"},
		{"blank flag and env fall through to cwd", " ", "\t", "/cwd/ws", "/cwd/ws"},
		{"flag is $VAR-expanded", "$PLUMB_WORKSPACE_TESTVAR/sub", "", "", "/expanded/ws/sub"},
		{"relative flag is made absolute", "rel/ws", "", "", filepath.Join(abs, "rel/ws")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveWorkspaceHint(tc.flag, tc.env, tc.cwd); got != tc.want {
				t.Fatalf("resolveWorkspaceHint(%q, %q, %q) = %q, want %q",
					tc.flag, tc.env, tc.cwd, got, tc.want)
			}
		})
	}
}

// TestServeWorkspaceFlag_InjectsFlagPathOverCwd is the acceptance check for
// the --workspace flag: with the serve process's working directory pointing
// elsewhere, the value the initialize frame carries under
// mcp.MetaWorkspaceKey is the resolved flag value — that is the hint the
// daemon's attachFromHint will attach from.
func TestServeWorkspaceFlag_InjectsFlagPathOverCwd(t *testing.T) {
	t.Parallel()
	const serveCwd = "/clients/launch/dir"
	const flag = "/pinned/project"

	resolved := resolveWorkspaceHint(flag, "", serveCwd)
	if resolved != flag {
		t.Fatalf("resolveWorkspaceHint = %q, want the flag value %q", resolved, flag)
	}

	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	out := injectInitMeta(frame, buildInitMeta(nil, "", resolved))

	var cwd string
	if err := json.Unmarshal(initMeta(t, out)[mcp.MetaWorkspaceKey], &cwd); err != nil {
		t.Fatalf("workspace hint: %v", err)
	}
	if cwd != flag {
		t.Fatalf("injected workspace hint = %q, want %q (the flag must beat the cwd %q)", cwd, flag, serveCwd)
	}
}

// TestServeWorkspaceHint_EnvFallbackInjected covers the middle rung: with no
// flag, PLUMB_WORKSPACE (resolved by runServe into the proxy's cwd field) is
// what the frame carries, not the serve cwd.
func TestServeWorkspaceHint_EnvFallbackInjected(t *testing.T) {
	t.Parallel()
	const serveCwd = "/clients/launch/dir"
	const env = "/env/pinned"

	resolved := resolveWorkspaceHint("", env, serveCwd)
	out := injectInitMeta([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`),
		buildInitMeta(nil, "", resolved))

	var cwd string
	if err := json.Unmarshal(initMeta(t, out)[mcp.MetaWorkspaceKey], &cwd); err != nil {
		t.Fatalf("workspace hint: %v", err)
	}
	if cwd != env {
		t.Fatalf("injected workspace hint = %q, want %q", cwd, env)
	}
}

// TestServeWorkspaceHint_NoFlagKeepsCwd guards against a regression of the
// historical behaviour: with neither flag nor env, the injected hint is the
// serve cwd, byte-for-byte as before.
func TestServeWorkspaceHint_NoFlagKeepsCwd(t *testing.T) {
	t.Parallel()
	const serveCwd = "/clients/launch/dir"

	resolved := resolveWorkspaceHint("", "", serveCwd)
	if resolved != serveCwd {
		t.Fatalf("resolveWorkspaceHint = %q, want the cwd %q", resolved, serveCwd)
	}
	out := injectInitMeta([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`),
		buildInitMeta(nil, "", resolved))

	var cwd string
	if err := json.Unmarshal(initMeta(t, out)[mcp.MetaWorkspaceKey], &cwd); err != nil {
		t.Fatalf("workspace hint: %v", err)
	}
	if cwd != serveCwd {
		t.Fatalf("injected workspace hint = %q, want the cwd %q", cwd, serveCwd)
	}
}
