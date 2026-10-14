package service

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The [service.env] section (PLAN-554): what it records, what a reader refuses or
// ignores, and what reaches the daemon — from the profile, never from the caller.

func envProfile() Profile {
	p := testProfile()
	p.Env = Env{Seeded: true}.
		With(EnvEntry{Name: "GOPRIVATE", Value: `github.com/acme/*,"quoted" \back`}).
		With(EnvEntry{Name: "SSH_AUTH_SOCK", Value: "/tmp/agent.sock", Inherit: true, Captured: true}).
		With(EnvEntry{Name: "HTTPS_PROXY", Inherit: true})
	return p
}

func TestEnv_WriteLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.toml")
	want := envProfile()
	if err := want.Write(path); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !reflect.DeepEqual(got.Env, want.Env) {
		t.Fatalf("round trip changed [service.env]:\n got %+v\nwant %+v", got.Env, want.Env)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// One variable, one line: the inline form a person reading the file expects.
	for _, want := range []string{"[service.env]", "SSH_AUTH_SOCK = { inherit = true, value = ", "HTTPS_PROXY = { inherit = true }"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("the file does not contain %q:\n%s", want, data)
		}
	}
}

// writeRaw writes a hand-edited profile: the valid base plus extra TOML.
func writeRaw(t *testing.T, extra string) string {
	t.Helper()
	base, err := testProfile().Marshal()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "service.toml")
	if err := os.WriteFile(path, append(base, []byte(extra)...), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoadFile_IgnoresADeniedEntry is the reader's half of the deny list: `env
// set` refuses these names, and a file somebody edited by hand is re-checked on
// every read, so the invariant does not rest on one writer being correct.
func TestLoadFile_IgnoresADeniedEntry(t *testing.T) {
	denied := []string{"HOME", "USER", "LOGNAME", "PATH", "XDG_CONFIG_HOME", "XDG_RUNTIME_DIR", "PLUMB_TEST_ACCOUNT_ROOT", "TSM_ORIG_XDG_DATA_HOME", "home", "Xdg_State_Home"}
	var extra strings.Builder
	extra.WriteString("\n[service.env]\nGOPROXY = \"https://proxy.example\"\n")
	for _, name := range denied {
		extra.WriteString(name + " = \"/hostile\"\n")
	}
	p, err := LoadFile(writeRaw(t, extra.String()))
	if err != nil {
		t.Fatalf("LoadFile: a denied entry must be ignored, not brick the profile: %v", err)
	}
	slices.Sort(denied)
	if !reflect.DeepEqual(p.Env.Ignored, denied) {
		t.Errorf("Ignored = %v, want %v", p.Env.Ignored, denied)
	}
	if names := p.Env.AppliedNames(); !reflect.DeepEqual(names, []string{"GOPROXY"}) {
		t.Errorf("applied %v, want only GOPROXY", names)
	}
	for _, kv := range p.LaunchEnv(testUser()) {
		if strings.HasSuffix(kv, "=/hostile") {
			t.Errorf("a denied entry reached the daemon: %s", kv)
		}
	}
}

func TestLoadFile_RefusesAMalformedEntry(t *testing.T) {
	cases := map[string]string{
		"a number":           "GOPROXY = 3",
		"inherit false":      "GOPROXY = { inherit = false, value = \"x\" }",
		"no inherit key":     "GOPROXY = { value = \"x\" }",
		"a non-string value": "GOPROXY = { inherit = true, value = 3 }",
		"an unknown key":     "GOPROXY = { inherit = true, from = \"x\" }",
		"an invalid name":    "\"GO-PROXY\" = \"x\"",
		"a NUL byte":         "GOPROXY = \"a\\u0000b\"",
	}
	for name, entry := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadFile(writeRaw(t, "\n[service.env]\n"+entry+"\n"))
			if !errors.Is(err, ErrInvalidProfile) {
				t.Fatalf("LoadFile = %v, want ErrInvalidProfile", err)
			}
		})
	}
}

// TestWrite_RefusesADeniedEntry: a writer that forgot CheckEnvName still cannot
// record a denied name, because Write validates.
func TestWrite_RefusesADeniedEntry(t *testing.T) {
	for _, name := range []string{"HOME", "PATH", "XDG_DATA_HOME", "PLUMB_X", "TSM_ORIG_XDG_CONFIG_HOME"} {
		p := testProfile()
		p.Env = p.Env.With(EnvEntry{Name: name, Value: "/x"})
		if err := p.Write(filepath.Join(t.TempDir(), "service.toml")); err == nil {
			t.Errorf("Write recorded the denied name %s", name)
		}
	}
}

// TestLaunchEnv_AppliesTheRecordedEnvironmentAndNeverTheCallers is the test that
// pins decision 2: a literal and a CAPTURED symbol reach the daemon; a symbol
// never captured does not; and a value present only in the SPAWNING client's
// environment never does — not for a name the profile captures differently, not
// for an uncaptured symbol, and not for a name it does not record at all.
func TestLaunchEnv_AppliesTheRecordedEnvironmentAndNeverTheCallers(t *testing.T) {
	t.Setenv("GOPRIVATE", "client-only")     // recorded as a literal
	t.Setenv("SSH_AUTH_SOCK", "client-only") // recorded as captured
	t.Setenv("HTTPS_PROXY", "client-only")   // recorded, never captured
	t.Setenv("GONOSUMDB", "client-only")     // not recorded at all

	env := envMap(envProfile().LaunchEnv(testUser()))
	if got := env["GOPRIVATE"]; got != `github.com/acme/*,"quoted" \back` {
		t.Errorf("GOPRIVATE = %q, want the recorded literal", got)
	}
	if got := env["SSH_AUTH_SOCK"]; got != "/tmp/agent.sock" {
		t.Errorf("SSH_AUTH_SOCK = %q, want the captured value", got)
	}
	for _, name := range []string{"HTTPS_PROXY", "GONOSUMDB"} {
		if v, ok := env[name]; ok {
			t.Errorf("%s=%q reached the daemon from the spawning client", name, v)
		}
	}
	for k, v := range env {
		if v == "client-only" {
			t.Errorf("%s carries the spawning client's value", k)
		}
	}
}

// TestApplied_RefusesADeniedEntryInMemory pins the second check: an Env built in
// memory, never read from a file, is held to the deny list too.
func TestApplied_RefusesADeniedEntryInMemory(t *testing.T) {
	p := testProfile()
	p.Env = Env{Entries: []EnvEntry{{Name: "HOME", Value: "/hostile"}, {Name: "XDG_CONFIG_HOME", Value: "/hostile"}}}
	if got := p.Env.Applied(); len(got) != 0 {
		t.Fatalf("Applied = %v, want nothing: every entry is denied", got)
	}
	if env := envMap(p.LaunchEnv(testUser())); env["HOME"] != testUser().Home {
		t.Fatalf("HOME = %q, want the account's %q", env["HOME"], testUser().Home)
	}
}

// TestLaunchEnv_ARecordedLocaleWins: the account's record decides, and the
// session's value for the same name is not appended beside it.
func TestLaunchEnv_ARecordedLocaleWins(t *testing.T) {
	t.Setenv("LANG", "session.UTF-8")
	p := testProfile()
	p.Env = p.Env.With(EnvEntry{Name: "LANG", Value: "en_AU.UTF-8"})
	var langs []string
	for _, kv := range p.LaunchEnv(testUser()) {
		if v, ok := strings.CutPrefix(kv, "LANG="); ok {
			langs = append(langs, v)
		}
	}
	if !reflect.DeepEqual(langs, []string{"en_AU.UTF-8"}) {
		t.Fatalf("LANG entries = %v, want only the recorded one", langs)
	}
}

func TestCapture_RecordsOnlyWhatTheShellHad(t *testing.T) {
	lookup := func(name string) (string, bool) {
		if name == "SET" {
			return "v", true
		}
		if name == "EMPTY" {
			return "", true
		}
		return "", false
	}
	if e := Capture("SET", lookup); !e.Inherit || !e.Captured || e.Value != "v" {
		t.Errorf("Capture(SET) = %+v", e)
	}
	for _, name := range []string{"EMPTY", "UNSET"} {
		if e := Capture(name, lookup); !e.Inherit || e.Captured || e.Applies() {
			t.Errorf("Capture(%s) = %+v, want an uncaptured symbol that applies nothing", name, e)
		}
	}
}

func TestCheckEnvName(t *testing.T) {
	for _, ok := range []string{"GOPRIVATE", "http_proxy", "_X1"} {
		if err := CheckEnvName(ok); err != nil {
			t.Errorf("CheckEnvName(%s) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "1X", "A-B", "A=B", "HOME", "PATH", "XDG_CACHE_HOME", "PLUMB_DEBUG", "LOGNAME", "USER", "TSM_ORIG_XDG_CONFIG_HOME"} {
		if err := CheckEnvName(bad); err == nil {
			t.Errorf("CheckEnvName(%q) accepted it", bad)
		}
	}
}
