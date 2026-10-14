package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/account"
)

// The PLAN-518 default flip's configuration half: `daemon.account_authority`
// defaults to ON, the environment overrides it, a project cannot set it, and
// Load PUBLISHES what it resolved to internal/account — which is where every
// global directory is resolved from, one layer below.

func TestDefaults_AccountAuthorityIsOn(t *testing.T) {
	if !Defaults().Daemon.AccountAuthority {
		t.Fatal("the compiled default must be the account authority: one daemon per account is the production behaviour")
	}
}

// authorityHelperEnv marks the child process below. It is not an environment
// variable production code reads: every run that does not set it skips.
const authorityHelperEnv = "PLUMB_TEST_AUTHORITY_RESOLUTION_HELPER"

// TestHelperLoadRecordsTheAccountAuthority is not a test. Run as a child process,
// it loads the configuration its environment points at and reports what the load
// DECIDED: the field it read, and the value it recorded in internal/account.
//
// It has to be a child because the recording is once-per-process by design (the
// gate is read once and a change needs a restart), so a test binary that has
// already loaded a config anywhere cannot load a second one and observe it.
func TestHelperLoadRecordsTheAccountAuthority(t *testing.T) {
	if os.Getenv(authorityHelperEnv) == "" {
		t.Skip("helper: the parent process runs this as a child with " + authorityHelperEnv + " set")
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Logf("HELPER account_authority=%t resolved=%t", cfg.Daemon.AccountAuthority, account.AuthorityResolved())
}

// loadInAChildProcess runs the helper above in a fresh process whose config
// directory is a temp tree holding fileBody, and returns what it decided.
func loadInAChildProcess(t *testing.T, fileBody string, env map[string]string) (cfgValue, resolved bool) {
	t.Helper()
	home := t.TempDir()
	cfgDir := filepath.Join(home, "config", "plumb")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", cfgDir, err)
	}
	if fileBody != "" {
		if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(fileBody), 0o600); err != nil {
			t.Fatalf("writing the config: %v", err)
		}
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperLoadRecordsTheAccountAuthority$", "-test.v")
	cmd.Env = []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
		"XDG_DATA_HOME=" + filepath.Join(home, "data"),
		"XDG_STATE_HOME=" + filepath.Join(home, "state"),
		"XDG_CACHE_HOME=" + filepath.Join(home, "cache"),
		authorityHelperEnv + "=1",
		"PATH=" + os.Getenv("PATH"),
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper process failed: %v\n%s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if _, rest, ok := strings.Cut(line, "HELPER account_authority="); ok {
			fields := strings.Fields(rest)
			if len(fields) != 2 {
				t.Fatalf("unparsable helper report %q", line)
			}
			return fields[0] == "true", strings.TrimPrefix(fields[1], "resolved=") == "true"
		}
	}
	t.Fatalf("the helper reported nothing; it answered with neither a value nor an error:\n%s", out)
	return false, false
}

// Load must hand the resolved value to internal/account, because that is what
// every global directory except the config directory is resolved from. Removing
// the recording leaves the directories on the compiled default while
// `plumb config show` reports the file's value — the two halves of one setting
// disagreeing, which is the whole defect this pins.
func TestLoad_RecordsTheAccountAuthorityForTheDirectories(t *testing.T) {
	cfgValue, resolved := loadInAChildProcess(t, "[daemon]\naccount_authority = false\n", nil)
	if cfgValue {
		t.Error("the file says false and the loaded config says true")
	}
	if resolved {
		t.Error("Load did not publish the resolved value: internal/account still reports the compiled default")
	}
}

func TestLoad_EnvironmentOverridesTheFileAndIsPublished(t *testing.T) {
	cfgValue, resolved := loadInAChildProcess(t, "[daemon]\naccount_authority = false\n",
		map[string]string{account.AuthorityEnvVar: "1"})
	if !cfgValue {
		t.Error("PLUMB_ACCOUNT_AUTHORITY=1 did not override the file")
	}
	if !resolved {
		t.Error("the environment override was not published to internal/account")
	}
}

// A project file must not be able to move the account's daemon, whatever it
// ships: the class table records the decision and forceGlobalOnlyToBase is its
// mechanism, so both halves are asserted here.
func TestLoadProject_CannotSetTheAccountAuthority(t *testing.T) {
	if got := projectFieldClasses["daemon.account_authority"]; got != ClassForcedGlobal {
		t.Fatalf("daemon.account_authority is classified %v, want ClassForcedGlobal", got)
	}
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".plumb"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "[daemon]\naccount_authority = false\n"
	if err := os.WriteFile(filepath.Join(ws, ".plumb", "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	base := Defaults() // the account authority is ON globally
	merged, err := LoadProject(base, ws)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if !merged.Daemon.AccountAuthority {
		t.Error("a project config turned the account authority off; a cloned repository must not be able to move the account's daemon")
	}
}

// The key is declared through the same sparse-save path as every other setting,
// so an explicit `false` round-trips and an untouched key stays out of the file.
func TestSaveSparse_RoundTripsTheAccountAuthority(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := SaveSparse(func(c *Config) { c.Daemon.AccountAuthority = false }); err != nil {
		t.Fatalf("SaveSparse: %v", err)
	}
	got := decodeGlobalConfigFile(t)
	want := map[string]any{"daemon": map[string]any{"account_authority": false}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sparse save wrote %v, want exactly %v", got, want)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load after the save: %v", err)
	}
	if cfg.Daemon.AccountAuthority {
		t.Error("the opt-out did not survive a round-trip through the config file")
	}
}
