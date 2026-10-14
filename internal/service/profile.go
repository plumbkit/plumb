// Package service owns plumb's service launch profile (PLAN-528): the record of
// which binary is THE account's daemon, and which global directories it serves
// from.
//
// It exists because a launch used to be "whatever binary the client happened to
// be running, with whatever environment it happened to have". `plumb serve`
// started os.Executable() with cmd.Env nil, so a client with a spoofed HOME, a
// branch build or a worktree checkout could cold-start the account's ONE daemon
// as an arbitrary binary rooted in temporary directories — and every later
// client then talked to that daemon.
//
// The profile is service-owned state, not configuration: it is written by
// `plumb service install` (and, at the default flip, by `plumb setup` and a
// migrating `plumb restart`) and changed only through `plumb service set-*`. A
// launch that cannot find one REFUSES; it never infers an executable from the
// calling process, a PATH from a login shell, or a directory from the
// environment. The refusal is the feature: an inferred profile is exactly the
// defect this package removes.
//
// Concurrency: safe for concurrent use. Every value is read from one file per
// call, so a profile changed by `plumb service set-*` is visible to the next
// call in the same process — there is deliberately no cache to go stale.
package service

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/plumbkit/plumb/internal/account"
	"github.com/plumbkit/plumb/internal/fsync"
)

// The two ways a profile can be unusable, kept distinct because the remedies
// differ: a missing one is a cold install (`plumb service install`), a broken
// one is a repair of a file plumb itself wrote.
var (
	// ErrNoProfile reports that no launch profile exists. It is NEVER inferred
	// around: every caller that would launch a daemon must fail with the
	// `plumb service install` instruction instead.
	ErrNoProfile = errors.New("no plumb service launch profile")
	// ErrInvalidProfile reports a profile that exists but cannot be used.
	ErrInvalidProfile = errors.New("invalid plumb service launch profile")
)

// Role names one of the global directories the profile records. The names are
// the words the CLI and the docs use ("config", "data", "state", "log",
// "cache"), and they are TOML keys under [dirs].
type Role string

const (
	RoleConfig Role = "config"
	RoleData   Role = "data"
	RoleState  Role = "state"
	RoleLog    Role = "log"
	RoleCache  Role = "cache"
)

// Roles returns every recorded role, in the order the profile writes them.
// A function rather than a package-level slice so nothing can mutate the list.
func Roles() []Role {
	return []Role{RoleConfig, RoleData, RoleState, RoleLog, RoleCache}
}

// Dirs is the resolved directory for each global role: the directories every
// consumer of plumb's global state resolves through (CLI, TUI, hooks, ctrl,
// sandbox, doctor). They are absolute and complete — a profile never records a
// partial set, so a reader never has to fill one in.
type Dirs struct {
	Config string `toml:"config"`
	Data   string `toml:"data"`
	State  string `toml:"state"`
	Log    string `toml:"log"`
	Cache  string `toml:"cache"`
}

// Dir returns the directory recorded for role, or "" when role is unknown.
func (d Dirs) Dir(role Role) string {
	switch role {
	case RoleConfig:
		return d.Config
	case RoleData:
		return d.Data
	case RoleState:
		return d.State
	case RoleLog:
		return d.Log
	case RoleCache:
		return d.Cache
	default:
		return ""
	}
}

// With returns a copy of d with dir recorded for role. It is a value method
// like Dir, so a Dirs is either wholly read or wholly replaced — never half
// mutated through a receiver that might be a copy.
func (d Dirs) With(role Role, dir string) Dirs {
	switch role {
	case RoleConfig:
		d.Config = dir
	case RoleData:
		d.Data = dir
	case RoleState:
		d.State = dir
	case RoleLog:
		d.Log = dir
	case RoleCache:
		d.Cache = dir
	}
	return d
}

// XDG is the XDG base directories the profile has EXPLICITLY adopted. An empty
// field is not recorded: it means "no override", and the daemon then uses the
// account standard path for that role. Adoption is what makes a real XDG setup
// survive the move to an account-owned service: without it, the service would
// serve from ~/.config/plumb while the user's config sat in $XDG_CONFIG_HOME.
type XDG struct {
	ConfigHome string `toml:"XDG_CONFIG_HOME,omitempty"`
	DataHome   string `toml:"XDG_DATA_HOME,omitempty"`
	StateHome  string `toml:"XDG_STATE_HOME,omitempty"`
	CacheHome  string `toml:"XDG_CACHE_HOME,omitempty"`
	RuntimeDir string `toml:"XDG_RUNTIME_DIR,omitempty"`
}

// Vars returns the recorded variables in a fixed order, empty ones included so
// callers can report which are set. A variable the profile does not record is
// NOT passed to the daemon: a client's value for it is never adopted by
// accident.
func (x XDG) Vars() []EnvVar {
	return []EnvVar{
		{Name: "XDG_CONFIG_HOME", Value: x.ConfigHome},
		{Name: "XDG_DATA_HOME", Value: x.DataHome},
		{Name: "XDG_STATE_HOME", Value: x.StateHome},
		{Name: "XDG_CACHE_HOME", Value: x.CacheHome},
		{Name: "XDG_RUNTIME_DIR", Value: x.RuntimeDir},
	}
}

// Value returns the recorded value of the named XDG variable, or "" when it is
// not recorded. It is the read half of With, so a caller never has to know the
// field order.
func (x XDG) Value(name string) string {
	for _, v := range x.Vars() {
		if v.Name == name {
			return v.Value
		}
	}
	return ""
}

// With returns a copy of x with value recorded for the named XDG variable. An
// unknown name is ignored, so a caller cannot smuggle an arbitrary variable
// into the profile.
func (x XDG) With(name, value string) XDG {
	switch name {
	case "XDG_CONFIG_HOME":
		x.ConfigHome = value
	case "XDG_DATA_HOME":
		x.DataHome = value
	case "XDG_STATE_HOME":
		x.StateHome = value
	case "XDG_CACHE_HOME":
		x.CacheHome = value
	case "XDG_RUNTIME_DIR":
		x.RuntimeDir = value
	}
	return x
}

// EnvVar is one environment variable name and value.
type EnvVar struct {
	Name  string
	Value string
}

// Profile is the service launch profile: what to start, and where it serves
// from. The zero value is not usable — see Validate.
type Profile struct {
	// Executable is the installed binary a managed launch starts. It is the
	// path the installer WAS, deliberately not a symlink-resolved one: the
	// Homebrew shim /opt/homebrew/bin/plumb must keep naming the shim, or every
	// upgrade would leave the profile pointing into the previous Cellar.
	Executable string `toml:"executable"`
	// Path is the PATH the daemon and its children (language servers, git)
	// inherit. It is recorded at install rather than taken from the launching
	// client, so a client's PATH can never decide which gopls the daemon finds.
	Path string `toml:"path"`
	// Dirs are the resolved global directories.
	Dirs Dirs `toml:"dirs"`
	// XDG are the explicitly adopted XDG overrides, if any.
	XDG XDG `toml:"xdg"`
	// Env is the recorded environment for the daemon's children, [service.env]
	// (env.go). It is encoded by hand, so one variable stays on one line.
	Env Env `toml:"-"`
}

// Path returns the profile's location: <account data dir>/service.toml. It is
// the ACCOUNT's data directory in every mode — the profile is the account
// service's own record, so it must not move when a client's environment does.
func Path() (string, error) {
	u, err := account.Current()
	if err != nil {
		return "", fmt.Errorf("resolving the account for the service profile: %w", err)
	}
	return u.ServiceProfilePath(), nil
}

// Load reads the profile from its standard location.
func Load() (Profile, error) {
	path, err := Path()
	if err != nil {
		return Profile{}, err
	}
	return LoadFile(path)
}

// LoadFile reads and validates the profile at path. A missing file is
// ErrNoProfile, and nothing else is inferred from it.
func LoadFile(path string) (Profile, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the path is the account's own service profile, or a test's temp file
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Profile{}, fmt.Errorf("%w: %s", ErrNoProfile, path)
		}
		return Profile{}, fmt.Errorf("reading the service profile %s: %w", path, err)
	}
	var p Profile
	if err := toml.Unmarshal(data, &p); err != nil {
		return Profile{}, fmt.Errorf("%w: %s: %w", ErrInvalidProfile, path, err)
	}
	if p.Env, err = parseEnv(data); err != nil {
		return Profile{}, fmt.Errorf("%w: %s: %w", ErrInvalidProfile, path, err)
	}
	if err := p.Validate(); err != nil {
		return Profile{}, fmt.Errorf("%w: %s: %w", ErrInvalidProfile, path, err)
	}
	return p, nil
}

// Write records the profile at path, atomically and service-owned (0600).
func (p Profile) Write(path string) error {
	if err := p.Validate(); err != nil {
		return err
	}
	data, err := p.Marshal()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating the service profile directory: %w", err)
	}
	if err := fsync.AtomicWrite(path, data, fsync.Options{Mode: 0o600, Label: "service"}); err != nil {
		return fmt.Errorf("writing the service profile %s: %w", path, err)
	}
	return nil
}

// Marshal renders the profile as the TOML file's bytes, comment header
// included.
func (p Profile) Marshal() ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return marshalUnchecked(p)
}

// marshalUnchecked renders a profile without validating it. Only the test that
// proves the READER refuses a hand-edited file needs it.
func marshalUnchecked(p Profile) ([]byte, error) {
	body, err := toml.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("encoding the service profile: %w", err)
	}
	env, err := p.Env.marshal()
	if err != nil {
		return nil, err
	}
	return append(append([]byte(header), body...), env...), nil
}

// header is the file's own statement of who owns it. A user editing this file
// by hand is not the intended path — `plumb service set-*` is — and the file
// says so where they would look.
const header = `# plumb service launch profile.
#
# Written by ` + "`plumb service install`" + `. Change it with ` + "`plumb service set-*`" + `
# and ` + "`plumb service env`" + `,
# not by hand: every field here decides what the account's ONE daemon runs as
# and which directories it serves. A launch with no profile refuses to start.

`

// Validate reports whether the profile can be used. It checks shape only — an
// absolute executable, a non-empty PATH, five absolute directories — because
// that is what a reader can know without trusting the filesystem it is on.
// Whether the executable exists is checked at launch, where the error can name
// the remedy.
func (p Profile) Validate() error {
	if strings.TrimSpace(p.Executable) == "" {
		return errors.New("no executable recorded")
	}
	if !filepath.IsAbs(p.Executable) {
		return fmt.Errorf("executable %q is not an absolute path", p.Executable)
	}
	if strings.TrimSpace(p.Path) == "" {
		return errors.New("no PATH recorded")
	}
	for _, role := range Roles() {
		dir := p.Dirs.Dir(role)
		if dir == "" {
			return fmt.Errorf("no %s directory recorded", role)
		}
		if !filepath.IsAbs(dir) {
			return fmt.Errorf("%s directory %q is not an absolute path", role, dir)
		}
	}
	for _, v := range p.XDG.Vars() {
		if v.Value != "" && !filepath.IsAbs(v.Value) {
			return fmt.Errorf("%s %q is not an absolute path", v.Name, v.Value)
		}
	}
	return p.Env.validate()
}

// Default returns the profile a first install writes for u: the account's
// standard directories for every role, plus the executable and PATH the
// installer hands over. No XDG override is adopted here — adoption is the
// installer's decision, and it is explicit (see internal/cli's install path).
func Default(u account.User, executable, path string) Profile {
	p := Profile{Executable: executable, Path: path}
	p.Dirs = Dirs{
		Config: u.ConfigDir(),
		Data:   u.DataDir(),
		State:  u.StateDir(),
		Log:    u.LogDir(),
		Cache:  u.CacheDir(),
	}
	return p
}

// DirUnder returns plumb's own directory under an XDG base: the rule that turns
// a base directory into the global directory plumb uses. It lives here so that
// the installer's adoption and the profile's readers cannot disagree about it.
func DirUnder(base string) string {
	return filepath.Join(base, "plumb")
}

// Adopt returns a copy of p with the XDG overrides recorded and the directories
// they govern recomputed, so that a real XDG setup is preserved rather than
// silently abandoned. Only the variables in x are adopted; every other role
// keeps the account default.
func (p Profile) Adopt(u account.User, x XDG) Profile {
	p.XDG = x
	def := Default(u, p.Executable, p.Path).Dirs
	for _, role := range Roles() {
		if base := x.baseFor(role); base != "" {
			p.Dirs = p.Dirs.With(role, DirUnder(base))
			continue
		}
		p.Dirs = p.Dirs.With(role, def.Dir(role))
	}
	return p
}

// baseFor returns the XDG base that governs role, or "" when the profile has
// not adopted one. The log role follows the state base everywhere except
// darwin, where plumb's logs live under ~/Library/Logs and no XDG variable
// governs them.
func (x XDG) baseFor(role Role) string {
	switch role {
	case RoleConfig:
		return x.ConfigHome
	case RoleData:
		return x.DataHome
	case RoleState, RoleLog:
		if role == RoleLog && runtime.GOOS == "darwin" {
			return ""
		}
		return x.StateHome
	case RoleCache:
		return x.CacheHome
	default:
		return ""
	}
}

// localeEnv is the allowlist of variables carried from the launching session
// into the daemon: a human's locale is a property of their session, not of the
// client that started plumb, and a daemon whose children (language servers, git)
// cannot format a message is a worse daemon. Nothing else crosses — in
// particular no HOME, PATH, XDG_*, USER or PLUMB_* variable picked up by
// accident.
func localeEnv() []string {
	return []string{"LANG", "LANGUAGE", "LC_ALL", "LC_CTYPE", "LC_MESSAGES", "LC_TIME", "LC_NUMERIC", "LC_COLLATE", "LC_MONETARY"}
}

// seamEnv names the account test seam's variables (PLAN-527). They are carried
// into the launched daemon for one reason: a launcher and the daemon it starts
// must resolve the SAME account, and in a container harness the account is
// redirected by these. Dropping them would make the child resolve the real
// account while its launcher resolved the seam — two different services, which
// is precisely what the profile exists to prevent. They are inert in a release
// build: internal/account's untagged seam honours neither without
// testing.Testing(), and no shipped binary is built with -tags=plumbtest.
func seamEnv() []string {
	return []string{"PLUMB_TEST_ACCOUNT_ROOT", "PLUMB_ACCOUNT_AUTHORITY", "PLUMB_SEPARATE_ENV"}
}

// LaunchEnv builds the environment a managed launch gives the daemon: HOME and
// USER from the account database, PATH from the profile, XDG only as the
// profile recorded it, the profile's recorded [service.env] entries, the
// launching session's locale (unless an entry records that variable — the
// account's record wins), and the account test seam when one is in play.
//
// The [service.env] entries are applied from the profile alone. An inherited
// symbol was captured when it was configured; the CALLER's value for it is never
// read here, so a transient client cannot freeze its proxy or agent socket into
// the account's one daemon (PLAN-554).
//
// It is built from nothing, deliberately: the daemon does not inherit the
// calling client's environment, so no client variable can retarget the
// daemon's executable (the profile decides that), its global directories (the
// profile decides those) or the language servers it finds (the profile's PATH
// decides that).
func (p Profile) LaunchEnv(u account.User) []string {
	env := make([]string, 0, 8+len(localeEnv()))
	env = append(env, "HOME="+u.Home)
	if u.Name != "" {
		// USER is what most language servers and git read; an account the
		// database would not name gets no invented one.
		env = append(env, "USER="+u.Name)
	}
	env = append(env, "PATH="+p.Path)
	for _, v := range p.XDG.Vars() {
		if v.Value != "" {
			env = append(env, v.Name+"="+v.Value)
		}
	}
	env = append(env, p.Env.Applied()...)
	for _, name := range localeEnv() {
		if _, recorded := p.Env.Lookup(name); recorded {
			continue
		}
		if v, ok := os.LookupEnv(name); ok && v != "" {
			env = append(env, name+"="+v)
		}
	}
	for _, name := range seamEnv() {
		if v, ok := os.LookupEnv(name); ok && v != "" {
			env = append(env, name+"="+v)
		}
	}
	return env
}
