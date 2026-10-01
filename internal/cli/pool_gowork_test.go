package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
)

// goWorkWorktreeFixture lays out the shape of issue #521 under a fresh
// directory: base/go.work lists ./main, and a git-worktree-like second copy of
// the same module sits inside it at main/.claude/worktrees/wt. It returns the
// main checkout's directory and the worktree's.
//
// The fixture carries its own go.work, so the upward search from either root
// stops there whatever encloses the test's temporary directory.
func goWorkWorktreeFixture(t *testing.T) (mainDir, wtDir string) {
	t.Helper()
	base := t.TempDir()
	mustWrite(t, filepath.Join(base, "go.work"), "go 1.22\n\nuse ./main\n")
	mainDir = filepath.Join(base, "main")
	wtDir = filepath.Join(mainDir, ".claude", "worktrees", "wt")
	for _, d := range []string{mainDir, wtDir} {
		mustWrite(t, filepath.Join(d, "go.mod"), "module example.com/wt521\n\ngo 1.22\n")
	}
	return mainDir, wtDir
}

// isolateGoWorkEnv removes every GOWORK the test process could hand a child —
// an exported GOWORK (a developer running the suite with GOWORK=off, as the
// worktree instructions recommend) and the user's go env file — so the pool's
// decision is the only thing under test. Both are restored on cleanup.
func isolateGoWorkEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GOENV", "off")
	t.Setenv("GOWORK", "") // registers the restore of any prior value
	if err := os.Unsetenv("GOWORK"); err != nil {
		t.Fatal(err)
	}
}

// spawnResult is what one pool spawn gave the Go language server: the GOWORK its
// process received (and whether it received one at all), the go.work the pool
// REPORTS switching off — what session_start renders — and the go.work the pool
// PLANNED to switch off, asked for before the server existed.
type spawnResult struct {
	goWork   string
	set      bool
	reported string
	planned  string
}

// spawnedGoWork starts the pool's "go" language server for root with a stand-in
// command that records its own environment, and returns what it was given.
//
// The stand-in never speaks LSP, so the entry stays warming; that is enough,
// because what is under test is the environment of the spawn itself.
func spawnedGoWork(t *testing.T, root string, tweak func(p *workspacePool)) spawnResult {
	t.Helper()
	out := filepath.Join(t.TempDir(), "env.txt")
	script := fmt.Sprintf("env > %q.tmp && mv %q.tmp %q; exec sleep 30", out, out, out)
	pool := warmingPool(context.Background(), "/bin/sh", []string{"-c", script})
	if tweak != nil {
		tweak(pool)
	}
	defer pool.close()
	// The prediction is read BEFORE the spawn: it is what session_start tells an
	// agent whose server has not started, so it must not lean on one that has.
	planned := pool.plannedGoWorkOff(root, "go")
	if _, err := pool.acquireLang(context.Background(), root, "go", false); err != nil {
		t.Fatalf("acquireLang: %v", err)
	}
	res := spawnResult{reported: pool.goWorkOffFor(root, "go"), planned: planned}
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, err := os.ReadFile(out)
		if err == nil {
			for line := range strings.SplitSeq(string(data), "\n") {
				if v, ok := strings.CutPrefix(line, "GOWORK="); ok {
					res.goWork, res.set = v, true // os/exec keeps the last duplicate; so does this
				}
			}
			return res
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stand-in language server never recorded its environment: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestPoolSpawn_GoWorkOffForWorktreeUnderEnclosingGoWork is the regression test
// for #521: a Go language server started for a worktree whose enclosing go.work
// lists the MAIN checkout's directory for the same module must run with
// GOWORK=off, or it answers every query from the main checkout. The pool must
// also report which go.work it switched off, for the orientation line.
func TestPoolSpawn_GoWorkOffForWorktreeUnderEnclosingGoWork(t *testing.T) {
	isolateGoWorkEnv(t)
	mainDir, wt := goWorkWorktreeFixture(t)

	got := spawnedGoWork(t, wt, nil)
	if !got.set || got.goWork != "off" {
		t.Fatalf("worktree server GOWORK = %q (set=%v), want \"off\"", got.goWork, got.set)
	}
	if want := filepath.Join(filepath.Dir(mainDir), "go.work"); got.reported != want {
		t.Fatalf("pool reports GOWORK=off against %q, want %q", got.reported, want)
	}
}

// goWorkLeftAloneCase is one way a Go server's GOWORK reaches it untouched, or
// as somebody's explicit choice.
type goWorkLeftAloneCase struct {
	name    string
	root    string
	inherit string // a GOWORK already in the daemon's environment
	tweak   func(p *workspacePool)
	wantSet bool
	want    string
}

func goWorkLeftAloneCases(mainDir, wt string) []goWorkLeftAloneCase {
	return []goWorkLeftAloneCase{
		{name: "main checkout listed in go.work keeps workspace mode", root: mainDir},
		{
			name: "inherited GOWORK wins", root: wt,
			inherit: "/elsewhere/go.work",
			wantSet: true, want: "/elsewhere/go.work",
		},
		{
			name: "[lsp.go] env GOWORK wins", root: wt,
			tweak: func(p *workspacePool) {
				p.langs[0].cfg.Env = map[string]string{"GOWORK": "/configured/go.work"}
			},
			wantSet: true, want: "/configured/go.work",
		},
		{
			// gopls applies its own env setting over its process environment.
			name: "gopls env setting in initialization_options wins", root: wt,
			tweak: func(p *workspacePool) {
				p.langs[0].cfg.InitializationOptions = map[string]any{"env": map[string]any{"GOWORK": "/gopls/go.work"}}
			},
		},
	}
}

// TestPoolSpawn_GoWorkLeftAlone is the other direction: every case where the
// workspace applies, or where GOWORK is already somebody's explicit choice, must
// reach the server untouched — and the pool must not claim otherwise.
func TestPoolSpawn_GoWorkLeftAlone(t *testing.T) {
	isolateGoWorkEnv(t)
	mainDir, wt := goWorkWorktreeFixture(t)

	for _, tc := range goWorkLeftAloneCases(mainDir, wt) {
		t.Run(tc.name, func(t *testing.T) {
			if tc.inherit != "" {
				t.Setenv("GOWORK", tc.inherit)
			}
			got := spawnedGoWork(t, tc.root, tc.tweak)
			if got.set != tc.wantSet || got.goWork != tc.want {
				t.Fatalf("server GOWORK = %q (set=%v), want %q (set=%v)", got.goWork, got.set, tc.want, tc.wantSet)
			}
			if got.reported != "" {
				t.Fatalf("pool reports GOWORK=off against %q for a server it left alone", got.reported)
			}
		})
	}
}

// TestPlannedGoWorkOff_EqualsTheSpawn pins the prediction session_start makes for
// a server that has not started (a subagent's first call, PR #559 review B2) to
// what the pool then really spawns: the go.work it plans to switch off is the one
// the server's own environment shows switched off, and the one the pool reports
// afterwards. The prediction re-derives the decision from config and disk; if it
// drifted from startOrReuse — a case one of them knew and the other did not — an
// agent would be told its server runs one way and find it runs the other.
//
// The cases are every outcome of the decision: switched off for a worktree under
// another checkout's go.work, and each of the four ways it is left alone.
func TestPlannedGoWorkOff_EqualsTheSpawn(t *testing.T) {
	isolateGoWorkEnv(t)
	mainDir, wt := goWorkWorktreeFixture(t)
	offFile := filepath.Join(filepath.Dir(mainDir), "go.work")

	cases := append([]goWorkLeftAloneCase{
		{name: "worktree under another checkout's go.work", root: wt},
	}, goWorkLeftAloneCases(mainDir, wt)...)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.inherit != "" {
				t.Setenv("GOWORK", tc.inherit)
			}
			got := spawnedGoWork(t, tc.root, tc.tweak)

			// What the server's own environment shows is the ground truth the
			// prediction is held to: switched off exactly when GOWORK=off arrived.
			spawnedOff := got.set && got.goWork == "off"
			if (got.planned != "") != spawnedOff {
				t.Errorf("planned GOWORK=off against %q, but the spawn's GOWORK = %q (set=%v)", got.planned, got.goWork, got.set)
			}
			if spawnedOff && got.planned != offFile {
				t.Errorf("planned GOWORK=off against %q, want the go.work that lists another directory: %q", got.planned, offFile)
			}
			if got.planned != got.reported {
				t.Errorf("planned %q but the pool reports %q after the spawn", got.planned, got.reported)
			}
		})
	}
}

// TestGoLSPEnv_OnlyTheGoServer: GOWORK means nothing to a server that does not
// run the go command, so another language's server is never given it — even
// for the very root where the Go server is.
func TestGoLSPEnv_OnlyTheGoServer(t *testing.T) {
	isolateGoWorkEnv(t)
	_, wt := goWorkWorktreeFixture(t)
	base := []string{"PATH=/usr/bin"}

	if env, work := goLSPEnv("typescript", wt, config.LSPConfig{}, base); work != "" || len(env) != 1 {
		t.Fatalf("typescript server env = %v (go.work %q), want it untouched", env, work)
	}
	if env, work := goLSPEnv("go", wt, config.LSPConfig{}, base); work == "" || env[len(env)-1] != "GOWORK=off" {
		t.Fatalf("go server env = %v (go.work %q), want GOWORK=off", env, work)
	}
}
