package cli

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
)

// TestResolveWorkspaceHint pins the resolution order for the serve workspace
// pre-pin: --workspace flag > PLUMB_WORKSPACE env > nothing. There is
// deliberately no serve-cwd fallback (PLAN-350): cwd is not intent — an MCP
// client spawns serve from its own launcher's directory — so with neither set
// the serve starts unattached and session_start pins the workspace. Blank
// values are treated as unset, and explicit values are $VAR-expanded and made
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
		want      string
	}{
		{"flag beats env", "/flag/ws", "/env/ws", "/flag/ws"},
		{"env when no flag", "", "/env/ws", "/env/ws"},
		{"nothing set means unattached — no cwd fallback", "", "", ""},
		{"blank flag falls through to env", "   ", "/env/ws", "/env/ws"},
		{"blank flag and env stay unset", " ", "\t", ""},
		{"flag is $VAR-expanded", "$PLUMB_WORKSPACE_TESTVAR/sub", "", "/expanded/ws/sub"},
		{"relative flag is made absolute", "rel/ws", "", filepath.Join(abs, "rel/ws")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveWorkspaceHint(tc.flag, tc.env); got != tc.want {
				t.Fatalf("resolveWorkspaceHint(%q, %q) = %q, want %q",
					tc.flag, tc.env, got, tc.want)
			}
		})
	}
}

// TestServeWorkspaceFlag_InjectsFlagPath is the acceptance check for the
// --workspace flag: the value the initialize frame carries under
// mcp.MetaWorkspaceKey is the resolved flag value — the pre-pin the daemon's
// attachFromHint attaches from.
func TestServeWorkspaceFlag_InjectsFlagPath(t *testing.T) {
	t.Parallel()
	const flag = "/pinned/project"

	resolved := resolveWorkspaceHint(flag, "")
	if resolved != flag {
		t.Fatalf("resolveWorkspaceHint = %q, want the flag value %q", resolved, flag)
	}

	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	out := injectInitMeta(frame, buildInitMeta(nil, "", resolved))

	var ws string
	if err := json.Unmarshal(initMeta(t, out)[mcp.MetaWorkspaceKey], &ws); err != nil {
		t.Fatalf("workspace pre-pin: %v", err)
	}
	if ws != flag {
		t.Fatalf("injected workspace pre-pin = %q, want %q", ws, flag)
	}
}

// TestServeWorkspaceHint_EnvFallbackInjected covers the middle rung: with no
// flag, PLUMB_WORKSPACE (resolved by runServe into the proxy's workspace field)
// is what the frame carries — the env var behaves exactly like the flag: an
// explicit pre-pin, not an implicit hint.
func TestServeWorkspaceHint_EnvFallbackInjected(t *testing.T) {
	t.Parallel()
	const env = "/env/pinned"

	resolved := resolveWorkspaceHint("", env)
	out := injectInitMeta([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`),
		buildInitMeta(nil, "", resolved))

	var ws string
	if err := json.Unmarshal(initMeta(t, out)[mcp.MetaWorkspaceKey], &ws); err != nil {
		t.Fatalf("workspace pre-pin: %v", err)
	}
	if ws != env {
		t.Fatalf("injected workspace pre-pin = %q, want %q", ws, env)
	}
}

// TestServeWorkspaceHint_NoFlagNoEnvStartsUnattached is the acceptance check
// for the unattached default (PLAN-350): with neither flag nor env, the serve
// process's working directory is NEVER transported — the frame carries no
// workspace key, so the daemon has no hint to auto-attach from and
// session_start stays the sole workspace-pin authority. This test replaces the
// old TestServeWorkspaceHint_NoFlagKeepsCwd, which pinned the cwd auto-attach
// this card removes.
func TestServeWorkspaceHint_NoFlagNoEnvStartsUnattached(t *testing.T) {
	t.Parallel()

	// The test process has a real working directory — exactly what the old
	// behaviour attached from. It must not be transported.
	if resolved := resolveWorkspaceHint("", ""); resolved != "" {
		t.Fatalf("resolveWorkspaceHint with neither flag nor env = %q, want \"\" (cwd is not a pre-pin)", resolved)
	}
	out := injectInitMeta([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`),
		buildInitMeta(nil, "", ""))
	if _, ok := initMeta(t, out)[mcp.MetaWorkspaceKey]; ok {
		t.Fatal("workspace key transported with no flag and no env — the serve must start unattached")
	}
}
