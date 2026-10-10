package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/contexthints"
)

func TestSnakeEvent(t *testing.T) {
	for in, want := range map[string]string{"UserPromptSubmit": "user_prompt_submit", "SubagentStart": "subagent_start", "Stop": "stop"} {
		if got := snakeEvent(in); got != want {
			t.Errorf("snakeEvent(%q) = %q, want %q", in, got, want)
		}
	}
}

// Codex's trust record is read from its own config.toml, keyed by the hooks
// file, the snake-cased event and the handler position; only the hint events
// count, and an approval for another hooks file does not.
func TestCodexContextTrust(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	hooks := filepath.Join(home, "hooks.json")
	body := `[hooks.state."` + hooks + `:user_prompt_submit:0:0"]
trusted_hash = "sha256:aa"

[hooks.state."` + hooks + `:session_start:0:0"]
trusted_hash = "sha256:bb"

[hooks.state."/elsewhere/hooks.json:subagent_start:0:0"]
trusted_hash = "sha256:cc"

[hooks.state."/elsewhere/hooks.json:user_prompt_submit:0:0"]
trusted_hash = "sha256:dd"
`
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// Another hooks file approves both hint events; only this file's own
	// UserPromptSubmit approval may count.
	if got := codexContextTrust(hooks); !strings.HasPrefix(got, "1/2") {
		t.Errorf("trust = %q, want 1/2 (UserPromptSubmit, this file only)", got)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("not = [valid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := codexContextTrust(hooks); got != "unknown" {
		t.Errorf("unreadable config = %q, want unknown", got)
	}
}

func TestRenderUptake(t *testing.T) {
	out := renderUptake(uptakeReport{
		Since:   time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC),
		Clients: []uptakeClient{{Client: "claude-code", Configured: "2/2"}, {Client: "codex", Configured: "0/2", Trusted: "0/2 (approval recorded; hash not re-verified)"}},
		Hosts: []contexthints.HostUptake{{
			Host: "claude-code", Invoked: 5, EmittedBytes: 220, Consumed: 2, Unconsumed: 1, Agents: 3,
			ByOutcome: map[contexthints.Outcome]int{contexthints.OutcomeEmitted: 3, contexthints.OutcomeNoop: 1, contexthints.OutcomeThrottled: 1},
			ByDetail:  map[string]int{"noop:no-seeds": 1, "throttled:budget": 1},
		}},
		Evicted: 7,
	})
	for _, want := range []string{
		"claude-code  configured 2/2", "trusted 0/2", "5 invoked, 3 emitted (220 B)",
		"consumed 2, not consumed 1, unattributed 0", "noop:no-seeds", "7 older observation(s) were evicted",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(renderUptake(uptakeReport{}), "No hint requests recorded") {
		t.Error("an empty report did not say so")
	}
}
