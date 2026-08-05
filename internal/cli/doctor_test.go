package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestClientConfigThatWillNotParseFails pins the one config fault every doctor
// check used to miss. A client config with broken syntax is unloadable — the
// client itself cannot read it, so plumb is not running there at all — yet the
// extractor's error was swallowed into a clean "registered" pass, and the Kimi
// tool-surface check stayed silent for the same reason. `plumb doctor` reported
// a fully healthy machine for a file no client could load.
func TestClientConfigThatWillNotParseFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	// Truncated mid-object: contains the word "plumb" (so checkOneClient gets
	// past its registered-at-all scan) and is not valid JSON.
	if err := os.WriteFile(path, []byte(`{"mcpServers": {"plumb": {"command": "/usr/local/bin/plumb"`), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	target := setupTarget{
		use: "kimi-code", name: "Kimi Code",
		pathFn:    func() (string, error) { return path, nil },
		extractFn: claudeDesktopCommandExtractor,
	}

	res := classifyClientBinary(target, path, "/usr/local/bin/plumb")
	if res.ok {
		t.Errorf("an unparseable config must fail the check, got %+v", res)
	}
	if res.fix == "" {
		t.Error("the failure must carry a fix line — the user has to know which file to repair")
	}
	if !strings.Contains(res.detail, "parse") {
		t.Errorf("detail should say the config cannot be parsed: %q", res.detail)
	}

	if got := checkOneClient(target, "/usr/local/bin/plumb"); got.ok {
		t.Errorf("checkOneClient must surface the parse failure, got %+v", got)
	}
}

func TestJsonCheckResultMarshaling(t *testing.T) {
	checks := []checkResult{
		{name: "socket", ok: true, detail: "~/.cache/plumb/plumb.sock", fix: ""},
		{name: "version", ok: false, detail: "running 0.7.0, binary is 0.7.1", fix: "run `plumb stop`"},
	}
	out := make([]jsonCheckResult, len(checks))
	for i, c := range checks {
		out[i] = jsonCheckResult{Name: c.name, OK: c.ok, Detail: c.detail, Fix: c.fix}
	}
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded []jsonCheckResult
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(decoded) != 2 {
		t.Fatalf("want 2 results, got %d", len(decoded))
	}
	if decoded[0].Name != "socket" || !decoded[0].OK {
		t.Errorf("first result: got %+v", decoded[0])
	}
	if decoded[1].Name != "version" || decoded[1].OK || decoded[1].Fix == "" {
		t.Errorf("second result: got %+v", decoded[1])
	}
}

func TestParseJavaMajorVersion(t *testing.T) {
	cases := []struct {
		line string
		want int
	}{
		// Modern format: "openjdk 21.0.3 ..."
		{`openjdk 21.0.3 2024-04-16`, 21},
		{`openjdk 17.0.1 2021-10-19`, 17},
		{`openjdk 11.0.22 2024-01-16`, 11},
		// java prefix instead of openjdk
		{`java 21.0.3 2024-04-16`, 21},
		// Legacy 1.x format: "java version "1.8.0_292""
		{`java version "1.8.0_292"`, 8},
		{`java version "1.7.0_80"`, 7},
		// Version string with surrounding quotes (some JVM output styles)
		{`openjdk version "21.0.3" 2024-04-16`, 21},
		{`openjdk version "17.0.9" 2023-10-17`, 17},
		// Distro line-1 strings (vendor appears on line 2+, major must read from line 1):
		// Eclipse Temurin, Amazon Corretto, Microsoft Build of OpenJDK.
		{`openjdk 21.0.3 2024-04-16 LTS`, 21},
		{`openjdk 17.0.11 2024-04-16 LTS`, 17},
		{`openjdk 11.0.23 2024-04-16 LTS`, 11},
		// Unrecognised / empty
		{``, 0},
		{`some random text`, 0},
		{`GraalVM CE 21.0.0`, 21},
	}
	for _, tc := range cases {
		got := parseJavaMajorVersion(tc.line)
		if got != tc.want {
			t.Errorf("parseJavaMajorVersion(%q) = %d, want %d", tc.line, got, tc.want)
		}
	}
}
