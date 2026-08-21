package config

import (
	"os"
	"path/filepath"
	"testing"
)

func jsWorkspace(t *testing.T, files map[string]string) string {
	t.Helper()
	ws := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(ws, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return ws
}

func TestDetectJSPackageManager_Lockfiles(t *testing.T) {
	cases := []struct {
		file string
		want string
	}{
		{"pnpm-lock.yaml", "pnpm"},
		{"yarn.lock", "yarn"},
		{"bun.lockb", "bun"},
		{"bun.lock", "bun"},
		{"package-lock.json", "npm"},
		{"npm-shrinkwrap.json", "npm"},
	}
	for _, tc := range cases {
		pm, ok := detectJSPackageManager(jsWorkspace(t, map[string]string{tc.file: ""}))
		if !ok || pm.Name != tc.want {
			t.Errorf("%s → %q (declared=%v), want %q", tc.file, pm.Name, ok, tc.want)
		}
	}
}

// A repository that migrated off npm usually still carries package-lock.json.
// Treating it as decisive would reintroduce the wrong-runner bug, so the
// non-npm managers take precedence.
func TestDetectJSPackageManager_LeftoverNpmLockLoses(t *testing.T) {
	ws := jsWorkspace(t, map[string]string{"pnpm-lock.yaml": "", "package-lock.json": ""})
	pm, ok := detectJSPackageManager(ws)
	if !ok || pm.Name != "pnpm" {
		t.Errorf("got %q, want pnpm to win over a leftover package-lock.json", pm.Name)
	}
}

// The corepack field is an explicit statement; a lockfile can be a leftover.
func TestDetectJSPackageManager_CorepackFieldWinsOverLockfile(t *testing.T) {
	ws := jsWorkspace(t, map[string]string{
		"package.json":   `{"packageManager":"yarn@4.1.0"}`,
		"pnpm-lock.yaml": "",
	})
	pm, ok := detectJSPackageManager(ws)
	if !ok || pm.Name != "yarn" {
		t.Errorf("got %q, want yarn from the packageManager field", pm.Name)
	}
}

func TestDetectJSPackageManager_NothingDeclared(t *testing.T) {
	if _, ok := detectJSPackageManager(jsWorkspace(t, map[string]string{"package.json": `{}`})); ok {
		t.Error("a workspace declaring nothing must report undeclared, so the npm default stands")
	}
}

// Malformed input informs a default; it must never fail a config load.
func TestDetectJSPackageManager_MalformedInputIsNotAnError(t *testing.T) {
	for _, body := range []string{`{ not json`, `{"packageManager":""}`, `{"packageManager":"cargo@1"}`, ``} {
		ws := jsWorkspace(t, map[string]string{"package.json": body})
		if _, ok := detectJSPackageManager(ws); ok {
			t.Errorf("package.json %q should not declare a known manager", body)
		}
	}
}

func TestApplyJSPackageManagerDefaults_RewritesOnlyShippedDefaults(t *testing.T) {
	shipped := defaultTasks()["typescript"]
	ws := jsWorkspace(t, map[string]string{"pnpm-lock.yaml": ""})

	tasks := map[string]TasksConfig{"typescript": shipped}
	got := applyJSPackageManagerDefaults(tasks, ws)["typescript"]
	if got.Build != "pnpm run build" || got.Test != "pnpm run test" {
		t.Errorf("shipped defaults should follow the lockfile, got build=%q test=%q", got.Build, got.Test)
	}
}

// The whole safety property: a command someone wrote is never rewritten, even
// when it names a different manager than the lockfile does.
func TestApplyJSPackageManagerDefaults_NeverOverridesAChosenCommand(t *testing.T) {
	shipped := defaultTasks()["typescript"]
	ws := jsWorkspace(t, map[string]string{"pnpm-lock.yaml": ""})

	chosen := TasksConfig{Build: "npm run build --workspace=app", Test: shipped.Test}
	got := applyJSPackageManagerDefaults(map[string]TasksConfig{"typescript": chosen}, ws)["typescript"]
	if got.Build != "npm run build --workspace=app" {
		t.Errorf("a chosen build command must survive, got %q", got.Build)
	}
	if got.Test != "pnpm run test" {
		t.Errorf("the untouched test slot should still follow the lockfile, got %q", got.Test)
	}
}

func TestApplyJSPackageManagerDefaults_NoLockfileLeavesNpm(t *testing.T) {
	shipped := defaultTasks()["typescript"]
	ws := jsWorkspace(t, map[string]string{"package.json": `{}`})
	got := applyJSPackageManagerDefaults(map[string]TasksConfig{"typescript": shipped}, ws)["typescript"]
	if got.Build != shipped.Build || got.Test != shipped.Test {
		t.Errorf("with nothing declared the shipped npm defaults stand, got build=%q test=%q", got.Build, got.Test)
	}
}

// bun test runs bun's OWN test runner rather than the project's test script, so
// the default must spell `run`.
func TestJSPackageManagers_BunUsesRunNotBareTest(t *testing.T) {
	if got := jsPackageManagers["bun"].Test; got != "bun run test" {
		t.Errorf("bun test default = %q; a bare `bun test` runs bun's test runner, not the project's script", got)
	}
}

// End-to-end through the real project loader.
func TestLoadProject_TypescriptDefaultsFollowTheLockfile(t *testing.T) {
	ws := jsWorkspace(t, map[string]string{"pnpm-lock.yaml": "", "package.json": `{}`})
	if err := os.MkdirAll(filepath.Join(ws, ".plumb"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadProject(cloneConfig(defaults), ws)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if got := cfg.Tasks["typescript"].Build; got != "pnpm run build" {
		t.Errorf("resolved typescript build = %q, want pnpm run build", got)
	}
	if got := cfg.Tasks["go"].Build; got != defaultTasks()["go"].Build {
		t.Errorf("other languages must be untouched, go build = %q", got)
	}
}
