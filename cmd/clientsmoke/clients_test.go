//go:build clients

package clientsmoke

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// connectTimeout bounds a single client's connecting probe. A probe that hangs
// (a known cursor-agent failure mode) is killed at this deadline; the assertion
// then checks whether plumb saw the connection before the hang.
const connectTimeout = 45 * time.Second

// TestClientsConnect is the auth-free CONNECTION tier. For each client with an
// auth-free connecting probe, it confirms the CLI completes the MCP initialize
// handshake with plumb — asserted on plumb's own session file, independent of
// the CLI's output format. No API keys; deterministic. Clients without such a
// probe (codex/crush/goose/auggie) and uninstalled clients are skipped.
func TestClientsConnect(t *testing.T) {
	for _, spec := range clientSpecs() {
		t.Run(spec.name, func(t *testing.T) {
			if !spec.connect {
				t.Skipf("no auth-free connection probe: %s", spec.connectSkip)
			}
			if _, err := exec.LookPath(spec.binary); err != nil {
				t.Skipf("%s not installed (%q not on PATH) — run scripts/install-clients.sh", spec.name, spec.binary)
			}

			tmpHome := mkTmpHome(t)
			fixture := makeBareFixture(t)
			env := isolatedEnv(tmpHome)
			t.Cleanup(func() { stopDaemon(env) })

			runPlumbSetup(t, env, spec.setupArgs...)
			if spec.prep != nil {
				spec.prep(t, tmpHome, fixture, env)
			}

			ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
			defer cancel()
			cmd := exec.CommandContext(ctx, spec.binary, spec.connectArgs...)
			cmd.Env = env
			if spec.probeEnv != nil {
				cmd.Env = append(cmd.Env, spec.probeEnv(realHome)...)
			}
			cmd.Dir = fixture
			out, runErr := cmd.CombinedOutput()
			t.Logf("$ %s %s  (exit=%v)\n%s", spec.binary, strings.Join(spec.connectArgs, " "), runErr, truncate(out, 2000))

			sess, ok := findClientSession(t, tmpHome)
			if !ok {
				t.Fatalf("FAIL %s: plumb recorded no client session — %q did not complete an MCP handshake with plumb",
					spec.name, spec.binary)
			}
			t.Logf("PASS %s: plumb saw client_name=%q version=%q (session %s)",
				spec.name, sess.ClientName, sess.ClientVersion, sess.ID)

			for _, w := range spec.wantOut {
				if !strings.Contains(string(out), w) {
					t.Logf("note: probe output did not contain %q (primary session signal still passed)", w)
				}
			}
		})
	}
}
