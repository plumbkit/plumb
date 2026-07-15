package xcodebsp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeRunner struct {
	root    string
	results []ExecResult
	calls   [][]string
	write   bool
}

func (f *fakeRunner) Run(_ context.Context, _ string, argv []string, _ time.Duration) (ExecResult, error) {
	f.calls = append(f.calls, append([]string(nil), argv...))
	result := f.results[len(f.calls)-1]
	if f.write && len(f.calls) == 2 {
		data := []byte("{\"name\":\"xcode build server\",\"argv\":[\"xcode-build-server\"],\"languages\":[\"swift\"]}")
		_ = os.WriteFile(filepath.Join(f.root, "buildServer.json"), data, 0o644)
	}
	return result, nil
}

func TestConfigureDisabledAndUntrustedRunNothing(t *testing.T) {
	for _, tc := range []ConfigureRequest{
		{Enabled: false, Trusted: true},
		{Enabled: true, Trusted: false},
	} {
		root := xcodeRoot(t)
		tc.Root = root
		runner := &fakeRunner{root: root}
		status := Configure(context.Background(), tc, runner)
		if len(runner.calls) != 0 {
			t.Fatalf("state %s ran %d commands", status.State, len(runner.calls))
		}
	}
}

func TestConfigureRefusesToOverwriteInvalidBuildServer(t *testing.T) {
	root := xcodeRoot(t)
	path := filepath.Join(root, "buildServer.json")
	if err := os.WriteFile(path, []byte(`{"name":"foreign","argv":["other-bsp"],"languages":["swift"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{root: root}
	status := Configure(context.Background(), ConfigureRequest{
		Root: root, Enabled: true, Trusted: true, Timeout: time.Minute,
	}, runner)
	if status.State != StateFailed || !strings.Contains(status.Detail, "refusing to overwrite") {
		t.Fatalf("status = %#v", status)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("invalid existing BSP ran commands: %#v", runner.calls)
	}
}

func TestConfigureGeneratesWithValidatedScheme(t *testing.T) {
	root := xcodeRoot(t)
	runner := &fakeRunner{
		root: root,
		results: []ExecResult{
			{Stdout: "{\"project\":{\"schemes\":[\"App\"]}}"},
			{},
		},
		write: true,
	}
	status := Configure(context.Background(), ConfigureRequest{
		Root: root, Enabled: true, Trusted: true, Timeout: time.Minute,
	}, runner)
	if status.State != StateConfiguredNeedsBuildData {
		t.Fatalf("status = %#v", status)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("calls = %#v", runner.calls)
	}
	got := strings.Join(runner.calls[1], " ")
	if !strings.Contains(got, "xcode-build-server config -scheme App -project") {
		t.Fatalf("generation argv = %q", got)
	}
}

func TestConfigureExplicitSchemeMustExist(t *testing.T) {
	root := xcodeRoot(t)
	runner := &fakeRunner{root: root, results: []ExecResult{{Stdout: "{\"project\":{\"schemes\":[\"App\"]}}"}}}
	status := Configure(context.Background(), ConfigureRequest{
		Root: root, Scheme: "Missing", Enabled: true, Trusted: true, Timeout: time.Minute,
	}, runner)
	if status.State != StateAmbiguous || len(runner.calls) != 1 {
		t.Fatalf("status = %#v, calls = %#v", status, runner.calls)
	}
}

func TestConfigurePreservesStderrOnFailure(t *testing.T) {
	root := xcodeRoot(t)
	runner := &fakeRunner{root: root, results: []ExecResult{{ExitCode: 65, Stderr: "simulator warning and failure"}}}
	status := Configure(context.Background(), ConfigureRequest{
		Root: root, Enabled: true, Trusted: true, Timeout: time.Minute,
	}, runner)
	if status.State != StateFailed || !strings.Contains(status.Detail, "simulator warning and failure") {
		t.Fatalf("status = %#v", status)
	}
}

func xcodeRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "App.xcodeproj"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}
