package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// toolAlias is the canonical tool a retired tool name is served by, plus the
// optional argument adapter that reshapes the old call onto the survivor's
// schema (nil = pass the arguments through unchanged).
//
// adapt receives the decoded arguments object and returns the object to
// re-encode. It may mutate and return its input.
type toolAlias struct {
	canonical string
	adapt     func(map[string]any) map[string]any
}

// toolAliases is the PERMANENT compatibility layer for tools that have been
// merged into a survivor: the retired name keeps working at the MCP layer,
// resolved to the canonical tool with a notice on the result.
//
// Aliases are deliberately absent from tools/list, the selftest coverage
// groups, and the advertised schemas — hidden-but-callable, exactly the
// semantics ToolFilter already has for a profile-hidden tool (see the
// tools/list comment in server_handlers.go). They exist so an agent working
// from a stale tool list, a cached prompt, or muscle memory does not hit a
// hard `unknown tool` wall; they are not a second name plumb advertises.
//
// Matching is EXACT — no fuzzy serving. A near-miss falls through to the
// unknown-tool rejection, which offers a "did you mean" hint over the union of
// registered names and these aliases.
//
// Behaviour deltas the caller inherits by being redirected:
//   - version → daemon_info: the answer is a superset (daemon_info reports the
//     go runtime and os/arch alongside the session and daemon state), so
//     nothing the old tool reported is lost.
//   - list_symbols → file_outline: the output SHAPE differs (a signature
//     skeleton with line ranges, not a name/kind tree with a count header),
//     the file must exist on disk and be under the 2 MiB outline cap, and
//     include_signatures is dropped — the outline always renders signature
//     lines, so the flag has no counterpart to carry.
//   - find_symbol → workspace_symbols: both parameters survive unchanged, so
//     the adapter is nil. uri is now OPTIONAL on the survivor: a uri-bearing
//     call gets the same single-document search, and a uri-less call — which
//     find_symbol rejected with a redirect — now runs the workspace-wide
//     search that redirect pointed at.
var toolAliases = map[string]toolAlias{
	"version":      {canonical: "daemon_info"},
	"list_symbols": {canonical: "file_outline", adapt: dropArgs("include_signatures")},
	"find_symbol":  {canonical: "workspace_symbols"},
}

// dropArgs builds an adapter that removes parameters the canonical tool does
// not declare. Silently dropping is deliberate: the alias exists to keep an old
// call working, and the argument guard would otherwise reject the whole call
// for an unknown parameter.
func dropArgs(keys ...string) func(map[string]any) map[string]any {
	return func(args map[string]any) map[string]any {
		for _, k := range keys {
			delete(args, k)
		}
		return args
	}
}

// resolveToolAlias maps a retired tool name onto its canonical tool and adapts
// the arguments. aliased is false (and the inputs are returned unchanged) when
// name is not an alias.
//
// It must run BEFORE the registry lookup in handleToolsCall: the canonical name
// is what flows into the hooks, resolveToolArgs (whose argShapes are keyed by
// the canonical name — an unresolved alias would silently skip parameter-alias
// resolution), execTool, and the recorded stats.
func resolveToolAlias(name string, args json.RawMessage) (canonical string, out json.RawMessage, aliased bool) {
	al, ok := toolAliases[name]
	if !ok {
		return name, args, false
	}
	if al.adapt == nil {
		return al.canonical, args, true
	}
	obj := map[string]any{}
	if trimmed := bytes.TrimSpace(args); len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			// Not a JSON object — leave the arguments untouched so the canonical
			// tool's own validation reports the real problem.
			return al.canonical, args, true
		}
	}
	adapted, err := json.Marshal(al.adapt(obj))
	if err != nil {
		return al.canonical, args, true
	}
	return al.canonical, adapted, true
}

// toolAliasNotice is the leading note prepended to a successful result when a
// tool-name alias was used, nudging the caller onto the canonical tool without
// failing the call. It coexists with aliasNotice (the parameter-alias note),
// which it precedes.
func toolAliasNotice(alias, canonical string) string {
	return "note: " + alias + " is a tool-name alias served by " + canonical +
		" — call " + canonical + " directly.\n\n"
}

// toolNameCandidates is the union of the registered tool names and the alias
// names, sorted so a "did you mean" suggestion is deterministic despite Go's
// randomised map iteration.
func (s *Server) toolNameCandidates() []string {
	s.mu.RLock()
	names := make([]string, 0, len(s.order)+len(toolAliases))
	names = append(names, s.order...)
	s.mu.RUnlock()
	for name := range toolAliases {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// unknownToolMessage formats the unknown-tool rejection, adding a "did you
// mean" hint when the name is a plausible typo of a registered tool or an
// alias — the same shape unknownErr uses for an unknown parameter. When nothing
// is close enough, the message is the bare original.
func (s *Server) unknownToolMessage(name string) string {
	msg := "unknown tool: " + name
	if suggestion := closest(name, s.toolNameCandidates()); suggestion != "" {
		msg += fmt.Sprintf("; did you mean %q?", suggestion)
	}
	return msg
}
