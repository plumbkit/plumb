package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/toolerror"
)

// TestGitCommandError_RenderedTextIsPinned is the byte-for-byte guarantee that
// folding the git failure report into the structured envelope changed nothing
// an agent reads. The expectation is assembled here from first principles —
// not from gitExitDescription/boundGitErrStream/gitRerunNote — so the pin fails
// if the rendering drifts, rather than drifting along with it.
//
// The fixture exercises every clause at once: an exit code, a labelled stderr,
// a labelled stdout truncated by the 40-line cap, and the re-run note.
func TestGitCommandError_RenderedTextIsPinned(t *testing.T) {
	var stdout strings.Builder
	for i := 1; i <= 50; i++ {
		fmt.Fprintf(&stdout, "line %d\n", i)
	}
	var wantStdout []string
	for i := 11; i <= 50; i++ { // the last 40 of 50
		wantStdout = append(wantStdout, fmt.Sprintf("line %d", i))
	}
	want := "git commit: exit code 1\n" +
		"stderr:\nhook: lint failed\n" +
		"stdout (last 40 lines):\n" + strings.Join(wantStdout, "\n") + "\n" +
		"… (output truncated — re-run `git commit -m 'hook fails'` in /r for the complete output)"

	err := gitCommandError("/r", "commit", []string{"commit", "-m", "hook fails"},
		exitError(t), stdout.String(), "hook: lint failed\n", "")

	if got := err.Error(); got != want {
		t.Errorf("rendered text drifted.\n got: %q\nwant: %q", got, want)
	}
}

func TestGitCommandError_Classification(t *testing.T) {
	tests := []struct {
		name        string
		runErr      error
		stdout      string
		wantDetails map[string]string
	}{
		{
			name:        "clean exit code, nothing truncated",
			runErr:      exitError(t),
			wantDetails: map[string]string{"subcommand": "push", "exit_code": "1", "output_truncated": "false"},
		},
		{
			name:        "truncated output is flagged",
			runErr:      exitError(t),
			stdout:      strings.Repeat("noise\n", 100),
			wantDetails: map[string]string{"subcommand": "push", "exit_code": "1", "output_truncated": "true"},
		},
		{
			name:        "no exit code when the child never ran",
			runErr:      context.Canceled,
			wantDetails: map[string]string{"subcommand": "push", "output_truncated": "false"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := gitCommandError("/r", "push", []string{"push"}, tt.runErr, tt.stdout, "", "")
			assertClassified(t, err, toolerror.KindGitCommandFailed, toolerror.ClassInspectOutput, false)
			te := mustClassify(t, err)
			if len(te.Details) != len(tt.wantDetails) {
				t.Fatalf("Details = %v, want %v", te.Details, tt.wantDetails)
			}
			for k, v := range tt.wantDetails {
				if te.Details[k] != v {
					t.Errorf("Details[%q] = %q, want %q", k, te.Details[k], v)
				}
			}
			// Bounded output belongs in the message, never in Details.
			for k, v := range te.Details {
				if strings.Contains(v, "noise") {
					t.Errorf("Details[%q] carries captured output: %q", k, v)
				}
			}
		})
	}
}

func TestGateGit_Classified(t *testing.T) {
	off := GitPolicy{}
	on := GitPolicy{AllowWrites: true, AllowDestructive: true, AllowPush: true}

	tests := []struct {
		name      string
		tier      gitTier
		policy    GitPolicy
		confirm   bool
		wantClass toolerror.RemediationClass
		wantRetry bool
	}{
		{"writes disabled", tierWrite, off, false, toolerror.ClassEnablePolicy, false},
		{"destructive disabled", tierDestructive, off, true, toolerror.ClassEnablePolicy, false},
		{"destructive needs confirm", tierDestructive, on, false, toolerror.ClassPassConfirm, true},
		{"network disabled", tierNetwork, off, true, toolerror.ClassEnablePolicy, false},
		{"network needs confirm", tierNetwork, on, false, toolerror.ClassPassConfirm, true},
		{"unknown tier", gitTier(99), on, true, toolerror.ClassNone, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := gateGit(tt.tier, tt.policy, tt.confirm)
			assertClassified(t, err, toolerror.KindGitPolicy, tt.wantClass, tt.wantRetry)
		})
	}

	if err := gateGit(tierRead, off, false); err != nil {
		t.Errorf("the read tier must stay ungated, got %v", err)
	}
}

func TestCheckPushProtection_Classified(t *testing.T) {
	protected := GitPolicy{AllowPush: true, ProtectedBranches: []string{"main"}}

	tests := []struct {
		name string
		args gitToolArgs
	}{
		{"ad-hoc remote", gitToolArgs{Subcommand: "fetch", Args: []string{"ext::sh -c whoami"}}},
		{"protected branch force push", gitToolArgs{Subcommand: "push", Args: []string{"-f", "origin", "main"}}},
		{"force push with no destination", gitToolArgs{Subcommand: "push", Args: []string{"--force"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkPushProtection(tt.args, protected, tierNetwork)
			assertClassified(t, err, toolerror.KindGitPolicy, toolerror.ClassNone, false)
		})
	}
}

func TestBeginSerialisedGit_DrainRefusalClassified(t *testing.T) {
	gitWriteDraining.Store(true)
	t.Cleanup(func() { gitWriteDraining.Store(false) })

	_, _, err := beginSerialisedGit(t.Context(), t.TempDir(), "commit", tierWrite)
	assertClassified(t, err, toolerror.KindDaemonTransport, toolerror.ClassRetryAfterWait, true)
	if !errors.Is(err, errGitDraining) {
		t.Error("the drain sentinel is no longer reachable through the classification")
	}
}

func TestGitRefGuard_RefusalsClassified(t *testing.T) {
	requireGit(t)
	ctx := t.Context()

	t.Run("expected_head mismatch advises fixing the argument, not confirming", func(t *testing.T) {
		repo := initTestRepo(t)
		g := &gitRefGuard{repoRoot: repo, expectedHead: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", check: true, confirm: true}
		err := g.preExec(ctx, "commit")
		// confirm: true is set and STILL refuses — which is why pass_confirm
		// would be the wrong advice here.
		assertClassified(t, err, toolerror.KindConcurrentRefMove, toolerror.ClassFixArguments, true)
	})

	t.Run("peer ref movement advises confirming", func(t *testing.T) {
		repo := initTestRepo(t)
		cur, ok := observeGitRef(ctx, repo)
		if !ok {
			t.Fatal("could not observe the fresh repository's HEAD")
		}
		st := gitRefStateFor(repo)
		st.record("sess-self", "self", gitRefObservation{head: "0000000", branch: cur.branch}, false)
		st.record("sess-peer", "peer", cur, true)

		g := &gitRefGuard{repoRoot: repo, sessID: "sess-self", check: true}
		err := g.preExec(ctx, "commit")
		assertClassified(t, err, toolerror.KindConcurrentRefMove, toolerror.ClassPassConfirm, true)
	})
}
