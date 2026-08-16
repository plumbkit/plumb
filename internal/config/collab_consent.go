package config

// collab_consent.go answers one question for a caller that has no single
// recipient to ask: does THIS OTHER workspace's own project config opt in to
// [collab] cross_project? It exists because a daemon-wide surface (the TUI and
// web dashboards) must resolve that opt-in for every workspace that shows up
// as a conversation participant, not just the one workspace a connection is
// pinned to — internal/tools' workspace_sessions_collab.go answers the
// single-recipient version of this question by reading the CALLER's own
// resolved [collab] snapshot, which has no equivalent when there is no one
// caller.

// TargetAllowsCrossProject reports whether workspace's own project config
// opts in to [collab] cross_project, honouring the same `plumb trust` gate
// LoadProject applies to every capability-granting field: an untrusted
// project cannot grant itself the cross-project channel by shipping a
// .plumb/config.toml that asks for it (see forceCapabilityFieldsToBase in
// project_policy.go). base is the caller's own resolved config, used as
// LoadProject's fallback base — normally the global default of false, unless
// the caller's own config.toml raised it.
//
// A workspace with no project config at all is the common case and resolves
// cleanly to base's own Collab.CrossProject (LoadProject's ordinary fallback).
// Fail-closed applies to the abnormal cases: an empty workspace, or one whose
// project config exists but will not parse, reports false rather than
// guessing. The callers this exists for — daemon-wide displays with no single
// recipient to ask — must treat "cannot determine consent" as "no consent".
func TargetAllowsCrossProject(base Config, workspace string) bool {
	if workspace == "" {
		return false
	}
	cfg, err := LoadProject(base, workspace)
	if err != nil {
		return false
	}
	return cfg.Collab.CrossProject
}
