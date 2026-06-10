//go:build clients_e2e

package clientsmoke

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// gutterLeak matches a residual display line-number gutter ("<n>\t" at line
// start) — the artifact that would land in the file if a client pasted guttered
// read_file output into edit_file's old_string AND plumb's forgiveness failed to
// strip it. The whole point of gutter forgiveness is that this never appears.
var gutterLeak = regexp.MustCompile(`(?m)^\s*\d+\t`)

// gutterPrompt drives the read→edit round-trip: read a guttered file through
// plumb, then edit one line through plumb. A client that pastes the gutter into
// old_string exercises plumb's forgiveness; the on-disk result proves whether it
// worked, independent of how the model narrates.
func gutterPrompt(file string) string {
	return "Use the plumb MCP server's read_file tool to read the file \"" + file + "\". " +
		"Then use the plumb edit_file tool to change the word ALPHA to OMEGA in that file. " +
		"Use only plumb tools; do not answer from memory or use any other editor."
}

// TestClientsAuth_GutterRoundTrip is an AUTH-tier scenario that validates the
// line-number gutter end to end through a real client: the agent reads a
// guttered file and edits it, both via plumb. The assertion is on the FILE
// CONTENT — the edit applied AND no gutter prefix leaked — which catches both
// "the client stripped the gutter itself" and "plumb's forgiveness caught it".
// Same per-client key gating and nondeterminism caveats as TestClientsAuth.
func TestClientsAuth_GutterRoundTrip(t *testing.T) {
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
			target := filepath.Join(fixture, "target.txt")
			original := "one\ntwo\nALPHA marker line\nfour\nfive\n"
			if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
				t.Fatal(err)
			}

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

			args := spec.promptArgs(gutterPrompt(target))
			ctx, cancel := context.WithTimeout(context.Background(), authTimeout)
			defer cancel()
			cmd := exec.CommandContext(ctx, spec.binary, args...)
			cmd.Env = env
			cmd.Dir = fixture
			out, runErr := cmd.CombinedOutput()
			t.Logf("gutter round-trip via %s\n$ %s %s  (exit=%v)\n%s",
				keyName, spec.binary, strings.Join(args, " "), runErr, truncate(out, 3000))

			stopDaemon(env)

			final, err := os.ReadFile(target)
			if err != nil {
				t.Fatalf("reading target back: %v", err)
			}
			body := string(final)
			switch {
			case gutterLeak.MatchString(body):
				t.Fatalf("FAIL %s: a line-number gutter leaked into the file — forgiveness did not strip a pasted gutter:\n%s",
					spec.name, body)
			case body == original:
				// The model may simply not have performed the edit (auth tier is
				// nondeterministic); that is a non-result, not a gutter failure.
				t.Skipf("inconclusive %s: file unchanged — the model did not perform the edit this run", spec.name)
			case strings.Contains(body, "OMEGA") && !strings.Contains(body, "ALPHA"):
				t.Logf("PASS %s: edit applied through the gutter with no leak", spec.name)
			default:
				t.Fatalf("FAIL %s: file changed but did not complete the requested ALPHA→OMEGA replacement:\n%s", spec.name, body)
			}
		})
	}
}
