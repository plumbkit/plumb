//go:build clients_e2e

package clientsmoke

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// authTimeout bounds a single headless LLM-driven prompt. Generous — a cold
// agent plus a model round-trip plus a plumb tool call.
const authTimeout = 180 * time.Second

// toolPrompt forces the agent to invoke a PATH-BEARING plumb tool. plumb only
// records a tool call in stats once it resolves a workspace root, and it derives
// that root from a path argument (seedPathFromArgs) — so a path-less call like
// `version` on a connection that never attached a workspace leaves no row. A
// list_directory on the fixture's absolute path always resolves the root, making
// the stats signal reliable regardless of how the model paraphrases the output.
func toolPrompt(dir string) string {
	return "Use the plumb MCP server's list_directory tool to list the directory \"" + dir +
		"\". Call that plumb tool with exactly that path. Do not use any other tool or answer from memory."
}

// TestClientsAuth is the LLM AUTH tier. For each client whose API key is present
// in the environment, it drives a headless prompt that forces a plumb tool call
// and asserts plumb's stats DB recorded ≥1 tool call — proof the agent invoked a
// plumb tool through the model. Clients whose key is unset are skipped, so the
// tier runs only what the current environment can pay for.
//
// Run with the relevant keys exported, e.g.:
//
//	OPENAI_API_KEY=…  ANTHROPIC_API_KEY=…  GEMINI_API_KEY=…  make clients-test-auth
func TestClientsAuth(t *testing.T) {
	for _, spec := range clientSpecs() {
		t.Run(spec.name, func(t *testing.T) {
			if len(spec.authKeys) == 0 || spec.promptArgs == nil {
				t.Skipf("%s has no auth-tier configuration", spec.name)
			}
			keyName, keyVal, ok := firstSetEnv(spec.authKeys)
			if !ok {
				t.Skipf("no API key for %s — set one of: %s", spec.name, strings.Join(spec.authKeys, ", "))
			}
			if _, err := exec.LookPath(spec.binary); err != nil {
				t.Skipf("%s not installed (%q not on PATH) — run scripts/install-clients.sh", spec.name, spec.binary)
			}

			tmpHome := mkTmpHome(t)
			fixture := makeBareFixture(t)
			env := isolatedEnv(tmpHome)
			if spec.authEnv != nil {
				env = append(env, spec.authEnv(keyVal)...)
			}
			if spec.probeEnv != nil {
				env = append(env, spec.probeEnv(realHome)...)
			}
			t.Cleanup(func() { stopDaemon(env) })

			runPlumbSetup(t, env, spec.setupArgs...)
			if spec.prep != nil {
				spec.prep(t, tmpHome, fixture, env)
			}

			args := spec.promptArgs(toolPrompt(fixture))
			ctx, cancel := context.WithTimeout(context.Background(), authTimeout)
			defer cancel()
			cmd := exec.CommandContext(ctx, spec.binary, args...)
			cmd.Env = env
			cmd.Dir = fixture
			out, runErr := cmd.CombinedOutput()
			t.Logf("auth via %s\n$ %s %s  (exit=%v)\n%s",
				keyName, spec.binary, strings.Join(args, " "), runErr, truncate(out, 3000))

			// plumb's stats Writer is async/batched; a graceful daemon stop drains it
			// (stats.TestWriter_DrainsOnClose), making the tool_calls row durable before
			// we read — otherwise the row can lag the client's exit and read as 0.
			stopDaemon(env)
			n, tools := pollToolCalls(t, tmpHome, 8*time.Second)
			if n == 0 {
				t.Fatalf("FAIL %s: agent ran but plumb recorded no tool call — the model did not invoke a plumb tool.\noutput:\n%s",
					spec.name, truncate(out, 3000))
			}
			t.Logf("PASS %s: plumb recorded %d tool call(s) [%s]", spec.name, n, tools)
		})
	}
}

// pollToolCalls reads the stats DB until a tool_calls row appears or timeout
// elapses, absorbing the small lag between the daemon stop and the WAL becoming
// visible to a fresh read-only handle.
func pollToolCalls(t *testing.T, tmpHome string, timeout time.Duration) (int, string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		n, tools := countToolCalls(t, tmpHome)
		if n > 0 || time.Now().After(deadline) {
			return n, tools
		}
		time.Sleep(300 * time.Millisecond)
	}
}
