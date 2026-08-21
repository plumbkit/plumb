package tui

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// fakeEnv and fakeLook stand in for the process environment and PATH, so the
// whole selection table can be exercised on a machine with no display server —
// and on CI, which has neither.
func fakeEnv(pairs map[string]string) func(string) string {
	return func(k string) string { return pairs[k] }
}

func fakeLook(installed ...string) func(string) (string, error) {
	set := make(map[string]string, len(installed))
	for _, name := range installed {
		set[name] = "/usr/bin/" + name
	}
	return func(name string) (string, error) {
		if p, ok := set[name]; ok {
			return p, nil
		}
		return "", errors.New("not found")
	}
}

func TestSelectClipboardMethod_WaylandPrefersWlCopy(t *testing.T) {
	m := selectClipboardMethod("linux",
		fakeEnv(map[string]string{"WAYLAND_DISPLAY": "wayland-1", "DISPLAY": ":0"}),
		fakeLook("wl-copy", "xclip", "xsel"))
	if m.kind != clipExec || m.name != "wl-copy" {
		t.Fatalf("want wl-copy on a Wayland session, got %+v", m)
	}
	if len(m.args) != 0 {
		t.Fatalf("wl-copy takes no args, got %v", m.args)
	}
}

// TestSelectClipboardMethod_WaylandFallsBackToXclipViaXWayland pins the
// deliberate fall-through: XWayland bridges the X11 CLIPBOARD selection into
// the compositor, so xclip still lands in a native Wayland paste.
func TestSelectClipboardMethod_WaylandFallsBackToXclipViaXWayland(t *testing.T) {
	m := selectClipboardMethod("linux",
		fakeEnv(map[string]string{"WAYLAND_DISPLAY": "wayland-1", "DISPLAY": ":0"}),
		fakeLook("xclip"))
	if m.kind != clipExec || m.name != "xclip" {
		t.Fatalf("want xclip via XWayland, got %+v", m)
	}
	if strings.Join(m.args, " ") != "-selection clipboard" {
		t.Fatalf("wrong xclip args: %v", m.args)
	}
}

// TestSelectClipboardMethod_WaylandWithoutXWaylandDoesNotReachForXclip covers a
// compositor started with XWayland disabled: DISPLAY is unset, and xclip there
// dies with "Can't open display", so picking it would be the silent failure
// this chain exists to remove.
func TestSelectClipboardMethod_WaylandWithoutXWaylandDoesNotReachForXclip(t *testing.T) {
	m := selectClipboardMethod("linux",
		fakeEnv(map[string]string{"WAYLAND_DISPLAY": "wayland-1"}),
		fakeLook("xclip", "xsel"))
	if m.kind != clipOSC52 {
		t.Fatalf("want OSC 52 with no XWayland, got %+v", m)
	}
	if m.installHint != "install wl-clipboard" {
		t.Fatalf("want the wl-clipboard hint, got %q", m.installHint)
	}
}

func TestSelectClipboardMethod_WaylandMissingHelpersHintsWlClipboard(t *testing.T) {
	m := selectClipboardMethod("linux",
		fakeEnv(map[string]string{"WAYLAND_DISPLAY": "wayland-1", "DISPLAY": ":0"}),
		fakeLook())
	if m.kind != clipOSC52 || m.installHint != "install wl-clipboard" {
		t.Fatalf("a Wayland user should be pointed at wl-clipboard, got %+v", m)
	}
}

func TestSelectClipboardMethod_X11PrefersXclipOverXsel(t *testing.T) {
	m := selectClipboardMethod("linux",
		fakeEnv(map[string]string{"DISPLAY": ":0"}),
		fakeLook("xclip", "xsel"))
	if m.kind != clipExec || m.name != "xclip" {
		t.Fatalf("want xclip, got %+v", m)
	}
}

func TestSelectClipboardMethod_X11UsesXselWhenXclipAbsent(t *testing.T) {
	m := selectClipboardMethod("linux",
		fakeEnv(map[string]string{"DISPLAY": ":0"}),
		fakeLook("xsel"))
	if m.kind != clipExec || m.name != "xsel" {
		t.Fatalf("want xsel, got %+v", m)
	}
	if strings.Join(m.args, " ") != "--clipboard --input" {
		t.Fatalf("wrong xsel args: %v", m.args)
	}
}

func TestSelectClipboardMethod_X11WithNoHelperHintsXclip(t *testing.T) {
	m := selectClipboardMethod("linux",
		fakeEnv(map[string]string{"DISPLAY": ":0"}),
		fakeLook())
	if m.kind != clipOSC52 || m.installHint != "install xclip" {
		t.Fatalf("want the xclip hint, got %+v", m)
	}
}

// TestSelectClipboardMethod_NoDisplayEnvUsesOSC52WithoutHint covers SSH and a
// bare TTY. OSC 52 is the right answer there, not a shortfall, so doctor must
// not be handed a hint it would render as a warning.
func TestSelectClipboardMethod_NoDisplayEnvUsesOSC52WithoutHint(t *testing.T) {
	m := selectClipboardMethod("linux", fakeEnv(nil), fakeLook("xclip", "wl-copy"))
	if m.kind != clipOSC52 {
		t.Fatalf("want OSC 52 with no display server, got %+v", m)
	}
	if m.installHint != "" {
		t.Fatalf("a headless box has nothing to install, got hint %q", m.installHint)
	}
}

func TestSelectClipboardMethod_DarwinUsesPbcopy(t *testing.T) {
	m := selectClipboardMethod("darwin", fakeEnv(nil), fakeLook("pbcopy"))
	if m.kind != clipExec || m.name != "pbcopy" || len(m.args) != 0 {
		t.Fatalf("want bare pbcopy, got %+v", m)
	}
}

func TestSelectClipboardMethod_DarwinWithoutPbcopyFallsToOSC52(t *testing.T) {
	m := selectClipboardMethod("darwin", fakeEnv(nil), fakeLook())
	if m.kind != clipOSC52 || m.installHint == "" {
		t.Fatalf("want OSC 52 with a hint naming pbcopy, got %+v", m)
	}
}

func TestSelectClipboardMethod_WindowsUsesOSC52(t *testing.T) {
	m := selectClipboardMethod("windows",
		fakeEnv(map[string]string{"DISPLAY": ":0"}),
		fakeLook("xclip"))
	if m.kind != clipOSC52 || m.installHint != "" {
		t.Fatalf("want a plain OSC 52 fallback on windows, got %+v", m)
	}
}

// TestSelectClipboardMethod_NeverReturnsAnUnverifiedBinary is the regression
// for the original defect: the old chain fell back to xsel without ever
// checking it was installed, so on a box with neither helper it ran a command
// that did not exist and discarded the error.
func TestSelectClipboardMethod_NeverReturnsAnUnverifiedBinary(t *testing.T) {
	helpers := []string{"wl-copy", "xclip", "xsel", "pbcopy"}
	for _, goos := range []string{"darwin", "linux", "windows"} {
		for _, wayland := range []string{"", "wayland-1"} {
			for _, display := range []string{"", ":0"} {
				for subset := range 1 << len(helpers) {
					var installed []string
					for i, h := range helpers {
						if subset&(1<<i) != 0 {
							installed = append(installed, h)
						}
					}
					env := fakeEnv(map[string]string{"WAYLAND_DISPLAY": wayland, "DISPLAY": display})
					m := selectClipboardMethod(goos, env, fakeLook(installed...))
					if m.kind != clipExec {
						continue
					}
					if !slices.Contains(installed, m.name) {
						t.Fatalf("goos=%s wayland=%q display=%q installed=%v picked uninstalled %q",
							goos, wayland, display, installed, m.name)
					}
					if m.path != "/usr/bin/"+m.name {
						t.Fatalf("path %q did not come from the lookup", m.path)
					}
				}
			}
		}
	}
}

// TestPopupFooterShowsCopyStatus covers the other half of the original defect:
// the call-detail popup's `c` key gave no feedback at all, in either focus
// state.
func TestPopupFooterShowsCopyStatus(t *testing.T) {
	RebuildStyles()
	for _, focus := range []bool{true, false} {
		m := Model{
			popupRightFocus: focus,
			copyStatus:      clipboardStatus{text: "Copy failed: wl-copy: exit status 1"},
		}
		lines := m.renderPopupBody(nil, nil, 3, 20, 44, nil, nil, 0)
		got := ansiStripForTest(strings.Join(lines, "\n"))
		if !strings.Contains(got, "Copy failed: wl-copy") {
			t.Fatalf("popupRightFocus=%v: copy status missing from the footer:\n%s", focus, got)
		}
		if strings.Contains(got, "c copy") {
			t.Fatalf("popupRightFocus=%v: status should replace the key hints:\n%s", focus, got)
		}
	}
}

func TestRunClipboardExec_ReportsFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("exec permissions behave differently on windows")
	}
	notRunnable := filepath.Join(t.TempDir(), "wl-copy")
	if err := os.WriteFile(notRunnable, []byte("not an executable"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := runClipboardExec(clipboardMethod{kind: clipExec, name: "wl-copy", path: notRunnable}, "text")
	if got.verified {
		t.Fatal("a helper that could not run was reported as verified")
	}
	if !strings.Contains(got.text, "wl-copy") {
		t.Fatalf("failure text should name the helper: %q", got.text)
	}
}

func TestRunClipboardExec_ReportsSuccess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no /bin/true on windows")
	}
	if _, err := os.Stat("/bin/true"); err != nil {
		t.Skip("/bin/true not available")
	}
	got := runClipboardExec(clipboardMethod{kind: clipExec, name: "wl-copy", path: "/bin/true"}, "text")
	if !got.verified || got.text != clipboardCopiedMsg {
		t.Fatalf("a helper that exited 0 should verify: %+v", got)
	}
}
