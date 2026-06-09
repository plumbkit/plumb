package cli

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/plumbkit/plumb/internal/config"
)

// envFor builds the environment slice for the LSP supervisor process.
// PATH is always augmented with directories that GUI-launched daemons on
// macOS commonly miss (launchd provides only /usr/bin:/bin:/usr/sbin:/sbin,
// omitting /usr/local/bin and /opt/homebrew/bin where Go and other tools
// are typically installed). Per-language lspCfg.Env overrides are applied
// on top, so users can still pin specific values via config.
func envFor(lspCfg config.LSPConfig) []string {
	env := augmentedEnv()
	for k, v := range lspCfg.Env {
		env = setEnvVar(env, k, v)
	}
	return env
}

// augmentedEnv returns os.Environ() with PATH extended to include directories
// that GUI-launched processes miss on macOS.
func augmentedEnv() []string {
	env := os.Environ()
	return setEnvVar(env, "PATH", augmentedPATH(currentEnvPATH(env)))
}

func currentEnvPATH(env []string) string {
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "PATH="); ok {
			return v
		}
	}
	return ""
}

// augmentedPATH extends current with entries from /etc/paths and
// /etc/paths.d/* (the sources macOS path_helper reads) and common Homebrew
// locations. Existing entries keep their position; new paths are appended in
// order, skipping duplicates.
func augmentedPATH(current string) string {
	seen := make(map[string]bool)
	var parts []string
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			parts = append(parts, p)
		}
	}
	for _, p := range filepath.SplitList(current) {
		add(p)
	}
	for _, p := range readPathsFile("/etc/paths") {
		add(p)
	}
	if entries, _ := filepath.Glob("/etc/paths.d/*"); entries != nil {
		for _, f := range entries {
			for _, p := range readPathsFile(f) {
				add(p)
			}
		}
	}
	for _, p := range []string{"/usr/local/bin", "/opt/homebrew/bin", "/opt/homebrew/sbin"} {
		add(p)
	}
	return strings.Join(parts, ":")
}

// readPathsFile reads a macOS-style paths file (one directory per line,
// blank lines ignored). Returns nil when the file is absent or unreadable.
func readPathsFile(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var lines []string
	for line := range strings.SplitSeq(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// setEnvVar replaces the value of key in env if it is already present,
// otherwise appends "key=value". The input slice is modified in place.
func setEnvVar(env []string, key, val string) []string {
	prefix := key + "="
	for i, e := range env {
		if strings.HasPrefix(e, prefix) {
			env[i] = prefix + val
			return env
		}
	}
	return append(env, prefix+val)
}
