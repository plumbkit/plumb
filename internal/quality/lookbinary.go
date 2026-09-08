package quality

// lookbinary.go resolves an analyser's executable.
//
// It exists because PATH alone is not enough, and the way it is not enough
// differs per ecosystem. The daemon does NOT run with the user's interactive
// PATH: it inherits the environment of whichever `plumb serve` proxy spawned it,
// captured when that agent session started. That environment routinely lacks the
// directory the tool was installed into — ~/go/bin for a `go install`ed linter,
// ~/.local/bin for a `uv tool install`ed one — so a PATH-only lookup silently
// disables the analyser on a perfectly well set-up machine. That failure was
// found once for golangci-lint and then again for ruff, which is why the search
// is a per-ecosystem list rather than one hardcoded fallback.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/plumbkit/plumb/internal/paths"
)

// lookPath is the PATH lookup seam (tests substitute it).
var lookPath = exec.LookPath

// LookBinary resolves the executable for t: an explicit override first, then
// PATH, then the ecosystem's install directories.
//
// override comes from [quality.bin] and wins outright when it names an
// executable file, because it is the user stating the answer — a resolution
// order that let PATH beat an explicit path would make the setting advisory,
// which is the opposite of what someone reaches for it to do. An override that
// does NOT resolve falls through rather than failing the lookup: the user's
// intent was "use this tool", and a stale path in a config file should not
// disable an analyser that is sitting on PATH.
//
// Exported so `plumb doctor` reports the same binary the analyser will actually
// run — a doctor check that resolved differently would be worse than none.
func LookBinary(t Tool, override string) (string, bool) {
	if override != "" {
		if p, ok := executableAt(paths.ExpandHome(override)); ok {
			return p, true
		}
	}
	if bin, err := lookPath(t.Binary); err == nil {
		return bin, true
	}
	for _, dir := range BinDirs(t.Ecosystem) {
		if p, ok := executableAt(filepath.Join(dir, t.Binary)); ok {
			return p, true
		}
	}
	return "", false
}

// executableAt reports whether p names a regular file with an execute bit, and
// returns it. A directory named like the binary, or a non-executable file, must
// never be mistaken for the tool.
func executableAt(p string) (string, bool) {
	info, err := os.Stat(p)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return "", false
	}
	return p, true
}

// BinDirs lists the directories an ecosystem's package manager installs into,
// most specific first. Read from the environment rather than shelling out to
// `go env` or `npm config get prefix`, which would spawn a process on a write
// path that must stay cheap.
//
// Exported so `plumb doctor` can name every directory it searched when a binary
// is missing — "not found" without the search path is a dead end for the user.
func BinDirs(e Ecosystem) []string {
	switch e {
	case EcosystemGo:
		return goToolBinDirs()
	case EcosystemPython:
		return pythonBinDirs()
	case EcosystemNode:
		return nodeBinDirs()
	default:
		return nil
	}
}

// goToolBinDirs lists the directories `go install` writes to.
func goToolBinDirs() []string {
	var dirs []string
	if gobin := os.Getenv("GOBIN"); gobin != "" {
		dirs = append(dirs, gobin)
	}
	if gopath := os.Getenv("GOPATH"); gopath != "" {
		// GOPATH may be a list; only the first element receives installs.
		first, _, _ := strings.Cut(gopath, string(os.PathListSeparator))
		if first != "" {
			dirs = append(dirs, filepath.Join(first, "bin"))
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, "go", "bin"))
	}
	return dirs
}

// pythonBinDirs lists where a Python tool lands. An active virtualenv comes
// first because a project that pins its own ruff means that ruff and not the
// user's global one; ~/.local/bin is where `pip install --user` and
// `uv tool install` both put a console script.
func pythonBinDirs() []string {
	var dirs []string
	if venv := os.Getenv("VIRTUAL_ENV"); venv != "" {
		dirs = append(dirs, filepath.Join(venv, "bin"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".local", "bin"))
	}
	return dirs
}

// nodeBinDirs lists where a globally installed Node tool lands.
//
// Deliberately NOT including a project's node_modules/.bin: that is per
// workspace rather than per machine, so resolving it needs the analysed file's
// project root, which this function does not have. No Node analyser is
// implemented yet; wiring one means threading the workspace in here, and doing
// it now would be an untested guess at the shape that thread wants.
func nodeBinDirs() []string {
	var dirs []string
	if prefix := os.Getenv("NPM_CONFIG_PREFIX"); prefix != "" {
		dirs = append(dirs, filepath.Join(prefix, "bin"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".local", "bin"))
	}
	return dirs
}
