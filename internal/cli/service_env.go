package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/account"
	"github.com/plumbkit/plumb/internal/service"
)

// `plumb service env` owns the profile's [service.env] (PLAN-554): the variables
// the daemon's children get on top of the clean launch environment. Every value
// is decided HERE, at configuration time, from the shell of the person running
// the command; the spawn path reads the profile and never the caller's
// environment, so a transient client cannot freeze its own proxy or agent socket
// into the account's one daemon.

func init() {
	serviceEnvCmd.AddCommand(serviceEnvShowCmd, serviceEnvSetCmd, serviceEnvUnsetCmd, serviceEnvRefreshCmd)
	serviceCmd.AddCommand(serviceEnvCmd)
}

var serviceEnvCmd = &cobra.Command{
	Use:   "env",
	Short: "Manage the environment the service's children inherit",
	Long: `The daemon starts from a clean environment: HOME and USER from the account,
PATH and XDG from the profile. Anything else its children need — git, the language
servers, run_task commands — is recorded here, in the profile's [service.env]:
a proxy, a custom CA bundle, an ssh-agent socket, private Go module settings.

  plumb service env show               print what is recorded, and what was captured
  plumb service env set NAME=VALUE     record a literal value
  plumb service env set NAME           record NAME from this shell (captured now)
  plumb service env unset NAME         stop passing NAME to the service
  plumb service env refresh            re-capture every NAME recorded from a shell

A captured value is frozen until the next refresh: the service never reads a
client's environment, so run refresh after your proxy or certificate path moves.
HOME, USER, LOGNAME, PATH, XDG_* and PLUMB_* can never be recorded here.
Restart the daemon (` + "`plumb restart`" + `) for a change to reach it.`,
}

var serviceEnvShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Print the recorded service environment",
	Args:  cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		p, err := loadProfileForEnv()
		if err != nil {
			return err
		}
		fmt.Print(renderServiceEnv(p.Env))
		return nil
	},
}

var serviceEnvSetCmd = &cobra.Command{
	Use:   "set NAME=VALUE | NAME",
	Short: "Record a variable for the service (a literal, or captured from this shell)",
	Args:  cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		return setServiceEnv(args[0], os.LookupEnv)
	},
}

var serviceEnvUnsetCmd = &cobra.Command{
	Use:   "unset NAME",
	Short: "Stop passing a variable to the service",
	Args:  cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		return unsetServiceEnv(args[0])
	},
}

var serviceEnvRefreshCmd = &cobra.Command{
	Use:   "refresh",
	Short: "Re-capture every variable recorded from a shell",
	Args:  cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		return refreshServiceEnv(os.LookupEnv)
	},
}

// setServiceEnv records one entry: `NAME=VALUE` is a literal, a bare `NAME` is an
// inherited symbol captured from lookup now. A denied name is refused with the
// reason, before anything is read or written.
func setServiceEnv(arg string, lookup func(string) (string, bool)) error {
	name, value, literal := strings.Cut(arg, "=")
	if err := service.CheckEnvName(name); err != nil {
		return err
	}
	entry := service.EnvEntry{Name: name, Value: value}
	if !literal {
		entry = service.Capture(name, lookup)
	}
	return mutateProfile("environment", func(p *service.Profile, _ account.User) error {
		p.Env = p.Env.With(entry)
		return nil
	})
}

func unsetServiceEnv(name string) error {
	return mutateProfile("environment", func(p *service.Profile, _ account.User) error {
		if _, ok := p.Env.Lookup(name); !ok {
			return fmt.Errorf("%s is not recorded in the service environment", name)
		}
		p.Env = p.Env.Without(name)
		return nil
	})
}

// refreshServiceEnv re-captures every inherited symbol from lookup. Literals are
// the user's own values and are never touched.
func refreshServiceEnv(lookup func(string) (string, bool)) error {
	return mutateProfile("environment", func(p *service.Profile, _ account.User) error {
		p.Env = recaptureEnv(p.Env, lookup)
		return nil
	})
}

func recaptureEnv(env service.Env, lookup func(string) (string, bool)) service.Env {
	for _, e := range env.Entries {
		if e.Inherit {
			env = env.With(service.Capture(e.Name, lookup))
		}
	}
	return env
}

// seedServiceEnv is install's half: the first time, every DefaultEnvNames entry is
// recorded as an inherited symbol captured from lookup. It is PRESERVING — an
// entry the user has (a literal, or a symbol they refreshed) is never replaced —
// and it runs once: a name the user unset afterwards is not seeded back by the
// next upgrade's post_install.
func seedServiceEnv(env service.Env, lookup func(string) (string, bool)) service.Env {
	if env.Seeded {
		return env
	}
	for _, name := range service.DefaultEnvNames() {
		if _, ok := env.Lookup(name); !ok {
			env = env.With(service.Capture(name, lookup))
		}
	}
	env.Seeded = true
	return env
}

func loadProfileForEnv() (service.Profile, error) {
	path, err := service.Path()
	if err != nil {
		return service.Profile{}, err
	}
	p, err := service.LoadFile(path)
	if errors.Is(err, service.ErrNoProfile) {
		return service.Profile{}, fmt.Errorf("no plumb service profile at %s — run `plumb service install` first", path)
	}
	return p, err
}

// renderServiceEnv prints what the service will get. It shows values — this is
// the person's own terminal, and a frozen value must be visible, not mysterious
// — and says which were captured and which never were.
func renderServiceEnv(env service.Env) string {
	var b strings.Builder
	if len(env.Entries) == 0 {
		b.WriteString("no variables are recorded for the service (`plumb service env set NAME` adds one)\n")
	}
	for _, e := range env.Entries {
		switch {
		case !e.Inherit:
			fmt.Fprintf(&b, "  %s=%s\n", e.Name, e.Value)
		case e.Captured:
			fmt.Fprintf(&b, "  %s=%s  (captured; `plumb service env refresh` re-captures)\n", e.Name, e.Value)
		default:
			fmt.Fprintf(&b, "  %s  (not set when captured: not passed)\n", e.Name)
		}
	}
	for _, name := range env.Ignored {
		fmt.Fprintf(&b, "  %s  IGNORED: %s\n", name, service.DeniedEnvName(name))
	}
	return b.String()
}

// describeEnvChange names the variables a mutation changed — names only: the
// values can carry credentials, and this output lands in terminals and logs.
func describeEnvChange(before, after service.Env) string {
	var b strings.Builder
	for _, e := range after.Entries {
		was, ok := before.Lookup(e.Name)
		switch {
		case !ok:
			fmt.Fprintf(&b, "  env %s: recorded\n", e.Name)
		case was != e:
			fmt.Fprintf(&b, "  env %s: changed\n", e.Name)
		}
	}
	for _, e := range before.Entries {
		if _, ok := after.Lookup(e.Name); !ok {
			fmt.Fprintf(&b, "  env %s: removed\n", e.Name)
		}
	}
	return b.String()
}
