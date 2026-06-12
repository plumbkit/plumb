package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestEditorFieldWidth(t *testing.T) {
	tests := []struct {
		name            string
		screenW, needed int
		want            int
	}{
		{"unknown screen keeps default", 0, 80, listFieldWidth},
		{"short content keeps default", 120, 10, listFieldWidth},
		{"long content grows to fit", 120, 80, 80},
		{"cap at screen minus margin and chrome", 120, 200, 120 - 2*editorModalMargin - editorFieldChrome},
		{"tiny screen floors at 8", 20, 80, 8},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := editorFieldWidth(tt.screenW, tt.needed); got != tt.want {
				t.Errorf("editorFieldWidth(%d, %d) = %d, want %d", tt.screenW, tt.needed, got, tt.want)
			}
		})
	}
}

func TestSanitisePaste(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"plain text unchanged", "/Users/me/project", "/Users/me/project"},
		{"trailing newline trimmed", "/Users/me/project\n", "/Users/me/project"},
		{"interior newlines become spaces", "a\nb\r\nc", "a b  c"},
		{"control runes dropped", "a\x1b[31mb", "a[31mb"},
		{"non-ascii kept", "/Users/José/プロジェクト", "/Users/José/プロジェクト"},
		{"whitespace only collapses to empty", "\n\t \r\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitisePaste(tt.in); got != tt.want {
				t.Errorf("sanitisePaste(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestListEditorPaste(t *testing.T) {
	e := newListEditor(skReadRoots, "Read roots", []string{"/existing"})

	// A paste while browsing opens a fresh add input holding the pasted text.
	e.paste("/pasted/path")
	if !e.adding || e.editing {
		t.Fatalf("paste while browsing: adding=%v editing=%v, want adding only", e.adding, e.editing)
	}
	if e.input != "/pasted/path" {
		t.Fatalf("input = %q, want %q", e.input, "/pasted/path")
	}

	// A second paste while typing appends.
	e.paste("/more")
	if e.input != "/pasted/path/more" {
		t.Fatalf("input after second paste = %q, want %q", e.input, "/pasted/path/more")
	}
}

// TestHandlePasteMsg_Routing covers the priority order in handlePasteMsg: an
// open list editor receives the paste, but an open rename modal outranks it, and
// a paste with no text input active is dropped (never spilled into keybindings).
func TestHandlePasteMsg_Routing(t *testing.T) {
	t.Run("routes to the list editor when it is the only input", func(t *testing.T) {
		m := Model{settingsListEditor: newListEditor(skReadRoots, "Read roots", nil)}
		m = m.handlePasteMsg(tea.PasteMsg{Content: "  /pasted\n"})
		if m.settingsListEditor.input != "/pasted" {
			t.Fatalf("list editor input = %q, want %q (paste sanitised + routed)", m.settingsListEditor.input, "/pasted")
		}
	})

	t.Run("rename modal outranks the list editor", func(t *testing.T) {
		m := Model{
			renameModal:        &renameSessionModal{},
			settingsListEditor: newListEditor(skReadRoots, "Read roots", nil),
		}
		m = m.handlePasteMsg(tea.PasteMsg{Content: "name"})
		if m.renameModal.input != "name" {
			t.Fatalf("rename modal input = %q, want %q", m.renameModal.input, "name")
		}
		if m.settingsListEditor.input != "" {
			t.Fatalf("list editor must not receive the paste when a rename modal is open; got %q", m.settingsListEditor.input)
		}
	})

	t.Run("paste with no active input is dropped", func(t *testing.T) {
		m := Model{}
		m = m.handlePasteMsg(tea.PasteMsg{Content: "ignored"})
		if m.memoryFilter != "" || m.logFilter != "" {
			t.Fatalf("paste leaked into a filter: memoryFilter=%q logFilter=%q", m.memoryFilter, m.logFilter)
		}
	})
}
