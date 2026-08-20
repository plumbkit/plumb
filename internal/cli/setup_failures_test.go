package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A client whose config plumb cannot parse used to put the parser's whole
// message in the table's Config cell. The column is sized to the widest cell,
// so one such client stretched every row past the terminal width and wrapped
// the table into unreadability. These tests pin the split that fixed it: the
// row still reports "error" against its config path, and the reason travels on
// the row instead, for printing below the table.

// failingWriterTarget is a setupTarget whose config file exists but whose
// writer always fails — the shape the sweep hits on an unparseable config.
func failingWriterTarget(t *testing.T, name string, err error) setupTarget {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if writeErr := os.WriteFile(path, []byte("{}\n"), 0o600); writeErr != nil {
		t.Fatalf("seeding config: %v", writeErr)
	}
	return setupTarget{
		name:   name,
		pathFn: func() (string, error) { return path, nil },
		intoFn: func(string, string) (bool, []string, error) { return false, nil, err },
	}
}

func TestRefreshClient_ErrorRowKeepsPathAndCarriesReason(t *testing.T) {
	boom := errors.New("parsing config.yaml as YAML: yaml: line 107: did not find expected '-' indicator")
	rows, changed := refreshClient(failingWriterTarget(t, "Hermes", boom), "/new/plumb", true)
	if changed || len(rows) != 1 {
		t.Fatalf("got (%+v, %v), want one unchanged row", rows, changed)
	}
	row := rows[0]
	if row.status != "error" {
		t.Errorf("status = %q, want error — the failure still belongs in the list", row.status)
	}
	if !errors.Is(row.err, boom) {
		t.Errorf("row.err = %v, want the writer's error", row.err)
	}
	if strings.Contains(row.detail, "yaml:") {
		t.Errorf("the reason must stay out of the Config cell — it is what stretched the table: %q", row.detail)
	}
	if !strings.HasSuffix(row.detail, "config.json") {
		t.Errorf("an errored row must still name the config it failed on, got %q", row.detail)
	}
}

func TestRefreshClient_UnresolvablePathReportsErrorWithoutADetail(t *testing.T) {
	boom := errors.New("no home directory")
	c := setupTarget{
		name:   "Ghost",
		pathFn: func() (string, error) { return "", boom },
		intoFn: func(string, string) (bool, []string, error) { return true, nil, nil },
	}
	rows, changed := refreshClient(c, "/new/plumb", true)
	if changed || len(rows) != 1 || rows[0].status != "error" {
		t.Fatalf("got (%+v, %v), want one unchanged error row", rows, changed)
	}
	if !errors.Is(rows[0].err, boom) {
		t.Errorf("row.err = %v, want the path resolver's error", rows[0].err)
	}
	if rows[0].detail != "" {
		t.Errorf("there is no path to report when the resolver failed, got %q", rows[0].detail)
	}
}

func TestUninstallPickerRows_ErrorKeepsPathAndCarriesReason(t *testing.T) {
	boom := errors.New("backing up ~/x.json: no space left on device")
	path := filepath.Join(t.TempDir(), "config.json")
	c := setupTarget{
		name:   "Cursor",
		pathFn: func() (string, error) { return path, nil },
		outFn:  func(string) (bool, error) { return false, boom },
	}
	rows, notes := uninstallPickerRows(c)
	if len(notes) != 0 {
		t.Errorf("a failed removal claims no skills, got %v", notes)
	}
	if len(rows) != 1 || rows[0].status != "error" {
		t.Fatalf("got %+v, want one error row", rows)
	}
	if !errors.Is(rows[0].err, boom) {
		t.Errorf("row.err = %v, want the remover's error", rows[0].err)
	}
	if strings.Contains(rows[0].detail, "no space") {
		t.Errorf("the reason must stay out of the Config cell, got %q", rows[0].detail)
	}
}

// A multi-path client blanks the name on every row but the first, so a failure
// on a continuation row has no name of its own to print with.
func TestAppendSetupFailures_LabelsContinuationRows(t *testing.T) {
	rows := []clientRow{
		{name: "Claude Desktop", status: "already current", detail: "~/a.json"},
		{status: "error", detail: "~/b.json", err: errors.New("boom")},
	}
	got := appendSetupFailures(nil, "Claude Desktop", rows)
	if len(got) != 1 {
		t.Fatalf("got %+v, want exactly the errored row", got)
	}
	if got[0].client != "Claude Desktop" {
		t.Errorf("client = %q, want the target's name", got[0].client)
	}
}

func TestPrintSetupFailures(t *testing.T) {
	if out := captureStdout(t, func() { printSetupFailures(nil) }); out != "" {
		t.Errorf("a sweep with no failures must print no error section, got %q", out)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	failures := []setupFailure{{
		client: "Hermes",
		err:    fmt.Errorf("reading %s/.hermes/config.yaml: yaml: line 107: did not find expected '-' indicator", home),
	}}
	out := captureStdout(t, func() { printSetupFailures(failures) })

	if !strings.Contains(out, "Hermes") {
		t.Errorf("an error must name the client it belongs to, got %q", out)
	}
	if !strings.Contains(out, "line 107") {
		t.Errorf("the reason held back from the table must actually print, got %q", out)
	}
	if !strings.Contains(out, "1 error(s)") {
		t.Errorf("the section must count what it holds, got %q", out)
	}
	if !strings.Contains(out, "~/.hermes/config.yaml") || strings.Contains(out, home) {
		t.Errorf("home must be contracted to ~ so the paths read like the table's, got %q", out)
	}
}
