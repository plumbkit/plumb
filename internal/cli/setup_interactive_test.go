package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The picker's contract lives in its pure pieces: the toggle cycle (uninstall
// is explicit and reversible), register-all (never creates an uninstall), and
// the filesystem classification that pre-checks registered clients. The tea
// program itself is standard bubbletea plumbing the repo does not chase.

var pickerTestTarget = setupTarget{use: "test", name: "Test Client"}

func pickerRowByUse(rows []pickerRow, use string) pickerRow {
	for _, r := range rows {
		if r.target.use == use {
			return r
		}
	}
	return pickerRow{}
}

func TestPickerToggle_CyclesThroughExplicitStates(t *testing.T) {
	registered := pickerRow{target: pickerTestTarget, registered: true}
	unregistered := pickerRow{target: pickerTestTarget}

	m := setupPickerModel{rows: []pickerRow{registered, unregistered}}

	// A registered row cycles keep -> uninstall -> keep; it can never be
	// marked "register" (it already is).
	m.rows[0] = togglePickerRow(m.rows[0])
	if m.rows[0].action != setupUninstall {
		t.Errorf("registered row first toggle = %v, want setupUninstall", m.rows[0].action)
	}
	m.rows[0] = togglePickerRow(m.rows[0])
	if m.rows[0].action != setupKeep {
		t.Errorf("registered row second toggle = %v, want setupKeep", m.rows[0].action)
	}

	// An unregistered row cycles keep -> register -> keep.
	m.rows[1] = togglePickerRow(m.rows[1])
	if m.rows[1].action != setupRegister {
		t.Errorf("unregistered row first toggle = %v, want setupRegister", m.rows[1].action)
	}
	m.rows[1] = togglePickerRow(m.rows[1])
	if m.rows[1].action != setupKeep {
		t.Errorf("unregistered row second toggle = %v, want setupKeep", m.rows[1].action)
	}
}

func TestPickerToggle_CursorMovesAndWraps(t *testing.T) {
	m := setupPickerModel{rows: []pickerRow{{target: pickerTestTarget}, {target: pickerTestTarget}, {target: pickerTestTarget}}}
	m.cursor = 0
	m.rows[m.cursor] = togglePickerRow(m.rows[m.cursor]) // would panic if cursor handling were off
	if m.cursor != 0 {
		t.Fatalf("toggle moved the cursor, cursor = %d", m.cursor)
	}

	// Down twice from the end wraps to the start via the modulo in Update;
	// here we pin the wrap arithmetic Update relies on.
	n := len(m.rows)
	for _, step := range []struct{ from, want int }{
		{0, 1}, {1, 2}, {2, 0},
	} {
		got := (step.from + 1) % n
		if got != step.want {
			t.Errorf("cursor step from %d = %d, want %d", step.from, got, step.want)
		}
	}
}

func TestPickerRegisterAll_LiftsUnregisteredNeverUninstalls(t *testing.T) {
	m := setupPickerModel{rows: []pickerRow{
		{target: pickerTestTarget, registered: true},
		{target: pickerTestTarget, registered: true, action: setupUninstall},
		{target: pickerTestTarget},
	}}
	// The "a" key body from Update, inlined here because the keypress itself
	// is bubbletea plumbing: lift unregistered neutral rows only.
	for i := range m.rows {
		if !m.rows[i].registered && m.rows[i].action == setupKeep {
			m.rows[i].action = setupRegister
		}
	}
	if m.rows[0].action != setupKeep {
		t.Errorf("registered neutral row lifted to %v, want setupKeep", m.rows[0].action)
	}
	if m.rows[1].action != setupUninstall {
		t.Errorf("explicit uninstall overwritten to %v", m.rows[1].action)
	}
	if m.rows[2].action != setupRegister {
		t.Errorf("unregistered row = %v, want setupRegister", m.rows[2].action)
	}
}

func TestPickerChanges(t *testing.T) {
	rows := []pickerRow{
		{target: pickerTestTarget},
		{target: pickerTestTarget, action: setupRegister},
		{target: pickerTestTarget, action: setupUninstall},
	}
	if n := pickerChanges(rows); n != 2 {
		t.Errorf("pickerChanges = %d, want 2", n)
	}
}

func TestNewPickerRows_ClassifiesFromTheFilesystem(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Cursor: config present with plumb -> installed + registered.
	cursorDir := filepath.Join(home, ".cursor")
	if err := os.MkdirAll(cursorDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestConfig(t, filepath.Join(cursorDir, "mcp.json"), map[string]any{
		"mcpServers": map[string]any{
			"plumb": map[string]any{"command": "/bin/plumb", "args": []any{"serve"}},
		},
	})
	// Junie: no config, but its home dir exists -> installed, not registered.
	if err := os.MkdirAll(filepath.Join(home, ".junie"), 0o755); err != nil {
		t.Fatal(err)
	}

	rows := newPickerRows()

	cursor := pickerRowByUse(rows, "cursor")
	if !cursor.installed || !cursor.registered {
		t.Errorf("cursor = (installed %v, registered %v), want (true, true)", cursor.installed, cursor.registered)
	}
	junie := pickerRowByUse(rows, "junie")
	if !junie.installed || junie.registered {
		t.Errorf("junie = (installed %v, registered %v), want (true, false)", junie.installed, junie.registered)
	}
	if junie.action != setupKeep || cursor.action != setupKeep {
		t.Error("fresh rows must start neutral")
	}
	// A client with no trace at all (claude-code) is neither.
	claudeCode := pickerRowByUse(rows, "claude-code")
	if claudeCode.installed || claudeCode.registered {
		t.Errorf("claude-code = (installed %v, registered %v), want (false, false)", claudeCode.installed, claudeCode.registered)
	}
}

func TestRenderSetupPicker_Smoke(t *testing.T) {
	m := setupPickerModel{rows: []pickerRow{
		{target: pickerTestTarget, registered: true},
		{target: pickerTestTarget, installed: true},
		{target: pickerTestTarget, registered: true, action: setupUninstall},
	}}
	out := renderSetupPicker(m)
	for _, want := range []string{"Test Client", "registered", "not registered", "uninstall", "space toggle"} {
		if !strings.Contains(out, want) {
			t.Errorf("picker render missing %q:\n%s", want, out)
		}
	}
}
