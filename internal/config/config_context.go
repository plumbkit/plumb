package config

// ContextConfig is the [context] section: plumb's advisory context hints, the
// selector-only blocks lifecycle hooks put in front of an agent when it names
// code in a prompt, resumes, compacts or starts a subagent (PLAN-462).
//
// Global only. The daemon answers every hook from the global config, so a
// project's value has no reader (classified ClassInert). The environment's
// PLUMB_CONTEXT_HINTS=off turns hints off too, and is read in the hook process
// as well, where a shell export reaches even when the daemon's does not.
type ContextConfig struct {
	// Hints gates every advisory context hint. Default true. Off, the hooks
	// still run and the daemon still records each request as noop(off), so the
	// switch is measurable rather than indistinguishable from an outage.
	Hints bool `toml:"hints"`
}
