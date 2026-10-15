package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/account"
	"github.com/plumbkit/plumb/internal/paths"
	"github.com/plumbkit/plumb/internal/service"
)

// `plumb service` owns the account's launch profile (PLAN-528): the record of
// which binary is THE daemon, and which global directories it serves from.
//
// It exists because a launch used to be "whatever binary the client was running,
// with whatever environment it had". `install` is what a package manager runs
// after an upgrade; `set-*` is how a person changes one recorded setting without
// editing the file by hand. `plumb setup` writes it too, through the same
// function, because a `make install` or `go install` binary that never ran a
// package's post_install would otherwise have no profile at all — and a canonical
// cold start without one refuses (PLAN-518 MUST-FIX 4). Nothing else writes it.

func init() {
	serviceCmd.AddCommand(serviceInstallCmd, serviceShowCmd, serviceSetExecutableCmd, serviceSetPathCmd, serviceSetXDXCmd)
	// The XDG flags are registered from the same table the run path reads, so a
	// flag cannot exist without its variable or its variable without its flag.
	for _, f := range serviceSetXDGFlags {
		serviceSetXDXCmd.Flags().String(f.name, "", f.usage+" (an empty value drops it)")
	}
}

var serviceCmd = &cobra.Command{
	Use:   "service",
	Short: "Manage the account-wide plumb service (its launch profile)",
	Long: `The plumb daemon is one service per account: one binary, one set of global
directories, shared by every client. That is recorded in a launch profile, not
derived from whoever starts it — a client's HOME, XDG or PATH can never decide
which binary runs or where it keeps its files.

  plumb service install          record the installed binary (idempotent)
  plumb service show             print the profile
  plumb service set-executable   point the service at another installed binary
  plumb service set-path         change the PATH its language servers get
  plumb service set-xdg          adopt (or drop) an XDG base directory override
  plumb service env              the variables its children inherit ([service.env])

"install" is what a package manager runs after an upgrade: it updates the
recorded executable and leaves every other setting alone.`,
}

var serviceInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Record (or update) the service launch profile",
	Long: `Write the account's launch profile, or update the executable in an existing one.

Idempotent by construction, and safe to run after every upgrade: only the
executable is ever changed, so a user's adopted directories, PATH and XDG
overrides survive. Run it from the binary that should be the service.

The first install also seeds [service.env] from this shell: the proxy, CA
bundle, ssh-agent and Go module variables (see ` + "`plumb service env`" + `), each
captured now and never read from a client later. It never replaces an entry
you have, and it seeds once: a variable you unset stays unset.`,
	Args: cobra.NoArgs,
	RunE: runServiceInstall,
}

var serviceShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Print the service launch profile",
	Args:  cobra.NoArgs,
	RunE:  runServiceShow,
}

var serviceSetExecutableCmd = &cobra.Command{
	Use:   "set-executable <path>",
	Short: "Point the service at another installed binary",
	Args:  cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		return setProfileExecutable(args[0])
	},
}

var serviceSetPathCmd = &cobra.Command{
	Use:   "set-path <path>",
	Short: "Change the PATH the service's language servers get",
	Args:  cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		return setProfilePath(args[0])
	},
}

// The XDG variables a profile can adopt. Each flag both records and clears: an
// empty value (`--state-home=`) drops the override, and the role then follows
// the account standard path again.
var serviceSetXDGFlags = []struct {
	name  string
	usage string
}{
	{"config-home", "adopt XDG_CONFIG_HOME"},
	{"data-home", "adopt XDG_DATA_HOME"},
	{"state-home", "adopt XDG_STATE_HOME"},
	{"cache-home", "adopt XDG_CACHE_HOME"},
	{"runtime-dir", "adopt XDG_RUNTIME_DIR"},
}

var serviceSetXDXCmd = &cobra.Command{
	Use:   "set-xdg",
	Short: "Adopt (or drop) an XDG base directory override",
	Long: `Record an XDG base directory the service should serve from, or drop one it no
longer should.

This is the remedy when plumb tells you that your XDG setup differs from the
service's: with --config-home, --data-home, --state-home, --cache-home or
--runtime-dir you adopt the directory you actually use, and the service's own
directories follow it from then on. Passing an empty value drops the override:
the role goes back to the account standard path.`,
	Args: cobra.NoArgs,
	RunE: runServiceSetXDG,
}

func runServiceInstall(_ *cobra.Command, _ []string) error {
	return installServiceProfile(true)
}

// recordServiceProfileForSetup is what every `plumb setup` entry point calls
// first: claude-code, claude-desktop, the target-backed clients, --all and the
// interactive picker. It is a variable for one reason (PLAN-555): a dropped call
// at any of the five re-bricks those installs silently, because the next
// canonical cold start finds no profile and refuses, and the suite can only pin
// "each entry point makes the call" by substituting it.
var recordServiceProfileForSetup = ensureServiceProfileForSetup

// ensureServiceProfileForSetup records this binary in the account's launch
// profile on the way through `plumb setup`.
//
// `plumb setup` is the one command every non-Homebrew install runs — `make
// install` users, the ops reinstall loop, and `go install` (which has no
// post_install hook at all) — so it is where a first-run profile has to come
// from. Without it, the flip would leave those installs with the account
// authority ON and no profile: the next cold start refuses with
// `plumb service install`, which is the brick MUST-FIX 4 exists to prevent.
//
// It does nothing in legacy mode, where the profile is ignored entirely, so a
// user who opted out is never handed a file that nothing reads and no setup test
// grows a dependency on the account. It is deliberately NOT run by
// `--uninstall`: removing a client registration must not touch the service.
func ensureServiceProfileForSetup() error {
	if !paths.AuthorityEnabled() {
		return nil
	}
	if err := installServiceProfile(false); err != nil {
		return fmt.Errorf("recording the plumb service profile, which the account-wide daemon needs (`plumb service install` retries it): %w", err)
	}
	return nil
}

// installServiceProfile writes the account's launch profile (PLAN-528) and is
// the ONE writer of it, shared by `plumb service install`, whose remedy every
// other error names, and by `plumb setup`, which is how a non-Homebrew install
// comes to have one (PLAN-518 MUST-FIX 4: without it, `make install` and a
// `go install` binary would brick on their next cold start, because a canonical
// launch with no profile refuses rather than guessing which binary is THE
// daemon).
//
// It is IDEMPOTENT and PRESERVING, which is what lets a package manager's
// post_install run it on every upgrade: an existing profile keeps its
// directories, its PATH and its XDG settings, and only the recorded executable
// changes. verbose selects the long report `plumb service install` prints; the
// setup path prints one line.
func installServiceProfile(verbose bool) error {
	path, err := service.Path()
	if err != nil {
		return err
	}
	u, err := account.Current()
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving executable path: %w", err)
	}

	existing, err := service.LoadFile(path)
	switch {
	case err == nil:
		return updateInstalledProfile(path, existing, exe, verbose)
	case !errors.Is(err, service.ErrNoProfile):
		// install is the remedy every other error names, so it has to work on a
		// profile that cannot be read. Nothing can be preserved from a file that
		// does not parse, and it says so rather than pretending otherwise — but
		// the file is kept beside the new one first: a typo in a hand-edited
		// [service.env] must not cost the user every recorded setting.
		backup := path + ".bak"
		if berr := copyProfileBackup(path, backup); berr != nil {
			// A file that cannot even be read holds nothing install could keep;
			// refusing here would leave the remedy itself broken.
			fmt.Fprintf(os.Stderr, "plumb service: the profile at %s could not be read (%v) and could not be kept (%v); replacing it\n", path, err, berr)
		} else {
			fmt.Fprintf(os.Stderr, "plumb service: the profile at %s could not be read (%v); replacing it (the old file is kept at %s)\n", path, err, backup)
		}
	}

	p := freshProfile(u, exe)
	p.Env = seedServiceEnv(p.Env, shellLookup)
	if err := p.Write(path); err != nil {
		return err
	}
	fmt.Printf("plumb service: wrote %s\n", path)
	if names := p.Env.AppliedNames(); len(names) > 0 && !verbose {
		// The setup path prints one line, and it may run in an agent's or a
		// sandbox's shell: what it captured must be visible there too.
		fmt.Printf("plumb service: captured %s from this shell (`plumb service env show`)\n", strings.Join(names, ", "))
	}
	if verbose {
		fmt.Print("\n")
		fmt.Print(reportProfile(p))
	}
	return nil
}

// updateInstalledProfile is the upgrade path: the executable changes, a profile
// recorded before [service.env] existed is seeded once (seedServiceEnv), and
// every other recorded setting is left exactly as the user has it. A package's
// post_install runs this on every upgrade, so it must be safe to run at any
// time, repeatedly.
func updateInstalledProfile(path string, existing service.Profile, exe string, verbose bool) error {
	before := existing
	existing.Env = seedServiceEnv(existing.Env, shellLookup)
	existing.Executable = exe
	changes := describeChange(before, existing)
	if changes == "" && before.Env.Seeded == existing.Env.Seeded {
		if verbose {
			fmt.Printf("plumb service: %s is already current (executable %s)\n", path, exe)
		}
		return nil
	}
	if err := existing.Write(path); err != nil {
		return err
	}
	fmt.Printf("plumb service: updated %s\n%s", path, changes)
	if verbose {
		fmt.Print("  your directories, PATH, XDG settings and recorded environment were left alone\n")
	}
	return nil
}

// freshProfile is what a first install records: the account's standard
// directories, this binary, and this environment's PATH — plus whatever XDG
// overrides it is right to preserve.
func freshProfile(u account.User, exe string) service.Profile {
	p := service.Default(u, exe, os.Getenv("PATH"))
	return p.Adopt(u, adoptXDG(u, p.Dirs))
}

// adoptXDG picks the XDG overrides this environment is entitled to keep.
//
// Two rules, and both are needed. A variable is adopted only when it is
// absolute AND the plumb directory under it already exists: a session manager's
// throwaway temp directory holds no plumb state, so nothing is lost by not
// adopting it, and adopting it would hand the service a directory that vanishes
// with the session. And a variable that resolves where the account standard path
// already is is not recorded at all — an override that changes nothing is noise,
// and it would freeze the service to today's default.
func adoptXDG(u account.User, def service.Dirs) service.XDG {
	var x service.XDG
	for _, role := range service.Roles() {
		name, value, ok := paths.RoleEnvOverride(role)
		if !ok {
			continue
		}
		dir := service.DirUnder(value)
		if dir == def.Dir(role) || !isDir(dir) {
			continue
		}
		x = x.With(name, value)
	}
	// The runtime directory is not one of the five recorded roles: its XDG
	// variable is itself the location, and it is adopted on the XDG spec's own
	// terms (a directory this uid owns, mode 0700) rather than by holding plumb
	// state, because a runtime directory is empty by design.
	if v := os.Getenv("XDG_RUNTIME_DIR"); v != "" && filepath.IsAbs(v) &&
		account.UsableRuntimeBase(v, u.UID) && service.DirUnder(v) != u.RuntimeDir() {
		x.RuntimeDir = v
	}
	return x
}

func runServiceShow(_ *cobra.Command, _ []string) error {
	path, err := service.Path()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: the account's own service profile
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("no plumb service profile at %s — run `plumb service install`", path)
		}
		return fmt.Errorf("reading the service profile %s: %w", path, err)
	}
	fmt.Printf("%s\n\n", path)
	_, err = os.Stdout.Write(data)
	return err
}

func runServiceSetXDG(cmd *cobra.Command, _ []string) error {
	changed := map[string]string{}
	for _, f := range serviceSetXDGFlags {
		if !cmd.Flags().Changed(f.name) {
			continue
		}
		v, err := cmd.Flags().GetString(f.name)
		if err != nil {
			return err
		}
		changed[serviceVarFor(f.name)] = strings.TrimSpace(v)
	}
	return setXDGProfile(changed)
}

// setXDGProfile records the named XDG variables in the profile and recomputes
// the directories they govern. An empty value DROPS an override, so the role
// goes back to the account standard path.
//
// Only the roles a NAMED variable governs are recomputed. A set-* is a change to
// one setting, and a profile is not a place plumb may quietly reset the rest of:
// a directory the user arranged some other way survives a later set-xdg, exactly
// as it survives an install.
func setXDGProfile(changed map[string]string) error {
	if len(changed) == 0 {
		return errors.New("set-xdg needs at least one of --config-home, --data-home, --state-home, --cache-home, --runtime-dir")
	}
	for name, v := range changed {
		if v == "" {
			continue // dropping an override is always allowed
		}
		if !filepath.IsAbs(v) {
			return fmt.Errorf("%s needs an absolute directory, got %q", name, v)
		}
		if !isDir(v) {
			return fmt.Errorf("%s names %q, which is not an existing directory", name, v)
		}
	}
	return mutateProfile("xdg", func(p *service.Profile, u account.User) error {
		// The runtime directory is held to the XDG spec's own checks here too, not
		// only at install: the daemon binds plumb.sock and plumb.ctrl.sock there, so
		// a base this uid does not privately own would expose them. Installing
		// refuses such a base (adoptXDG); a set-* must not be the way around that.
		if v := changed["XDG_RUNTIME_DIR"]; v != "" && !account.UsableRuntimeBase(v, u.UID) {
			return fmt.Errorf("XDG_RUNTIME_DIR %q is not a usable runtime directory: it must already exist, "+
				"be a directory, be owned by uid %d and be mode 0700 — the daemon's sockets are bound there, "+
				"so a wider mode would expose them to every user on this machine", v, u.UID)
		}
		for name, v := range changed {
			p.XDG = p.XDG.With(name, v)
		}
		def := service.Default(u, p.Executable, p.Path).Dirs
		for _, role := range service.Roles() {
			name := paths.EnvVarForRole(role)
			if _, named := changed[name]; name == "" || !named {
				continue
			}
			if v := p.XDG.Value(name); v != "" {
				p.Dirs = p.Dirs.With(role, service.DirUnder(v))
				continue
			}
			p.Dirs = p.Dirs.With(role, def.Dir(role))
		}
		return nil
	})
}

// setProfilePath records the PATH the service's children inherit.
func setProfilePath(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return errors.New("set-path needs a non-empty PATH")
	}
	return mutateProfile("path", func(p *service.Profile, _ account.User) error {
		p.Path = v
		return nil
	})
}

// setProfileExecutable records the binary a managed launch starts.
func setProfileExecutable(exe string) error {
	exe = strings.TrimSpace(exe)
	if !filepath.IsAbs(exe) {
		return fmt.Errorf("set-executable needs an absolute path, got %q", exe)
	}
	if err := checkLaunchExecutable(exe); err != nil {
		return err
	}
	return mutateProfile("executable", func(p *service.Profile, _ account.User) error {
		p.Executable = exe
		return nil
	})
}

// serviceVarFor maps a flag name to the XDG variable it records. It is a
// function rather than a map so a flag cannot be added without its variable.
func serviceVarFor(flag string) string {
	switch flag {
	case "config-home":
		return "XDG_CONFIG_HOME"
	case "data-home":
		return "XDG_DATA_HOME"
	case "state-home":
		return "XDG_STATE_HOME"
	case "cache-home":
		return "XDG_CACHE_HOME"
	case "runtime-dir":
		return "XDG_RUNTIME_DIR"
	default:
		return ""
	}
}

// mutateProfile applies change to the recorded profile and writes it back, so
// every `set-*` shares one path: refuse without a profile, keep every field the
// change does not name, and report what changed.
func mutateProfile(what string, change func(*service.Profile, account.User) error) error {
	path, err := service.Path()
	if err != nil {
		return err
	}
	u, err := account.Current()
	if err != nil {
		return err
	}
	p, err := service.LoadFile(path)
	if err != nil {
		if errors.Is(err, service.ErrNoProfile) {
			return fmt.Errorf("no plumb service profile at %s — run `plumb service install` first: %w", path, err)
		}
		return fmt.Errorf("reading the service profile %s: %w (run `plumb service install` to rewrite it)", path, err)
	}
	before := p
	if err := change(&p, u); err != nil {
		return err
	}
	if err := p.Write(path); err != nil {
		return err
	}
	fmt.Printf("plumb service: updated the %s in %s\n", what, path)
	if d := describeChange(before, p); d != "" {
		fmt.Print(d)
	}
	return nil
}

// describeChange names the fields a mutation actually changed, so a `set-*`
// that quietly touched something else is visible in its own output.
func describeChange(before, after service.Profile) string {
	var b strings.Builder
	if before.Executable != after.Executable {
		fmt.Fprintf(&b, "  executable: %s\n           -> %s\n", before.Executable, after.Executable)
	}
	if before.Path != after.Path {
		fmt.Fprintf(&b, "  path: %s\n     -> %s\n", before.Path, after.Path)
	}
	for _, role := range service.Roles() {
		if bd, ad := before.Dirs.Dir(role), after.Dirs.Dir(role); bd != ad {
			fmt.Fprintf(&b, "  %s dir: %s\n        -> %s\n", role, bd, ad)
		}
	}
	for _, v := range after.XDG.Vars() {
		if was := before.XDG.Value(v.Name); was != v.Value {
			fmt.Fprintf(&b, "  %s: %q -> %q\n", v.Name, was, v.Value)
		}
	}
	b.WriteString(describeEnvChange(before.Env, after.Env))
	return b.String()
}

// reportProfile renders a freshly written profile for its installer.
func reportProfile(p service.Profile) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  executable: %s\n", p.Executable)
	fmt.Fprintf(&b, "  path:       %s\n", p.Path)
	for _, role := range service.Roles() {
		fmt.Fprintf(&b, "  %-6s dir: %s\n", role, p.Dirs.Dir(role))
	}
	for _, v := range p.XDG.Vars() {
		if v.Value != "" {
			fmt.Fprintf(&b, "  adopted %s=%s\n", v.Name, v.Value)
		}
	}
	if names := p.Env.AppliedNames(); len(names) > 0 {
		fmt.Fprintf(&b, "  env:       %s (captured from this shell; `plumb service env show`)\n", strings.Join(names, ", "))
	}
	return b.String()
}

// copyProfileBackup keeps a profile that is about to be replaced, byte for byte
// and private (0600), because a profile can carry credentials.
func copyProfileBackup(from, to string) error {
	data, err := os.ReadFile(from) //nolint:gosec // G304: the account's own service profile
	if err != nil {
		return err
	}
	return os.WriteFile(to, data, 0o600) //nolint:gosec // G703: the account's own service profile path plus ".bak"
}

// isDir reports whether path names an existing directory.
func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
