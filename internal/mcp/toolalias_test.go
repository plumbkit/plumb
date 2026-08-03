package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestToolAliases_ExactMembership pins the whole alias table. Adding or
// removing a retired tool name is a reviewable event, not silent drift: the
// aliases are a permanent compatibility promise, so the set they cover must be
// stated in one place and asserted here.
func TestToolAliases_ExactMembership(t *testing.T) {
	want := map[string]string{
		"version":      "daemon_info",
		"list_symbols": "file_outline",
	}
	if len(toolAliases) != len(want) {
		t.Fatalf("toolAliases has %d entries, want exactly %d: %v", len(toolAliases), len(want), toolAliases)
	}
	for alias, canonical := range want {
		got, ok := toolAliases[alias]
		if !ok {
			t.Errorf("toolAliases is missing %q", alias)
			continue
		}
		if got.canonical != canonical {
			t.Errorf("toolAliases[%q].canonical = %q, want %q", alias, got.canonical, canonical)
		}
	}
}

// TestToolAliases_CanonicalsAreRegistered ties every alias to a tool that
// actually exists. selftestToolNames is the canonical registration list — the
// smoke parity guard pins it to the live tools/list — so an alias pointing at a
// tool that was itself removed or renamed fails here.
func TestToolAliases_CanonicalsAreRegistered(t *testing.T) {
	registered := map[string]bool{}
	for _, name := range selftestToolNames() {
		registered[name] = true
	}
	for alias, target := range toolAliases {
		if !registered[target.canonical] {
			t.Errorf("alias %q resolves to %q, which is not a registered tool", alias, target.canonical)
		}
	}
}

// TestToolAliases_AliasesAreNotRegistered guards the hidden-but-callable
// contract from the other side: an alias name must never also be a live tool.
// If one were re-added under its old name, handleToolsCall would rewrite the
// call away from it before the registry lookup and the real tool would be
// unreachable.
func TestToolAliases_AliasesAreNotRegistered(t *testing.T) {
	for _, name := range selftestToolNames() {
		if _, isAlias := toolAliases[name]; isAlias {
			t.Errorf("%q is registered AND a tool-name alias — the alias would shadow the real tool", name)
		}
	}
}

func TestResolveToolAlias(t *testing.T) {
	tests := []struct {
		name        string
		tool        string
		args        string
		wantCanon   string
		wantAliased bool
		wantArgs    string // "" => arguments must be returned unchanged
		absentArg   string
	}{
		{
			name:        "version passes arguments through",
			tool:        "version",
			args:        `{}`,
			wantCanon:   "daemon_info",
			wantAliased: true,
			wantArgs:    `{}`,
		},
		{
			name:        "list_symbols drops include_signatures and keeps uri",
			tool:        "list_symbols",
			args:        `{"uri":"/p/main.go","include_signatures":true}`,
			wantCanon:   "file_outline",
			wantAliased: true,
			wantArgs:    `{"uri":"/p/main.go"}`,
			absentArg:   "include_signatures",
		},
		{
			name:        "list_symbols tolerates null arguments",
			tool:        "list_symbols",
			args:        `null`,
			wantCanon:   "file_outline",
			wantAliased: true,
			wantArgs:    `{}`,
		},
		{
			name:        "non-object arguments are left for the tool to reject",
			tool:        "list_symbols",
			args:        `[1,2]`,
			wantCanon:   "file_outline",
			wantAliased: true,
			wantArgs:    `[1,2]`,
		},
		{
			name:      "a registered tool name is not an alias",
			tool:      "read_file",
			args:      `{"file_path":"/p/main.go"}`,
			wantCanon: "read_file",
			wantArgs:  `{"file_path":"/p/main.go"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			canon, args, aliased := resolveToolAlias(tt.tool, json.RawMessage(tt.args))
			if canon != tt.wantCanon {
				t.Errorf("canonical = %q, want %q", canon, tt.wantCanon)
			}
			if aliased != tt.wantAliased {
				t.Errorf("aliased = %v, want %v", aliased, tt.wantAliased)
			}
			if got := string(args); got != tt.wantArgs {
				t.Errorf("arguments = %s, want %s", got, tt.wantArgs)
			}
			if tt.absentArg != "" && strings.Contains(string(args), tt.absentArg) {
				t.Errorf("adapted arguments still carry %q: %s", tt.absentArg, args)
			}
		})
	}
}

// TestToolAliasNotice_Format pins the wording, which is a user-visible string
// an agent reads on every aliased call. It stays in the parameter-alias house
// voice ("note: … — <do this instead>") and names the canonical tool twice so
// the fix is unambiguous.
func TestToolAliasNotice_Format(t *testing.T) {
	const want = "note: version is a tool-name alias served by daemon_info — call daemon_info directly.\n\n"
	if got := toolAliasNotice("version", "daemon_info"); got != want {
		t.Errorf("toolAliasNotice = %q, want %q", got, want)
	}
}
