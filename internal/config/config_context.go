package config

// ContextConfig is the [context] section: plumb's advisory context hints, the
// selector-only blocks lifecycle hooks put in front of an agent when it names
// code in a prompt, resumes, compacts or starts a subagent (PLAN-462).
//
// Global only. The daemon answers every hook from the global config, so a
// project's value has no reader (classified ClassInert). PLUMB_CONTEXT_HINTS
// overrides it either way (on|1|true, off|0|false), and is read in the hook
// process as well, where a shell export reaches even when the daemon's does not.
//
// Experimental: the hint-only hook handlers are installed only by `plumb hooks
// install --context`, and hints are off until turned on here or by the
// environment.
type ContextConfig struct {
	// Hints gates every advisory context hint. Default false. Off, any installed
	// hook still runs and the daemon records each request as noop(off), so the
	// switch is measurable rather than indistinguishable from an outage.
	Hints bool `toml:"hints"`
}
