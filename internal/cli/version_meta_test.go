package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestServerJSONVersionMatchesVERSION guards the MCP registry metadata against
// version drift: server.json's "version" must always equal the repo's VERSION
// file. The two have drifted before (server.json sat at 0.9.7 while VERSION
// reached 0.9.16), which would publish a stale version to the registry. This
// test goes red on a VERSION bump alone, forcing the pair to move in one commit.
func TestServerJSONVersionMatchesVERSION(t *testing.T) {
	root := repoRootFromCaller(t)

	versionBytes, err := os.ReadFile(filepath.Join(root, "VERSION"))
	if err != nil {
		t.Fatalf("reading VERSION: %v", err)
	}
	version := strings.TrimSpace(string(versionBytes))
	if version == "" {
		t.Fatal("VERSION file is empty")
	}

	serverJSONBytes, err := os.ReadFile(filepath.Join(root, "server.json"))
	if err != nil {
		t.Fatalf("reading server.json: %v", err)
	}
	var meta struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(serverJSONBytes, &meta); err != nil {
		t.Fatalf("parsing server.json: %v", err)
	}

	if meta.Version != version {
		t.Fatalf("server.json version = %q, VERSION = %q; bump server.json in the same commit as VERSION", meta.Version, version)
	}
}

// repoRootFromCaller resolves the repository root from this test file's
// location (internal/cli/ → two levels up), independent of the test working
// directory.
func repoRootFromCaller(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}
