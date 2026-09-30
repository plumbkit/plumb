package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// git_gowork_test.go covers the decision behind the automatic GOWORK=off: which
// directories an enclosing go.work shadows, how the choice already made elsewhere
// (environment, go env file) is respected, and how the result is folded into a
// child's environment. git_gowork_hook_test.go is the end-to-end half, through
// real git, real hooks and a real `go build`.
//
// Every layout below was run against the go command first (see the file comment
// in git_gowork.go); the table records what go does, and the expected column is
// what plumb does about it.

const (
	goModMain  = "module example.com/m\n\ngo 1.21\n"
	goModOther = "module example.com/other\n\ngo 1.21\n"
)

// goWorkLayout writes files (relative path → content) under a fresh directory and
// returns it, with symlinks resolved so paths compare the way git reports them.
func goWorkLayout(t *testing.T, files map[string]string) string {
	t.Helper()
	root := evalTempDir(t)
	writeTree(t, root, files)
	return root
}

// hermeticGoEnv points GOENV at a file that does not exist and removes GOWORK, so
// neither a developer's go env file nor their shell can decide a case for the test.
func hermeticGoEnv(t *testing.T) {
	t.Helper()
	unsetEnvForTest(t, "GOWORK")
	t.Setenv("GOENV", filepath.Join(t.TempDir(), "no-go-env"))
}

func TestGoWorkBypass(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		dir   string // relative to the layout root
		off   bool
	}{
		{
			// The field setup. go refuses ./... here and resolves example.com/m to
			// main — the worktree has nothing that works in workspace mode.
			name: "worktree inside the listed main checkout, same module",
			files: map[string]string{
				"go.work":                             "go 1.21\n\nuse ./main\n",
				"main/go.mod":                         goModMain,
				"main/.claude/worktrees/wt/go.mod":    goModMain,
				"main/.claude/worktrees/wt/README.md": "x",
			},
			dir: "main/.claude/worktrees/wt",
			off: true,
		},
		{
			name: "worktree beside the listed main checkout, same module",
			files: map[string]string{
				"go.work":     "go 1.21\n\nuse ./main\n",
				"main/go.mod": goModMain,
				"wt/go.mod":   goModMain,
			},
			dir: "wt",
			off: true,
		},
		{
			name: "a subdirectory of the shadowed module",
			files: map[string]string{
				"go.work":         "go 1.21\n\nuse ./main\n",
				"main/go.mod":     goModMain,
				"wt/go.mod":       goModMain,
				"wt/pkg/i/x.txt":  "x",
				"wt/pkg/i/y.txt":  "y",
				"main/pkg/i/x.go": "package i\n",
			},
			dir: "wt/pkg/i",
			off: true,
		},
		{
			name: "the module the go.work lists",
			files: map[string]string{
				"go.work":   "go 1.21\n\nuse ./wt\n",
				"wt/go.mod": goModMain,
			},
			dir: "wt",
			off: false,
		},
		{
			name: "listed inside a use( block with no space",
			files: map[string]string{
				"go.work":     "go 1.21\n\nuse(\n\t./main\n\t./wt\n)\n",
				"main/go.mod": goModMain,
				"wt/go.mod":   goModMain,
			},
			dir: "wt",
			off: false,
		},
		{
			// A plain checkout of an UNLISTED module whose path no listed module
			// shares. go refuses ./... here too, but still resolves the listed
			// modules by import path — `go run example.com/devtools/cmd/lint` works
			// in workspace mode and would break under GOWORK=off. Left alone.
			name: "unlisted module with no copy of itself in the workspace",
			files: map[string]string{
				"go.work":         "go 1.21\n\nuse ./devtools\n",
				"devtools/go.mod": "module example.com/devtools\n\ngo 1.21\n",
				"app/go.mod":      goModOther,
			},
			dir: "app",
			off: false,
		},
		{
			name:  "no go.work above",
			files: map[string]string{"repo/go.mod": goModMain},
			dir:   "repo",
			off:   false,
		},
		{
			name: "go.work above but no go.mod at or above the directory",
			files: map[string]string{
				"go.work":        "go 1.21\n\nuse ./main\n",
				"main/go.mod":    goModMain,
				"docs/README.md": "x",
			},
			dir: "docs",
			off: false,
		},
		{
			// A go.mod ABOVE the go.work is not this workspace's module; the search
			// stops at the go.work's directory. (A stray ~/go.mod over ~/src/go.work
			// used to make every repository below look like an excluded module.)
			name: "the only go.mod is above the go.work",
			files: map[string]string{
				"go.mod":                    goModMain,
				"projects/go.work":          "go 1.21\n\nuse ./main\n",
				"projects/main/go.mod":      goModMain,
				"projects/notgo/README.txt": "x",
			},
			dir: "projects/notgo",
			off: false,
		},
		{
			name: "a plain subdirectory of a listed module",
			files: map[string]string{
				"go.work":              "go 1.21\n\nuse ./main\n",
				"main/go.mod":          goModMain,
				"main/pkg/inner/i.txt": "x",
			},
			dir: "main/pkg/inner",
			off: false,
		},
		{
			// Deliberately cautious: a workspace that reaches INTO the module is
			// evidence it is meant to apply there.
			name: "a use directory inside the module",
			files: map[string]string{
				"go.work":    "go 1.21\n\nuse (\n\t./sub\n\t./copy\n)\n",
				"go.mod":     goModMain,
				"sub/go.mod": goModOther,
				"copy/x.txt": "x",
			},
			dir: ".",
			off: false,
		},
		{
			name: "a go.work go cannot parse",
			files: map[string]string{
				"go.work":     "go 1.21\n\nuse `./main`\n",
				"main/go.mod": goModMain,
				"wt/go.mod":   goModMain,
			},
			dir: "wt",
			off: false,
		},
		{
			name: "a go.mod with no module line",
			files: map[string]string{
				"go.work":     "go 1.21\n\nuse ./main\n",
				"main/go.mod": goModMain,
				"wt/go.mod":   "go 1.21\n",
			},
			dir: "wt",
			off: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hermeticGoEnv(t)
			root := goWorkLayout(t, tc.files)
			dir := filepath.Join(root, tc.dir)
			// The ceiling keeps the go.work search inside the layout: t.TempDir()
			// sits under the checkout's .testcache under `make test`.
			got := goWorkBypassBelow(dir, nil, root)
			if (got != "") != tc.off {
				t.Fatalf("goWorkBypass(%s) = %q, want off=%v", tc.dir, got, tc.off)
			}
			if tc.off && got != filepath.Join(root, "go.work") {
				t.Errorf("reported go.work %q, want %q", got, filepath.Join(root, "go.work"))
			}
		})
	}
}

// TestGoWorkBypass_ComparesPathsTheWayGoDoes: go matches `use` entries lexically,
// against the working directory's own spelling (checked: `use ./alias` with
// alias → main does NOT cover main when go runs from main's real path, and does
// when it runs from the alias spelling). Resolving symlinks here would call a
// module covered that go refuses, and withhold the fix.
func TestGoWorkBypass_ComparesPathsTheWayGoDoes(t *testing.T) {
	hermeticGoEnv(t)
	root := goWorkLayout(t, map[string]string{
		"go.work":     "go 1.21\n\nuse ./alias\n",
		"main/go.mod": goModMain,
	})
	if err := os.Symlink(filepath.Join(root, "main"), filepath.Join(root, "alias")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	if got := goWorkBypassBelow(filepath.Join(root, "main"), nil, root); got == "" {
		t.Error("from main's real path go refuses a workspace that lists it only as ./alias; plumb must switch it off")
	}
	if got := goWorkBypassBelow(filepath.Join(root, "alias"), nil, root); got != "" {
		t.Errorf("from the alias spelling go accepts the workspace; plumb switched it off (%s)", got)
	}
}

// TestGoWorkBypass_AChoiceAlreadyMadeWins: a GOWORK in the child's environment —
// whatever its value — or in the go env file go would read, is never overridden.
func TestGoWorkBypass_AChoiceAlreadyMadeWins(t *testing.T) {
	layout := map[string]string{
		"go.work":     "go 1.21\n\nuse ./main\n",
		"main/go.mod": goModMain,
		"wt/go.mod":   goModMain,
	}
	for _, v := range []string{"off", "", "auto", "/x/go.work"} {
		t.Run("built env GOWORK="+v, func(t *testing.T) {
			hermeticGoEnv(t)
			root := goWorkLayout(t, layout)
			if got := goWorkBypassBelow(filepath.Join(root, "wt"), []string{"PATH=/bin", "GOWORK=" + v}, root); got != "" {
				t.Errorf("an explicit GOWORK=%q in the child's env was overridden", v)
			}
		})
		t.Run("inherited GOWORK="+v, func(t *testing.T) {
			hermeticGoEnv(t)
			root := goWorkLayout(t, layout)
			t.Setenv("GOWORK", v)
			if got := goWorkBypassBelow(filepath.Join(root, "wt"), nil, root); got != "" {
				t.Errorf("an inherited GOWORK=%q was overridden", v)
			}
		})
	}
	t.Run("the go env file sets GOWORK", func(t *testing.T) {
		hermeticGoEnv(t)
		root := goWorkLayout(t, layout)
		envFile := filepath.Join(root, "goenv")
		writeTree(t, root, map[string]string{"goenv": "GOPROXY=off\nGOWORK=" + filepath.Join(root, "go.work") + "\n"})
		t.Setenv("GOENV", envFile)
		if got := goWorkBypassBelow(filepath.Join(root, "wt"), nil, root); got != "" {
			t.Error("a GOWORK in the go env file is go's choice too, and was overridden")
		}
		// The CHILD's GOENV is the one go reads, not the daemon's.
		if got := goWorkBypassBelow(filepath.Join(root, "wt"), []string{"GOENV=" + envFile}, root); got != "" {
			t.Error("a GOWORK in the child's go env file was overridden")
		}
		if got := goWorkBypassBelow(filepath.Join(root, "wt"), []string{"GOENV=off"}, root); got == "" {
			t.Error("GOENV=off disables the go env file, so its GOWORK must not count")
		}
	})
	t.Run("the default go env file under the child's HOME", func(t *testing.T) {
		hermeticGoEnv(t)
		root := goWorkLayout(t, layout)
		home := filepath.Join(root, "home")
		config := filepath.Join(home, ".config")
		if goEnvFilePath(root, []string{"HOME=" + home}) == filepath.Join(home, "Library", "Application Support", "go", "env") {
			config = filepath.Join(home, "Library", "Application Support")
		}
		writeTree(t, config, map[string]string{"go/env": "GOWORK=off\n"})
		env := []string{"HOME=" + home}
		if got := goWorkBypassBelow(filepath.Join(root, "wt"), env, root); got != "" {
			t.Error("the go env file under the child's user config directory sets GOWORK; it was overridden")
		}
	})
	t.Run("the toolchain's GOROOT/go.env sets GOWORK", func(t *testing.T) {
		hermeticGoEnv(t)
		root := goWorkLayout(t, layout)
		writeTree(t, root, map[string]string{"goroot/go.env": "GOWORK=off\n", "tc/go.env": "GOWORK=off\n", "tc2/go.env": "GOPROXY=off\n"})
		wt := filepath.Join(root, "wt")
		if got := goWorkBypassBelow(wt, []string{"GOENV=off", "GOROOT=" + filepath.Join(root, "goroot")}, root); got != "" {
			t.Error("a GOWORK in the child's $GOROOT/go.env is go's choice too, and was overridden")
		}
		// No GOROOT set: go finds its root from the binary on PATH (through links).
		for _, tc := range []string{"tc", "tc2"} {
			writeTree(t, root, map[string]string{tc + "/bin/go": "#!/bin/sh\n"})
			if err := os.Chmod(filepath.Join(root, tc, "bin", "go"), 0o755); err != nil { //nolint:gosec // G302: a fake go binary must be executable
				t.Fatal(err)
			}
		}
		link := filepath.Join(root, "linkbin")
		if err := os.MkdirAll(link, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(root, "tc", "bin", "go"), filepath.Join(link, "go")); err != nil {
			t.Skipf("symlink: %v", err)
		}
		if got := goWorkBypassBelow(wt, []string{"GOENV=off", "PATH=" + link}, root); got != "" {
			t.Error("the go.env beside the go on the child's PATH sets GOWORK; it was overridden")
		}
		if got := goWorkBypassBelow(wt, []string{"GOENV=off", "PATH=" + filepath.Join(root, "tc2", "bin")}, root); got == "" {
			t.Error("a toolchain go.env without GOWORK must not stop the fix")
		}
	})
	t.Run("a go env file with other variables only", func(t *testing.T) {
		hermeticGoEnv(t)
		root := goWorkLayout(t, layout)
		writeTree(t, root, map[string]string{"goenv": "GOPROXY=off\n# GOWORK=x\ngowork=x\n"})
		if got := goWorkBypassBelow(filepath.Join(root, "wt"), []string{"GOENV=" + filepath.Join(root, "goenv")}, root); got == "" {
			t.Error("a go env file that does not set GOWORK (go ignores comment and lower-case lines) must not stop the fix")
		}
	})
}

// TestGoWorkBypass_ADeviceGoWorkIsLeftAloneQuickly: the decision runs on every git
// call and every run_task, so a go.work committed as a link to /dev/zero or a FIFO
// must be answered at once — with "leave it alone" — instead of reading forever.
func TestGoWorkBypass_ADeviceGoWorkIsLeftAloneQuickly(t *testing.T) {
	hermeticGoEnv(t)
	for _, target := range []string{"/dev/zero", "fifo"} {
		t.Run(target, func(t *testing.T) {
			root := goWorkLayout(t, map[string]string{"main/go.mod": goModMain, "wt/go.mod": goModMain})
			work := filepath.Join(root, "go.work")
			if target == "fifo" {
				if err := mkfifo(work); err != nil {
					t.Skipf("mkfifo: %v", err)
				}
			} else if err := os.Symlink(target, work); err != nil {
				t.Skipf("symlink: %v", err)
			}
			done := make(chan string, 1)
			go func() { done <- goWorkBypassBelow(filepath.Join(root, "wt"), nil, root) }()
			select {
			case got := <-done:
				if got != "" {
					t.Errorf("a go.work that is not a regular file must leave the environment alone; got %q", got)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the decision is still reading a device go.work after 5s — every git call on this clone would hang")
			}
		})
	}
}

// TestApplyAutoGoWork covers folding the decision into a command: the environment
// is what the child would have had anyway, plus GOWORK=off — including the PWD
// os/exec adds for a nil Env — and a shared slice is never written into.
func TestApplyAutoGoWork(t *testing.T) {
	hermeticGoEnv(t)
	root := goWorkLayout(t, map[string]string{
		"go.work":     "go 1.21\n\nuse ./main\n",
		"main/go.mod": goModMain,
		"wt/go.mod":   goModMain,
	})
	wt := filepath.Join(root, "wt")

	t.Run("nil env gains GOWORK=off and PWD, keeping the rest", func(t *testing.T) {
		t.Setenv("PLUMB_GOWORK_PROBE", "kept")
		cmd := exec.Command("true")
		cmd.Dir = wt
		if got := applyAutoGoWork(cmd); got != filepath.Join(root, "go.work") {
			t.Fatalf("applyAutoGoWork = %q, want the go.work", got)
		}
		requireEnvOnce(t, cmd.Env, "GOWORK", "off")
		requireEnvOnce(t, cmd.Env, "PWD", wt)
		requireEnvOnce(t, cmd.Env, "PLUMB_GOWORK_PROBE", "kept")
	})
	t.Run("a shared env is copied, not written into", func(t *testing.T) {
		shared := make([]string, 2, 8) // spare capacity: an in-place append would land in it
		shared[0], shared[1] = "PATH=/usr/bin:/bin", "PWD=/stale"
		before := slices.Clone(shared)
		cmd := exec.Command("true")
		cmd.Dir = wt
		cmd.Env = shared
		applyAutoGoWork(cmd)
		pinChildPWD(cmd)
		if !slices.Equal(shared, before) || slices.Contains(shared[:cap(shared)], "GOWORK=off") {
			t.Errorf("the shared slice was modified: %q", shared[:cap(shared)])
		}
		requireEnvOnce(t, cmd.Env, "GOWORK", "off")
		requireEnvOnce(t, cmd.Env, "PWD", wt)
	})
	t.Run("a directory that is not shadowed is left exactly alone", func(t *testing.T) {
		cmd := exec.Command("true")
		cmd.Dir = filepath.Join(root, "main")
		if got := applyAutoGoWork(cmd); got != "" || cmd.Env != nil {
			t.Errorf("applyAutoGoWork on a listed module = (%q, env %v), want nothing touched", got, cmd.Env)
		}
	})
	t.Run("no directory, nothing to decide", func(t *testing.T) {
		cmd := exec.Command("true")
		if got := applyAutoGoWork(cmd); got != "" || cmd.Env != nil {
			t.Errorf("a command with no Dir must be left alone; got %q", got)
		}
	})
}

// TestPinChildPWD: an explicit environment carries the child's own directory as
// PWD, replacing every inherited (stale) entry; a nil one is left for os/exec.
func TestPinChildPWD(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("true")
	cmd.Dir = dir
	cmd.Env = []string{"PWD=/stale", "A=1", "PWD=/also-stale"}
	pinChildPWD(cmd)
	requireEnvOnce(t, cmd.Env, "PWD", dir)
	requireEnvOnce(t, cmd.Env, "A", "1")

	nilEnv := exec.Command("true")
	nilEnv.Dir = dir
	pinChildPWD(nilEnv)
	if nilEnv.Env != nil {
		t.Errorf("a nil Env must stay nil — os/exec sets PWD itself then; got %q", nilEnv.Env)
	}
}

func TestFindUpFile(t *testing.T) {
	root := goWorkLayout(t, map[string]string{
		"go.work":            "x",
		"a/go.work/keep":     "a DIRECTORY named go.work is not a go.work",
		"a/b/go.work":        "x",
		"a/b/c/d/leaf/x.txt": "x",
	})
	leaf := filepath.Join(root, "a", "b", "c", "d", "leaf")
	if got := findUpFile(leaf, "go.work", ""); got != filepath.Join(root, "a", "b", "go.work") {
		t.Errorf("nearest wins: got %q", got)
	}
	if got := findUpFile(filepath.Join(root, "a"), "go.work", ""); got != filepath.Join(root, "go.work") {
		t.Errorf("a directory of the same name is skipped for the real file above it: got %q", got)
	}
	if got := findUpFile(leaf, "go.work", filepath.Join(root, "a", "b", "c")); got != "" {
		t.Errorf("the ceiling stops the climb: got %q", got)
	}
	if got := findUpFile(leaf, "go.work", filepath.Join(root, "a", "b")); got != filepath.Join(root, "a", "b", "go.work") {
		t.Errorf("the ceiling directory itself is searched: got %q", got)
	}
}

func TestWithEnvVar(t *testing.T) {
	in := []string{"A=1", "B=2", "A=3"}
	got := withEnvVar(in, "A", "9")
	if want := []string{"B=2", "A=9"}; !slices.Equal(got, want) {
		t.Errorf("withEnvVar = %q, want %q (every old entry gone: os/exec keeps the LAST duplicate)", got, want)
	}
	if !slices.Equal(in, []string{"A=1", "B=2", "A=3"}) {
		t.Errorf("the input was modified: %q", in)
	}
	if got := withEnvVar(nil, "A", ""); !slices.Equal(got, []string{"A="}) {
		t.Errorf("an empty value is still a value: %q", got)
	}
}

// requireEnvOnce fails unless env holds key exactly once, with value want.
func requireEnvOnce(t *testing.T, env []string, key, want string) {
	t.Helper()
	var got []string
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, key+"="); ok {
			got = append(got, v)
		}
	}
	if len(got) != 1 || got[0] != want {
		t.Errorf("%s in the env = %q, want exactly [%q]", key, got, want)
	}
}

// mkfifo makes a named pipe at path.
func mkfifo(path string) error { return syscall.Mkfifo(path, 0o600) }
