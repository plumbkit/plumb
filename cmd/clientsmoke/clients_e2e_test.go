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

// toolPrompt forces the agent to invoke a plumb tool. `version` is read-only,
// fast, and side-effect-free, yet still lands a tool_calls row in stats.db.
const toolPrompt = "Use the plumb MCP server's \"version\" tool to report plumb's version. " +
	"You must call the tool — do not answer from memory or use any other tool."

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

			args := spec.promptArgs(toolPrompt)
			ctx, cancel := context.WithTimeout(context.Background(), authTimeout)
			defer cancel()
			cmd := exec.CommandContext(ctx, spec.binary, args...)
			cmd.Env = env
			cmd.Dir = fixture
			out, runErr := cmd.CombinedOutput()
			t.Logf("auth via %s\n$ %s %s  (exit=%v)\n%s",
				keyName, spec.binary, strings.Join(args, " "), runErr, truncate(out, 3000))

			n, tools := countToolCalls(t, tmpHome)
			if n == 0 {
				t.Fatalf("FAIL %s: agent ran but plumb recorded no tool call — the model did not invoke a plumb tool.\noutput:\n%s",
					spec.name, truncate(out, 3000))
			}
			t.Logf("PASS %s: plumb recorded %d tool call(s) [%s]", spec.name, n, tools)
		})
	}
}
