package service

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/pelletier/go-toml/v2"
)

// The profile's recorded environment, [service.env] (PLAN-554): the variables the
// daemon's CHILDREN need on a corporate or CI machine — a proxy, a custom CA
// bundle, an ssh-agent socket, a private-module rule — and which a launch built
// from nothing (LaunchEnv) would otherwise drop.
//
// They are a property of the ACCOUNT, like the executable and the PATH, so they
// are recorded, not inherited. An entry is either a LITERAL (`NAME = "VALUE"`) or
// an INHERITED SYMBOL (`NAME = { inherit = true, value = "..." }`), whose value
// was CAPTURED from the shell of the human configuring it — `plumb service env
// set NAME`, `env refresh` or `service install` — and is never read at spawn. A
// launch that read the caller's environment would let a transient client (a dev
// container, a session with a temporary proxy or token) freeze ITS values into
// the account's one daemon for every later client.

// EnvEntry is one recorded variable.
type EnvEntry struct {
	Name string
	// Value is the literal, or the value captured for an inherited symbol.
	Value string
	// Inherit marks an inherited symbol: refresh re-captures it.
	Inherit bool
	// Captured reports, for an inherited symbol, that the configuring shell had
	// it set. An uncaptured symbol is recorded (so a later refresh can pick it
	// up) and passes nothing to the daemon.
	Captured bool
}

// Applies reports whether the entry puts a variable into the daemon's
// environment: a literal always does, an inherited symbol only once captured.
func (e EnvEntry) Applies() bool { return !e.Inherit || e.Captured }

// Env is the [service.env] section, entries sorted by name.
type Env struct {
	// Seeded records that `plumb service install` has seeded DefaultEnvNames
	// once, so a name the user later unset is not seeded back by an upgrade.
	Seeded  bool
	Entries []EnvEntry
	// Ignored names the entries the reader dropped because they are denied: a
	// hand-edited file is re-checked on every read, not trusted.
	Ignored []string
}

// Lookup returns the entry recorded for name.
func (e Env) Lookup(name string) (EnvEntry, bool) {
	i := slices.IndexFunc(e.Entries, func(x EnvEntry) bool { return x.Name == name })
	if i < 0 {
		return EnvEntry{}, false
	}
	return e.Entries[i], true
}

// With returns a copy of e with entry recorded, replacing one of the same name.
func (e Env) With(entry EnvEntry) Env {
	out := slices.DeleteFunc(slices.Clone(e.Entries), func(x EnvEntry) bool { return x.Name == entry.Name })
	out = append(out, entry)
	slices.SortFunc(out, func(a, b EnvEntry) int { return strings.Compare(a.Name, b.Name) })
	e.Entries = out
	return e
}

// Without returns a copy of e with no entry for name.
func (e Env) Without(name string) Env {
	e.Entries = slices.DeleteFunc(slices.Clone(e.Entries), func(x EnvEntry) bool { return x.Name == name })
	return e
}

// Applied returns the NAME=VALUE pairs the entries put into the daemon's
// environment. A denied name is skipped here too, so the invariant does not rest
// on the reader alone: an Env built in memory is held to it as well.
func (e Env) Applied() []string {
	var out []string
	for _, x := range e.Entries {
		if x.Applies() && DeniedEnvName(x.Name) == "" {
			out = append(out, x.Name+"="+x.Value)
		}
	}
	return out
}

// AppliedNames returns the names Applied would set — what a log may say. The
// values are never logged: a proxy URL can carry credentials.
func (e Env) AppliedNames() []string {
	var out []string
	for _, x := range e.Entries {
		if x.Applies() && DeniedEnvName(x.Name) == "" {
			out = append(out, x.Name)
		}
	}
	return out
}

// DefaultEnvNames is the set `plumb service install` seeds by capture: the
// network, certificate, agent and module variables the daemon's children (git,
// language servers, run_task commands) need on a corporate machine. Both cases
// of the proxy variables, because tools disagree about which they read.
func DefaultEnvNames() []string {
	return []string{
		"http_proxy", "https_proxy", "no_proxy", "all_proxy",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY",
		"SSL_CERT_FILE", "SSL_CERT_DIR", "CURL_CA_BUNDLE", "REQUESTS_CA_BUNDLE", "NODE_EXTRA_CA_CERTS",
		"SSH_AUTH_SOCK", "GIT_SSH_COMMAND",
		"GOPRIVATE", "GONOSUMDB", "GOPROXY",
	}
}

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// DeniedEnvName returns why name may never be an entry, or "" when it may. The
// denied names are the ones that decide WHERE the daemon is — its account, its
// directories, its sockets and locks — which the account and the profile decide
// and no entry may override: HOME, USER, LOGNAME, PATH (the profile's own
// field), every XDG_* and PLUMB_* variable, and TSM_ORIG_*, which the path
// resolver reads to recover a session manager's hijacked XDG base. GIT_DIR and
// GIT_WORK_TREE are refused too: the daemon serves every workspace, and either
// one would redirect all of their git commands to a single repository.
func DeniedEnvName(name string) string {
	upper := strings.ToUpper(name)
	switch {
	case upper == "HOME" || upper == "USER" || upper == "LOGNAME":
		return name + " comes from the account database, never from an entry"
	case upper == "PATH":
		return "PATH is the profile's own setting: use `plumb service set-path`"
	case strings.HasPrefix(upper, "XDG_"):
		return name + " decides where the service keeps its files: use `plumb service set-xdg`"
	case strings.HasPrefix(upper, "PLUMB_"):
		return name + " is plumb's own control variable and is never recorded for the service"
	case strings.HasPrefix(upper, "TSM_ORIG_"):
		return name + " steers plumb's directory resolution and is never recorded for the service"
	case upper == "GIT_DIR" || upper == "GIT_WORK_TREE":
		return name + " would point every git command the service runs, in every workspace, at one repository"
	}
	return ""
}

// CheckEnvName reports whether name can be recorded at all: a valid variable
// name that is not denied.
func CheckEnvName(name string) error {
	if !envNamePattern.MatchString(name) {
		return fmt.Errorf("%q is not a valid environment variable name", name)
	}
	if why := DeniedEnvName(name); why != "" {
		return fmt.Errorf("%s cannot be recorded: %s", name, why)
	}
	return nil
}

// envFile is the on-disk shape of the [service] table. Entries are decoded loosely
// (a string or an inline table) and checked by parseEnv.
type envFile struct {
	Service struct {
		EnvSeeded bool           `toml:"env_seeded"`
		Env       map[string]any `toml:"env"`
	} `toml:"service"`
}

// parseEnv reads the [service] table out of a profile file's bytes. A malformed
// entry makes the profile invalid; a denied one is dropped and named in Ignored.
func parseEnv(data []byte) (Env, error) {
	var f envFile
	if err := toml.Unmarshal(data, &f); err != nil {
		return Env{}, err
	}
	env := Env{Seeded: f.Service.EnvSeeded}
	for name, raw := range f.Service.Env {
		if !envNamePattern.MatchString(name) {
			return Env{}, fmt.Errorf("[service.env] %q is not a valid environment variable name", name)
		}
		entry, err := parseEnvEntry(name, raw)
		if err != nil {
			return Env{}, err
		}
		if DeniedEnvName(name) != "" {
			env.Ignored = append(env.Ignored, name)
			continue
		}
		env = env.With(entry)
	}
	slices.Sort(env.Ignored)
	return env, nil
}

func parseEnvEntry(name string, raw any) (EnvEntry, error) {
	switch v := raw.(type) {
	case string:
		return EnvEntry{Name: name, Value: v}, checkEnvValue(name, v)
	case map[string]any:
		inherit, ok := v["inherit"].(bool)
		if !ok || !inherit {
			return EnvEntry{}, fmt.Errorf("[service.env] %s: a table entry must be { inherit = true, value = \"...\" }", name)
		}
		entry := EnvEntry{Name: name, Inherit: true}
		for key, val := range v {
			switch key {
			case "inherit":
			case "value":
				s, ok := val.(string)
				if !ok {
					return EnvEntry{}, fmt.Errorf("[service.env] %s: value must be a string", name)
				}
				entry.Value, entry.Captured = s, true
			default:
				return EnvEntry{}, fmt.Errorf("[service.env] %s: unknown key %q", name, key)
			}
		}
		return entry, checkEnvValue(name, entry.Value)
	default:
		return EnvEntry{}, fmt.Errorf("[service.env] %s must be a string or { inherit = true, value = \"...\" }", name)
	}
}

// checkEnvValue refuses what no process environment can carry, and what the
// profile cannot record faithfully: TOML strings are UTF-8, so invalid UTF-8
// would be rewritten silently on the way through the file.
func checkEnvValue(name, value string) error {
	if strings.ContainsRune(value, 0) {
		return fmt.Errorf("[service.env] %s: a value cannot contain a NUL byte", name)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("[service.env] %s: a value must be valid UTF-8", name)
	}
	return nil
}

// validate is Env's half of Profile.Validate: what a writer must never record.
func (e Env) validate() error {
	for _, x := range e.Entries {
		if err := CheckEnvName(x.Name); err != nil {
			return err
		}
		if err := checkEnvValue(x.Name, x.Value); err != nil {
			return err
		}
	}
	return nil
}

// marshal renders the [service] table. go-toml would write each inherited symbol
// as its own [service.env.NAME] sub-table; an inline table keeps one variable on
// one line, which is what a person reading the file expects.
func (e Env) marshal() (string, error) {
	if !e.Seeded && len(e.Entries) == 0 {
		return "", nil
	}
	var b strings.Builder
	b.WriteString("\n[service]\n")
	if e.Seeded {
		b.WriteString("env_seeded = true\n")
	}
	if len(e.Entries) == 0 {
		return b.String(), nil
	}
	b.WriteString("\n[service.env]\n")
	for _, x := range e.Entries {
		val, err := tomlString(x.Value)
		if err != nil {
			return "", err
		}
		switch {
		case !x.Inherit:
			fmt.Fprintf(&b, "%s = %s\n", x.Name, val)
		case x.Captured:
			fmt.Fprintf(&b, "%s = { inherit = true, value = %s }\n", x.Name, val)
		default:
			fmt.Fprintf(&b, "%s = { inherit = true }\n", x.Name)
		}
	}
	return b.String(), nil
}

// tomlString encodes s as a TOML string through the library, so escaping is
// never hand-rolled.
func tomlString(s string) (string, error) {
	out, err := toml.Marshal(map[string]string{"v": s})
	if err != nil {
		return "", fmt.Errorf("encoding a [service.env] value: %w", err)
	}
	enc, ok := strings.CutPrefix(strings.TrimSpace(string(out)), "v = ")
	if !ok {
		return "", errors.New("encoding a [service.env] value: unexpected encoder output")
	}
	return enc, nil
}

// Capture returns the inherited-symbol entry for name, captured from lookup —
// the configuring shell's environment. It is the ONE place a symbol's value is
// read, and it runs at configuration time only.
func Capture(name string, lookup func(string) (string, bool)) EnvEntry {
	v, ok := lookup(name)
	return EnvEntry{Name: name, Inherit: true, Value: v, Captured: ok && v != ""}
}
