package tools

import (
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
	env, _, _ := taskEnvPlan(cmd, dir, nil, nil)
	return env
}

// taskEnvPlan returns the environment one task command runs with: the command's
// [tasks.<lang>] env with this call's per-call changes (#634) applied on top,
// expanded at dir. It also returns the names removed for this call, and the notes
// the report prints for the two changes the resulting list cannot show — a
// configured entry whose value this call replaced, and a configured entry it
// removed.
//
// set and unset are already validated (config.ValidateTaskEnvOverrides), so this
// function only applies them. A removal is applied to the MERGED list, which is
// what makes `unset` beat a configured entry as well as an inherited one: the
// reported environment never names a variable the call asked to remove. The
// removal itself reaches the child through RunTaskArgv's unset argument, because
// only that layer can see what was INHERITED.
//
// Concurrency: stateless; safe for concurrent use.
func taskEnvPlan(cmd TaskCommand, dir string, set map[string]string, unset []string) (env, remove, notes []string) {
	root := cmd.Root
	if root == "" {
		root = dir
	}
	merged := make(map[string]string, len(cmd.Env)+len(set))
	configured := make(map[string]bool, len(cmd.Env))
	for _, kv := range cmd.Env {
		k, v, _ := strings.Cut(kv, "=")
		merged[k] = v
		configured[k] = true
	}
	where := configuredEnvLabel(cmd.Language)
	for _, k := range slices.Sorted(maps.Keys(set)) {
		if configured[k] {
			notes = append(notes, fmt.Sprintf("env %s from this call replaces the %s entry for it", k, where))
		}
		merged[k] = set[k]
	}
	remove = slices.Clone(unset)
	slices.Sort(remove)
	remove = slices.Compact(remove)
	for _, k := range remove {
		delete(merged, k)
		if configured[k] {
			notes = append(notes, fmt.Sprintf("env %s is removed for this call, so the %s entry for it does not apply", k, where))
		}
	}
	keys := slices.Sorted(maps.Keys(merged))
	raw := make([]string, 0, len(keys))
	for _, k := range keys {
		raw = append(raw, k+"="+merged[k])
	}
	return PrepareTaskEnv(raw, root, dir), remove, notes
}

// configuredEnvLabel names the config a resolved command's env came from, for a
// note about a per-call change to it.
func configuredEnvLabel(lang string) string {
	if lang == "" {
		return "[tasks.<lang>] env"
	}
	return "[tasks." + lang + "] env"
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
	if flag, fixed, ok := flagCoversOnlyFirstPart(run); ok {
		return fmt.Errorf("%s %q would silently select too few tests: go test -run splits a pattern at each top-level | and / and matches every part as its own regex, "+
			"so the leading %s applies to the first part only (go test -list applies it to the whole pattern, so -list looks right). %s",
			what, run, flag, flagFixAdvice(run, flag, fixed))
	}
	return nil
}

// flagFixAdvice says how to respell a filter flagCoversOnlyFirstPart refused,
// offering only spellings this validator would itself accept: putting the flag
// on every part lengthens the filter, and past the 256-character limit the
// suggestion would only be refused in its turn.
func flagFixAdvice(run, flag, fixed string) string {
	var ways []string
	if runPattern.MatchString(fixed) {
		ways = append(ways, "put the flag on every part: "+fixed)
	}
	if !strings.Contains(fixed, "/") {
		if grouped := flag + "(" + strings.TrimPrefix(run, flag) + ")"; runPattern.MatchString(grouped) {
			ways = append(ways, "group the alternatives: "+grouped)
		}
	}
	if len(ways) == 0 {
		return "Put the flag on every part; the spelled-out form would pass the 256-character limit, so shorten or split the filter first."
	}
	advice := strings.Join(ways, ", or ")
	return strings.ToUpper(advice[:1]) + advice[1:]
}

// leadingFlagGroup is an inline flag group opening a pattern: (?i), (?is),
// (?-i), (?i-s). The scoped form (?i:…) is a group, not a leading flag.
var leadingFlagGroup = regexp.MustCompile(`^\(\?[a-zA-Z-]+\)`)

// flagCoversOnlyFirstPart reports whether run opens with an inline flag group
// that go test -run will apply to the first part of the pattern only, and if so
// returns the flag and the pattern with the flag put on every part.
//
// Go's testing package (splitRegexp) splits a -run pattern at each top-level |
// into alternatives, and each alternative at each top-level / into subtest
// levels, then compiles every part as its own regex. `(?i)write|delete` is
// therefore case-insensitive for `write` only, and `(?i)TestFoo/bar` for the
// top-level name only. The tests that then run are a silent subset, which can
// pass a mutant its real tests would kill — mutation_test reports it SURVIVED.
func flagCoversOnlyFirstPart(run string) (flag, fixed string, ok bool) {
	flag = leadingFlagGroup.FindString(run)
	if flag == "" {
		return "", "", false
	}
	var out strings.Builder
	bare := false // a part the flag does not reach
	for _, p := range splitRunParts(run) {
		// An empty part (`TestFoo/` runs every subtest) has nothing for a flag
		// to change, and a part opening with its own flag group — (?i)… or a
		// scoped (?i:…) — has chosen its flags.
		if p.text != "" && !strings.HasPrefix(p.text, "(?") {
			out.WriteString(flag)
			bare = true
		}
		out.WriteString(p.text)
		out.WriteString(p.sep)
	}
	if !bare {
		return "", "", false
	}
	return flag, out.String(), true
}

// runPart is one part of a -run pattern and the separator that ended it
// ("|", "/", or "" for the last).
type runPart struct{ text, sep string }

// splitRunParts splits a -run pattern exactly where testing.splitRegexp does:
// at a top-level | or /, tracking a [ ] nesting count (an unmatched ] is legal)
// and counting parentheses only outside a class. runPattern refuses a
// backslash, so there is no escape to skip.
func splitRunParts(run string) []runPart {
	var parts []runPart
	start, cs, cp := 0, 0, 0
	for i := range len(run) {
		switch run[i] {
		case '[':
			cs++
		case ']':
			if cs--; cs < 0 {
				cs = 0
			}
		case '(':
			if cs == 0 {
				cp++
			}
		case ')':
			if cs == 0 {
				cp--
			}
		case '|', '/':
			if cs == 0 && cp == 0 {
				parts = append(parts, runPart{run[start:i], string(run[i])})
				start = i + 1
			}
		}
	}
	return append(parts, runPart{run[start:], ""})
}
