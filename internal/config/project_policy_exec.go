package config

import "strings"

// project_policy_exec.go covers the project-config sections that decide which
// PROCESS plumb spawns for a workspace, and the predicate that authorises them:
//
//   - [[command]]  — the allow-list run_command executes
//   - [commands]   — the execution policy for those commands (require_sandbox is
//                    free; every other key is gated, see policyCommandsFreeFields)
//   - [xcode]      — auto_build_server, which runs xcodebuild, and so this
//                    repository's own build
//
// These were the last capability-granting sections gated on the coarse per-root
// Trusted boolean rather than on a hash of the content the user approved
// (threat-model gap 8). Their [git], [lsp.<lang>] and [collab] siblings live in
// project_policy.go; they are here because the authorisation predicate is theirs
// alone — the other sections are enforced by forcing values back in LoadProject,
// while these are enforced at the resolver seam, where a project entry can still
// be told apart from a global one.
//
// Concurrency: pure functions over immutable values; the trust lookup is
// serialised inside TrustStore.

// execPolicyEntries extracts the sections that decide which PROCESS plumb
// spawns for a workspace: the [[command]] allow-list run_command executes, the
// [commands] execution policy governing it, and [xcode] auto_build_server,
// which spawns xcodebuild — that is, this repository's own build.
//
// These were the last capability-granting sections still gated on the coarse
// per-root Trusted boolean rather than on a hash of the content that was
// approved (threat-model gap 8). Two things followed from that, and both were
// live:
//
//   - Anything ADDED after a grant inherited it. A repository trusted for a
//     benign [git] tweak could append a [[command]] and have it run.
//   - The coarse flag is set by the TUI's Commands tab on ANY project-scope save
//     ("trusted by authorship"), so saving one unrelated setting in a freshly
//     cloned repository blessed every [[command]] that repository already
//     shipped, plus its [commands] policy, plus the xcode build server — none of
//     which the user authored or was shown.
//
// Putting them in the spec fixes both at once: they are hashed, so an edit
// invalidates the grant, and they are disclosed by `plumb trust`, so the grant
// is made with the argv in view.
//
// [[command]] is taken as ONE entry holding the whole array rather than one
// entry per command. encodePolicyValue already encodes arrays and nested tables
// injectively and order-sensitively, and order matters here — FindCommand takes
// the first match by name — so flattening to per-entry keys would lose exactly
// the property the hash needs.
func execPolicyEntries(raw map[string]any) ProjectPolicySpec {
	var out ProjectPolicySpec
	// Whole-value, any type: [[command]] is an array of tables, not a table.
	for _, cmds := range rawValues(raw, "command") {
		out = append(out, PolicyEntry{Key: "command", Value: cmds})
	}
	for _, commands := range rawTables(raw, "commands") {
		for k, v := range commands {
			if isFreeCommandsField(k) {
				continue
			}
			out = append(out, PolicyEntry{Key: "commands." + k, Value: v})
		}
	}
	// [xcode] is taken whole: auto_build_server decides whether xcodebuild runs at
	// all, and scheme and timeout are inputs to that same argv.
	for _, xcode := range rawTables(raw, "xcode") {
		for k, v := range xcode {
			out = append(out, PolicyEntry{Key: "xcode." + k, Value: v})
		}
	}
	return out
}

// policyCommandsFreeFields are the [commands] keys NOT gated on trust. Only
// require_sandbox qualifies: it is ClassOneWay, so a project may move it in the
// safe direction and nothing else (effectiveRequireSandbox takes the most
// restrictive of global and project). Gating it would demand a re-trust for a
// change that can only ADD safety.
//
// An ALLOW-list, like its [lsp] and [collab] siblings and for the same reason: a
// [commands] key added later is gated until someone decides otherwise, rather
// than free until someone remembers.
var policyCommandsFreeFields = map[string]bool{"require_sandbox": true}

// isFreeCommandsField reports whether a [commands] key is inert. Matched
// case-INSENSITIVELY, because go-toml/v2 binds a TOML key to a struct field that
// way.
func isFreeCommandsField(key string) bool { return policyCommandsFreeFields[strings.ToLower(key)] }

// ProjectExecTrusted reports whether this workspace may run the commands its
// project config supplies — the [[command]] allow-list, its [commands] policy,
// and the xcode build server.
//
// It is the CONJUNCTION of two grants, because they answer different questions:
//
//   - The coarse per-root flag answers "has the user approved this workspace for
//     execution at all". It is what the TUI's Commands tab sets on a
//     project-scope save, and what `plumb trust` sets.
//   - The policy hash answers "and is the request the same one they approved".
//     Empty spec means the project asks for nothing gated, so there is nothing
//     to have changed and the coarse grant alone is enough — which is the common
//     case, where the commands come from the user's own global config.
//
// Neither alone is sufficient. Without the hash, a repository trusted for one
// thing inherits the grant for anything added later, and a TUI save blesses
// whatever the clone already shipped. Without the coarse flag, a repository
// could be honoured on a policy grant made for an unrelated section.
//
// Any error — an unreadable project config, an unreadable trust store — fails
// CLOSED. A gate that cannot run must refuse, not pass.
//
// Callers holding a live session should prefer the decision resolved at config
// apply (sessionView.execTrusted), which is computed from the same bytes as the
// merged config it is authorising. This function re-reads, so it is for
// one-shot CLI surfaces (doctor, trust) with no loaded config to be consistent
// with.
func ProjectExecTrusted(workspace string) bool {
	st, err := ProjectPolicyStatusFor(workspace)
	if err != nil {
		return false
	}
	return ExecTrustedFor(workspace, st)
}

// ExecTrustedFor is ProjectExecTrusted against an ALREADY-RESOLVED status — the
// form a session uses, so the trust decision and the merged config it authorises
// come from one read of the file.
//
// Re-reading at the point of use would be a fresh TOCTOU in the other direction:
// a repository could load hostile content, then restore the file to content that
// IS trusted, and the check would pass while the loaded (hostile) commands are
// what actually run.
func ExecTrustedFor(workspace string, st ProjectPolicyStatus) bool {
	return projectPolicyTrust().IsTrusted(workspace) && st.InEffect()
}
