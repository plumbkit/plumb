package separateenv

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type vector struct {
	Name     string            `json:"name"`
	Platform string            `json:"platform"`
	Env      map[string]string `json:"env"`
	Readable []string          `json:"readable"`
	Expect   *string           `json:"expect"`
}

func TestDetect_SharedVectors(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Cases []vector `json:"cases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Cases) == 0 {
		t.Fatal("no vectors")
	}
	// The vectors are shared with the Node copy of this rule, which names
	// platforms as process.platform does.
	nodePlatformToGOOS := map[string]string{"win32": "windows"}
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			goos := c.Platform
			if mapped, ok := nodePlatformToGOOS[goos]; ok {
				goos = mapped
			}
			got := Detect(Probe{
				Getenv:   func(k string) string { return c.Env[k] },
				GOOS:     goos,
				Readable: func(p string) bool { return slices.Contains(c.Readable, p) },
			})
			want := ""
			if c.Expect != nil {
				want = *c.Expect
			}
			if got != want {
				t.Fatalf("Detect = %q, want %q", got, want)
			}
		})
	}
}

func TestContainerMarkers_MatchTheVectors(t *testing.T) {
	if got, want := ContainerMarkers(), []string{"/.dockerenv", "/run/.containerenv"}; !slices.Equal(got, want) {
		t.Fatalf("ContainerMarkers = %v, want %v", got, want)
	}
}

func TestReadableFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "marker")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !ReadableFile(file) {
		t.Error("an empty regular file (Docker writes /.dockerenv empty) must be readable")
	}
	if ReadableFile(dir) {
		t.Error("a directory is not a marker")
	}
	if ReadableFile(filepath.Join(dir, "missing")) {
		t.Error("a missing path is not a marker")
	}
	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "missing"), dangling); err != nil {
		t.Fatal(err)
	}
	if ReadableFile(dangling) {
		t.Error("a dangling symlink is not a marker")
	}
	locked := filepath.Join(dir, "locked")
	if err := os.WriteFile(locked, nil, 0o000); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 && ReadableFile(locked) {
		t.Error("a file this process cannot read is not a marker")
	}
}

// TestRequireForTestMain_RefusesOnAHost runs this test binary again as a child
// that calls RequireForTestMain with the CI and VM evidence removed, so only a
// container marker could let it through.
func TestRequireForTestMain_RefusesOnAHost(t *testing.T) {
	if os.Getenv("SEPARATEENV_CHILD") == "1" {
		RequireForTestMain("separateenv-child")
		os.Exit(0)
	}
	child := Probe{Getenv: func(string) string { return "" }, GOOS: "linux", Readable: ReadableFile}
	if Detect(child) != "" {
		t.Skip("this process runs in a container, so the child would be let through")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRequireForTestMain_RefusesOnAHost$") //nolint:gosec // re-runs this test binary
	cmd.Env = append(withoutKeys(os.Environ(), "GITHUB_ACTIONS", "PLUMB_SEPARATE_ENV"), "SEPARATEENV_CHILD=1")
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 {
		t.Fatalf("child: err=%v, want exit status 2\n%s", err, out)
	}
	if !strings.Contains(string(out), "separateenv-child: refusing to run here") {
		t.Fatalf("child output lacks the refusal:\n%s", out)
	}
}

func withoutKeys(env []string, keys ...string) []string {
	out := env[:0:0]
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(keys, key) {
			out = append(out, kv)
		}
	}
	return out
}
