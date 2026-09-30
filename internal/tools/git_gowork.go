package tools

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/plumbkit/plumb/internal/paths"
)

// git_gowork.go decides, per child process, whether a go command that child runs
// should be given GOWORK=off.
//
// The failure it removes: the recommended way to isolate an agent is a git
// worktree of the repository, and worktrees usually live INSIDE the directory
// tree the main checkout sits in. Go finds a workspace by walking up from the
// working directory, so a go.work above the worktree is found — and it lists the
// main checkout's directory for the module, not the worktree's. In workspace mode
// that leaves the worktree with nothing that works: a package pattern such as
// "./..." is refused ("directory prefix . does not contain modules listed in
// go.work"), and the module's own import path resolves to the MAIN checkout's
// copy, so anything that does build builds the wrong tree. A pre-commit hook
// running "go build ./..." fails, `git log` still shows the previous commit, and
// the only way through used to be a shell: `export GOWORK=off && git commit`.
//
// A static [git] env GOWORK=off is not an answer: it is project-wide, and the
// main checkout legitimately NEEDS its workspace. What is needed is a decision
// per child, made where plumb spawns the children that run a repository's own
// configured commands: the git child that runs its hooks (execGitCmd) and the
// stored [tasks.<lang>] commands of run_task and mutation_test (RunTaskArgv) —
// and where the daemon's pool spawns a Go language server for a workspace root
// (internal/cli, through GoWorkBypass), because gopls resolves the same go.work
// and would otherwise answer every query about a worktree from the main
// checkout (#521). run_command is deliberately not one of them: its commands
// are the agent's own, and one of them may be exactly `go work use .` — the fix
// a workspace needs, which GOWORK=off would refuse.
//
// The rule, checked against the go command rather than assumed. GOWORK=off is
// applied only when ALL of these hold, and every doubt resolves to "leave it
// alone":
//
//  1. No GOWORK is set for the child: not in its environment (whatever the value —
//     an inherited GOWORK and a [git] env entry are both explicit choices, and the
//     latter is the documented override), and not in either file go also consults
//     for it: the user's go env file (`go env GOENV`) and the toolchain's
//     $GOROOT/go.env.
//  2. A go.work is found by walking up from the child's working directory, as go
//     finds it, and it parses the way go parses it (gomodfile.go).
//  3. A go.mod is found at or above that directory and no higher than the
//     go.work's own directory, and declares a module path. A go.mod above the
//     go.work is not this workspace's business (a stray ~/go.mod over
//     ~/src/go.work would otherwise make every repository below look like Go).
//  4. That go.mod's directory is not a `use` directory, and no `use` directory
//     sits inside it: a workspace that reaches into the module is evidence it is
//     meant to apply there.
//  5. Some OTHER `use` directory declares the SAME module path. This is what makes
//     GOWORK=off strictly better than workspace mode, not merely different: the
//     workspace has another directory for this very module, so from here
//     workspace mode can only refuse or build that other copy. A module the
//     go.work simply does not list, with no copy of it listed either, is left
//     alone — there, workspace mode still resolves other modules by import path
//     (`go run example.com/devtools/cmd/lint`), and GOWORK=off would break that.
//
// Paths are compared LEXICALLY, from the working directory's own spelling, because
// go does: it walks up from the working directory it is given (os.Getwd prefers
// PWD, which the child is given — see pinChildPWD) and matches `use` entries as
// written. A `use ./alias` whose link points at this module does not cover it to
// go, and does not cover it here.
//
// The known cost, stated: a hook that relies on a tool listed only in the
// workspace — `go tool x` whose `tool` directive is in a separately listed tools
// module — loses it with GOWORK=off, where workspace mode would have run it (while
// refusing every `./...` the same hook ran). Such a repository sets [git] env
// GOWORK (any value) and keeps the go command's own behaviour.

// goWorkEnvKey is the environment variable the go command reads for the workspace.
const goWorkEnvKey = "GOWORK"

// applyAutoGoWork gives cmd GOWORK=off when GoWorkBypass says the go.work its
// working directory would find must be switched off, and returns that go.work's
// path ("" when cmd was left alone). The environment it sets is cmd.Environ() —
// what the child would have run with, including the PWD os/exec adds for a nil
// Env — plus GOWORK=off, so it never aliases a slice shared with other commands
// (gitChildSpec.Env is built once per connection).
//
// Concurrency: touches only cmd, which the caller owns; safe for concurrent use
// on distinct commands.
func applyAutoGoWork(cmd *exec.Cmd) string {
	workFile := GoWorkBypass(cmd.Dir, cmd.Env)
	if workFile == "" {
		return ""
	}
	slog.Debug("running the child with GOWORK=off: the enclosing go.work lists another directory for this module",
		"dir", cmd.Dir, "go_work", workFile)
	cmd.Env = withEnvVar(cmd.Environ(), goWorkEnvKey, "off")
	return workFile
}

// pinChildPWD makes an explicit environment carry PWD=<the child's directory>.
//
// os/exec sets PWD to cmd.Dir only when cmd.Env is nil. Once plumb builds the
// environment itself — [git] env, or the GOWORK=off above — the child would
// otherwise inherit the DAEMON's PWD, which is "/" for an auto-spawned daemon or
// wherever a hand-started one was launched. Anything that trusts $PWD then works
// in the wrong directory: a Makefile's $(PWD), a hook script reading os.environ,
// and go itself, whose os.Getwd prefers a PWD naming the same directory. cmd/go
// does the same for its own children (base.AppendPWD).
func pinChildPWD(cmd *exec.Cmd) {
	if cmd.Env == nil || cmd.Dir == "" {
		return
	}
	abs, err := filepath.Abs(cmd.Dir)
	if err != nil {
		return
	}
	cmd.Env = withEnvVar(cmd.Env, "PWD", abs)
}

// withEnvVar returns a copy of env with every key entry removed and key=val
// appended. It never writes into env's backing array, and removing every entry
// (not just the first) matters because os/exec keeps the LAST duplicate.
func withEnvVar(env []string, key, val string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if !strings.HasPrefix(e, prefix) {
			out = append(out, e)
		}
	}
	return append(out, prefix+val)
}

// GoWorkBypass returns the go.work a go command started in dir with env (nil means
// the daemon's own) would use and that the rule in the file comment says to switch
// off, or "" to leave the environment alone.
func GoWorkBypass(dir string, env []string) string {
	return goWorkBypassBelow(dir, env, "")
}

// goWorkBypassBelow is GoWorkBypass with the upward go.work search stopped at
// ceiling ("" means the filesystem root, which is what production passes — the go
// command does not stop earlier either). The ceiling exists for tests: `make test`
// puts t.TempDir() under the repository's own .testcache, where a go.work above a
// fixture is whatever the developer's checkout has, and the "no go.work" cases
// cannot be built any other way.
func goWorkBypassBelow(dir string, env []string, ceiling string) string {
	if dir == "" || !filepath.IsAbs(dir) {
		return ""
	}
	if _, set := lookupChildEnv(env, goWorkEnvKey); set {
		return ""
	}
	start := filepath.Clean(dir)
	workFile := findUpFile(start, "go.work", ceiling)
	if workFile == "" || !goWorkShadowsModule(workFile, start) {
		return ""
	}
	if goEnvFileSetsGoWork(start, env) {
		return ""
	}
	return workFile
}

// goWorkShadowsModule applies rules 2–5 of the file comment to the go.work at
// workFile, found above start.
func goWorkShadowsModule(workFile, start string) bool {
	modFile := findUpFile(start, "go.mod", filepath.Dir(workFile))
	if modFile == "" {
		return false
	}
	modDir := filepath.Dir(modFile)
	modPath, ok := goModulePath(modFile)
	if !ok {
		return false
	}
	uses, err := goWorkUses(workFile)
	if err != nil {
		return false
	}
	for _, u := range uses {
		if withinRoot(modDir, u) {
			return false // listed, or the workspace reaches into the module
		}
	}
	for _, u := range uses {
		if p, ok := goModulePath(filepath.Join(u, "go.mod")); ok && p == modPath {
			return true
		}
	}
	return false
}

// findUpFile walks up from dir looking for an entry called name that is not a
// directory — exactly the test the go command applies to go.work and go.mod — and
// returns its path or "". Whether it is a file plumb will READ is decided by the
// reader (readGoConfigFile), not here: go finds a go.work that is a link to a
// device too, and the right answer for one is "leave it alone", not "look
// further up". A non-empty ceiling is the last directory searched.
func findUpFile(dir, name, ceiling string) string {
	dir = filepath.Clean(dir)
	for {
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir || (ceiling != "" && dir == filepath.Clean(ceiling)) {
			return ""
		}
		dir = parent
	}
}

// lookupChildEnv returns key's value in a child environment and whether it is
// present. A nil env means the daemon's own, which is what os/exec gives the child
// then. For a built env the LAST entry wins, as it does in os/exec.
func lookupChildEnv(env []string, key string) (string, bool) {
	if env == nil {
		return os.LookupEnv(key)
	}
	prefix := key + "="
	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], prefix); ok {
			return v, true
		}
	}
	return "", false
}

// goEnvFileSetsGoWork reports whether a go env file the child's go command would
// read has a GOWORK line: the user's (`go env GOENV`) or the toolchain's
// $GOROOT/go.env. go consults them, in that order, for any variable its
// environment leaves empty, so a GOWORK written in either is as much an explicit
// choice as one in the environment. A missing or unreadable file sets nothing, as
// it does for go.
func goEnvFileSetsGoWork(dir string, env []string) bool {
	for _, file := range []string{goEnvFilePath(dir, env), goRootEnvFile(env)} {
		if file != "" && goEnvFileHasGoWork(file) {
			return true
		}
	}
	return false
}

// goEnvFileHasGoWork reports whether the go env file at path has a GOWORK line,
// read the way go reads it (cmd/go/internal/cfg): KEY=VALUE lines, where a line
// with no "=" or not starting with an upper-case letter is ignored.
func goEnvFileHasGoWork(path string) bool {
	data, err := readGoConfigFile(path)
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		i := strings.IndexByte(line, '=')
		if i < 0 || line[0] < 'A' || line[0] > 'Z' {
			continue
		}
		if line[:i] == goWorkEnvKey {
			return true
		}
	}
	return false
}

// goRootEnvFile is the toolchain's go.env for the go the child would run: under
// the child's GOROOT when it sets one, else beside the first `go` on the child's
// PATH, resolved through links the way go finds its own GOROOT (the parent of the
// directory holding the real binary). "" when there is no go to find.
func goRootEnvFile(env []string) string {
	if root, _ := lookupChildEnv(env, "GOROOT"); root != "" {
		return filepath.Join(root, "go.env")
	}
	path, _ := lookupChildEnv(env, "PATH")
	for _, dir := range filepath.SplitList(path) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue // go's own lookup refuses a relative PATH entry too
		}
		bin := filepath.Join(dir, "go")
		info, err := os.Stat(bin)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		return filepath.Join(filepath.Dir(filepath.Dir(paths.Canonical(bin))), "go.env")
	}
	return ""
}

// goEnvFilePath is where the child's go command looks for its env file: $GOENV
// ("off" disables it), else go/env under the user config directory, computed from
// the CHILD's environment the way os.UserConfigDir computes it. "" when there is
// none.
func goEnvFilePath(dir string, env []string) string {
	if v, _ := lookupChildEnv(env, "GOENV"); v != "" {
		if v == "off" {
			return ""
		}
		if !filepath.IsAbs(v) {
			v = filepath.Join(dir, v)
		}
		return v
	}
	home, _ := lookupChildEnv(env, "HOME")
	var config string
	switch runtime.GOOS {
	case "darwin", "ios":
		if home == "" {
			return ""
		}
		config = filepath.Join(home, "Library", "Application Support")
	default:
		if xdg, _ := lookupChildEnv(env, "XDG_CONFIG_HOME"); xdg != "" {
			if !filepath.IsAbs(xdg) {
				return "" // os.UserConfigDir rejects a relative one, and so go has no file
			}
			config = xdg
		} else {
			if home == "" {
				return ""
			}
			config = filepath.Join(home, ".config")
		}
	}
	return filepath.Join(config, "go", "env")
}
