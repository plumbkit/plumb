package stats

import "testing"

func TestTokensSavedForClient_ClaudeCodeVsDesktop(t *testing.T) {
	// Claude Code has strong local file access; filesystem tools should save fewer tokens.
	// Claude Desktop has weak access; the same LSP tool saves more.
	ccDesktop := TokensSavedForClient("list_symbols", "claude-desktop", 0)
	ccCode := TokensSavedForClient("list_symbols", "claude-code", 0)
	if ccDesktop <= ccCode {
		t.Errorf("claude-desktop savings (%d) should exceed claude-code (%d) for list_symbols", ccDesktop, ccCode)
	}
}

func TestTokensSavedForClient_SearchInFiles(t *testing.T) {
	// search_in_files should have non-zero savings for claude-code but zero for claude-desktop.
	ccCode := TokensSavedForClient("search_in_files", "claude-code", 0)
	ccDesktop := TokensSavedForClient("search_in_files", "claude-desktop", 0)
	if ccCode == 0 {
		t.Error("claude-code search_in_files savings should be > 0")
	}
	if ccDesktop != 0 {
		t.Errorf("claude-desktop search_in_files savings = %d, want 0 (no desktop profile entry)", ccDesktop)
	}
}

func TestTokensSavedForClient_ZeroClamp(t *testing.T) {
	// When output bytes already exceed the alternative cost, savings clamp to 0.
	if got := TokensSavedForClient("list_symbols", "claude-desktop", 999999); got != 0 {
		t.Errorf("expected 0 for large output, got %d", got)
	}
}

func TestTokensSavedForClient_UnknownClientIsConservative(t *testing.T) {
	// Unknown clients use the conservative profile — lower than claude-desktop.
	unknown := TokensSavedForClient("list_symbols", "totally-unknown-client-xyz", 0)
	desktop := TokensSavedForClient("list_symbols", "claude-desktop", 0)
	if unknown >= desktop {
		t.Errorf("unknown client savings (%d) should be less than claude-desktop (%d)", unknown, desktop)
	}
	if unknown == 0 {
		t.Error("unknown client savings for list_symbols should be > 0")
	}
}

func TestNormaliseClient(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"claude-code", clientClaudeCode},
		{"claude-code/1.2.3", clientClaudeCode},
		{"Claude-Code", clientClaudeCode},
		{"claude", clientClaudeDesktop},
		{"claude-desktop", clientClaudeDesktop},
		{"CLAUDE", clientClaudeDesktop},
		{"codex", clientCodex},
		{"Codex CLI", clientCodex},
		{"gemini", clientGemini},
		{"gemini-cli", clientGemini},
		{"", clientUnknown},
		{"cursor", clientUnknown},
		{"vscode-mcp", clientUnknown},
	}
	for _, tc := range cases {
		if got := normaliseClient(tc.input); got != tc.want {
			t.Errorf("normaliseClient(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestHasSavingsModel_NewTools(t *testing.T) {
	// search_in_files now has a model (non-zero for claude-code/codex/unknown).
	if !HasSavingsModel("search_in_files") {
		t.Error("HasSavingsModel(search_in_files) = false, want true")
	}
	// filesystem tools that are universally zero should still return false.
	for _, tool := range []string{"read_file", "write_file", "git", "file_diff", "list_files"} {
		if HasSavingsModel(tool) {
			t.Errorf("HasSavingsModel(%s) = true, want false", tool)
		}
	}
}

func TestTokensSaved_BackwardCompatIsConservative(t *testing.T) {
	// TokensSaved (no client) uses the unknown/conservative profile.
	noClient := TokensSaved("list_symbols", 0)
	unknown := TokensSavedForClient("list_symbols", "unknown-client", 0)
	if noClient != unknown {
		t.Errorf("TokensSaved = %d, TokensSavedForClient(unknown) = %d; should be equal", noClient, unknown)
	}
}

func TestTokensSavedForClient_CallHierarchyHighForAll(t *testing.T) {
	// call_hierarchy is high-value for every client profile.
	clients := []string{"claude-desktop", "claude-code", "codex", "gemini", "unknown"}
	for _, c := range clients {
		if got := TokensSavedForClient("call_hierarchy", c, 0); got < 500 {
			t.Errorf("call_hierarchy savings for %q = %d, want >= 500", c, got)
		}
	}
}
