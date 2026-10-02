package tools

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/plumbkit/plumb/internal/redact"
)

// task_env.go carries a [tasks.<lang>] env (#537) from the resolved TaskCommand to
// the child: placeholder expansion at the directory the command actually runs in,
// the one directory plumb creates for it, and the redacted form run_task reports.
// The config layer validates the table and binds it into the trust hash; this file
// only applies what the resolver handed over.
//
// Concurrency: stateless; safe for concurrent use.

// The env placeholders. config.TaskEnvWorkspace / TaskEnvWorkingDir name the same
// strings — this package does not import config — and
// TestTaskEnvPlaceholders_MatchConfig pins the two together.
const (
	taskEnvWorkspace  = "{workspace}"
	taskEnvWorkingDir = "{working_dir}"
)

// tempDirEnvKeys name a directory the tool refuses to run without: go fails with
// "creating work dir: stat …: no such file or directory" when GOTMPDIR is absent.
// `make test` has a prerequisite that creates plumb's .testcache; run_task has no
// prerequisites, and a fresh worktree has no .testcache, so without this every
// run_task test in a new worktree would fail before running a test.
var tempDirEnvKeys = map[string]bool{"GOTMPDIR": true, "TMPDIR": true}

// taskEnviron is PrepareTaskEnv for a resolved command run in dir; {workspace}
// is the resolver's root, or dir when it gave none.
func taskEnviron(cmd TaskCommand, dir string) []string {
	root := cmd.Root
	if root == "" {
		root = dir
	}
	return PrepareTaskEnv(cmd.Env, root, dir)
}

// PrepareTaskEnv expands KEY=VALUE templates for a command run in dir under the
// workspace root ({workspace} → root, {working_dir} → dir), and creates the
// directory a temp-dir variable names when it lies inside root — checked after
// symlink resolution, so a link out of the tree is left alone. Nothing outside the
// workspace is ever created: the tool then reports the missing directory itself.
// Exported for the `plumb build|test|…` CLI, which runs the same stored commands
// outside the daemon.
func PrepareTaskEnv(env []string, root, dir string) []string {
	if len(env) == 0 {
		return nil
	}
	r := strings.NewReplacer(taskEnvWorkspace, root, taskEnvWorkingDir, dir)
	out := make([]string, 0, len(env))
	for _, kv := range env {
		kv = r.Replace(kv)
		out = append(out, kv)
		k, v, _ := strings.Cut(kv, "=")
		// No root, no "inside": PathWithinWorkspace("", v) is true for any v, and
		// nothing may be created on that answer.
		if root == "" || !tempDirEnvKeys[k] || !filepath.IsAbs(v) || !PathWithinWorkspace(root, v) {
			continue
		}
		if err := os.MkdirAll(v, 0o755); err != nil {
			slog.Debug("could not create a task env temp directory", "key", k, "dir", v, "err", err)
		}
	}
	return out
}

// secretEnvWords mark a variable whose value is a credential by its NAME. The
// shared redactor recognises secrets by the shape of the value (a GitHub token,
// an AWS key) or by an assignment such as `token=…`, and neither catches
// `GITHUB_TOKEN=…` holding an opaque string.
var secretEnvWords = []string{"TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "API_KEY", "APIKEY", "PRIVATE_KEY"}

// describeTaskEnv renders the applied env for run_task's report: every name is
// shown, since which variables were set is the point, and a value is withheld
// when its name says it is a credential or the redactor recognises one in it.
func describeTaskEnv(env []string) string {
	parts := make([]string, 0, len(env))
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		upper := strings.ToUpper(k)
		for _, w := range secretEnvWords {
			if strings.Contains(upper, w) {
				v = "[REDACTED]"
				break
			}
		}
		v, _ = redact.Redact(v)
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, " ")
}

// validateRunFilter checks a {run} test-name filter (runPattern says what is
// allowed and why). Empty is valid: no filter.
func validateRunFilter(what, run string) error {
	if run == "" {
		return nil
	}
	if !runPattern.MatchString(run) {
		return fmt.Errorf("%s %q is not an accepted test-name filter: up to 256 of letters, digits, space and ._/:@|^$*+?()[]-, "+
			"not starting with -, @ or a space", what, run)
	}
	if flag, rest, ok := flagCoversOnlyFirstAlternative(run); ok {
		return fmt.Errorf("%s %q would silently select too few tests: go test -run matches each top-level | alternative as its own regex, "+
			"so the leading %s applies to the first alternative only (go test -list applies it to all, so -list looks right). "+
			"Group the alternatives: %s(%s)", what, run, flag, flag, rest)
	}
	return nil
}

// leadingFlagGroup is an inline flag group opening a pattern: (?i), (?is), …
var leadingFlagGroup = regexp.MustCompile(`^\(\?[a-zA-Z]+\)`)

// flagCoversOnlyFirstAlternative reports whether run opens with an inline flag
// group and then alternates at the top level, e.g. `(?i)write|delete`. Go's
// testing package splits a -run pattern on its top-level | (and /) and compiles
// each alternative separately, so the flag reaches only the first: `delete`
// stays case-sensitive and, for a mixed-case test name, matches nothing. The
// subset of tests that then runs can pass a mutant its real tests would kill,
// which mutation_test reports as SURVIVED. It returns the flag group and the
// rest of the pattern, for the grouped spelling the refusal suggests.
func flagCoversOnlyFirstAlternative(run string) (flag, rest string, ok bool) {
	flag = leadingFlagGroup.FindString(run)
	if flag == "" {
		return "", "", false
	}
	rest = run[len(flag):]
	depth, inClass := 0, false
	for i := 0; i < len(rest); i++ {
		switch c := rest[i]; {
		case c == '\\':
			i++
		case inClass:
			if c == ']' {
				inClass = false
			}
		case c == '[':
			inClass = true
		case c == '(':
			depth++
		case c == ')':
			if depth > 0 {
				depth--
			}
		case c == '|' && depth == 0:
			return flag, rest, true
		}
	}
	return "", "", false
}
