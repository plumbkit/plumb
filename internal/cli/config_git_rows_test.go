package cli

import (
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
)

// TestGitConfigRows_CoverEveryRegistryField is the drift guard, and the reason
// this card exists rather than a two-line patch.
//
// `plumb config show` rendered its [git] provenance rows from a hand-written
// literal while internal/config's field registry grew independently. The two
// drifted silently: git.commit_trailer and git.write_timeout were in the
// registry with no row at all. That is worse than cosmetic, because
// session_start's ignored-[git] notice tells the reader to confirm the resolved
// value with exactly this command, and names PLUMB_GIT_COMMIT_TRAILER as one of
// the four variables that can be overriding a trusted project value. One of the
// four could not be confirmed the way the notice prescribes.
//
// Nothing here stops the literal drifting again; this test does. Adding a git.*
// field to the registry without a row fails it, and the message says what to do.
func TestGitConfigRows_CoverEveryRegistryField(t *testing.T) {
	rows := gitConfigRows(config.Defaults(), config.Defaults(), config.Defaults(), config.ProjectPolicyStatus{})

	rendered := make(map[string]bool, len(rows))
	for _, r := range rows {
		rendered[r[0]] = true
	}

	for _, f := range config.Registry() {
		name, ok := strings.CutPrefix(f.Key, "git.")
		if !ok {
			continue
		}
		if !rendered[name] {
			t.Errorf("config field %q is in the registry but `plumb config show` prints no [git] provenance row for it.\n"+
				"Every git.* field must be showable: session_start's ignored-[git] notice sends readers to this command "+
				"to confirm which layer won, so a missing row makes that instruction unfollowable.\n"+
				"Add a row to gitConfigRows in internal/cli/config.go (and an envVarFor case if it has a PLUMB_GIT_* variable).", f.Key)
		}
	}
}

// TestGitConfigRows_NameTheirEnvVars is the second half of the notice's promise.
//
// A row that exists but cannot name the variable overriding it is no more useful
// than no row: the notice's whole point is telling the reader WHICH layer won.
// envVarFor was missing a commit_trailer case, so the command could not have
// named PLUMB_GIT_COMMIT_TRAILER even with a row present.
func TestGitConfigRows_NameTheirEnvVars(t *testing.T) {
	// The variables applyGitEnv actually reads (internal/config/config_load.go).
	want := map[string]string{
		"allow_writes":      "PLUMB_GIT_ALLOW_WRITES",
		"allow_destructive": "PLUMB_GIT_ALLOW_DESTRUCTIVE",
		"allow_push":        "PLUMB_GIT_ALLOW_PUSH",
		"commit_trailer":    "PLUMB_GIT_COMMIT_TRAILER",
		"write_timeout":     "PLUMB_GIT_WRITE_TIMEOUT",
	}
	for field, env := range want {
		if got := envVarForField(field); got != env {
			t.Errorf("envVarForField(%q) = %q, want %q — `plumb config show` cannot name the variable "+
				"that overrode this field, which is the one thing the ignored-[git] notice asks it to do", field, got, env)
		}
	}
}

// TestGitConfigRows_EnvVarWins proves the env column is live rather than
// decorative: with PLUMB_GIT_COMMIT_TRAILER set, the row's source must say so.
//
// This is the exact check the #294 notice prescribes, run end to end.
func TestGitConfigRows_EnvVarWins(t *testing.T) {
	t.Setenv("PLUMB_GIT_COMMIT_TRAILER", "true")

	rows := gitConfigRows(config.Defaults(), config.Defaults(), config.Defaults(), config.ProjectPolicyStatus{})
	for _, r := range rows {
		if r[0] != "commit_trailer" {
			continue
		}
		if !strings.Contains(r[2], "PLUMB_GIT_COMMIT_TRAILER") {
			t.Fatalf("commit_trailer's source column is %q; it must name PLUMB_GIT_COMMIT_TRAILER when that "+
				"variable is what won, or the notice's instruction to confirm it here cannot be followed", r[2])
		}
		return
	}
	t.Fatal("no commit_trailer row at all")
}
