package cli

import "reflect"

// The status half of `plumb hooks`'s client registry: how an entry plumb would
// install is classified against what is on disk. The writers and the ownership
// rules live in hooks_clients.go; the table that renders these states lives in
// hooks_status.go.

// hookState is one entry's classification for the status table and for the
// per-hook action lines the writers print.
type hookState struct {
	entry  hookEntry
	state  string
	detail string
}

// hookStatesAt classifies every entry plumb would install against what is on
// disk: missing (nothing of plumb's on that event), installed (byte-identical
// to what this binary writes), or stale (plumb's, but written by a different
// binary path or an older entry shape — including a legacy script hook).
func hookStatesAt(path string, entries []hookEntry, ours ownershipTest) ([]hookState, error) {
	cfg, isNew, err := readHookConfig(path)
	if err != nil {
		return nil, err
	}
	var hooks map[string]any
	if !isNew {
		hooks, _ = cfg["hooks"].(map[string]any)
	}

	out := make([]hookState, 0, len(entries))
	for _, e := range entries {
		found := findHookHandler(hooks, e.event, ours)
		switch {
		case found == nil:
			out = append(out, hookState{entry: e, state: hookStateMissing})
		case reflect.DeepEqual(found, e.handler):
			out = append(out, hookState{entry: e, state: hookStateInstalled})
		default:
			out = append(out, hookState{entry: e, state: hookStateStale, detail: staleHookDetail(found, e.handler)})
		}
	}
	return out, nil
}

// findHookHandler returns plumb's handler on one event, or nil. A nil hooks map
// (no config, or no "hooks" key) simply finds nothing.
func findHookHandler(hooks map[string]any, event string, ours ownershipTest) map[string]any {
	groups, ok := hooks[event].([]any)
	if !ok {
		return nil
	}
	for _, groupAny := range groups {
		group, ok := groupAny.(map[string]any)
		if !ok {
			continue
		}
		handlers, ok := group["hooks"].([]any)
		if !ok {
			continue
		}
		for _, handlerAny := range handlers {
			if handler, ok := handlerAny.(map[string]any); ok && ours(event, handler) {
				return handler
			}
		}
	}
	return nil
}

// staleHookDetail says why an installed entry is not the one this binary would
// write. A different command is the case that matters and the one a reader can
// act on — a moved binary, or a legacy script hook awaiting migration — so it
// is quoted verbatim rather than summarised.
func staleHookDetail(found, want map[string]any) string {
	foundCmd, _ := found["command"].(string)
	wantCmd, _ := want["command"].(string)
	if foundCmd != wantCmd {
		return "runs " + foundCmd
	}
	return "entry differs from this binary's — install refreshes it"
}
